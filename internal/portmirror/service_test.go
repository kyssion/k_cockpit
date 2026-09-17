package portmirror_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/portmirror"
)

func newTestEnv(t *testing.T) (*portmirror.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "mirror.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.PortMirror{}, &model.Node{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return portmirror.NewService(db, agent.NewMockClient(), audit.NewRecorder(db)), db
}

func admin() authz.Viewer { return authz.Viewer{UserID: 1, IsAdmin: true} }

func seedMirror(t *testing.T, svc *portmirror.Service, name string, sources, targets []string, dir string) *portmirror.View {
	t.Helper()
	view, err := svc.Create(context.Background(), portmirror.Request{
		NodeID: 1, Name: name, SourcePorts: sources,
		TargetSwitches: targets, Direction: dir,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("创建镜像 %s 失败: %v", name, err)
	}
	return view
}

// TestEnableAlwaysArmsWatchdog 覆盖本功能的核心。
//
// 看门狗**不能设为「无」**：一个没有兜底的端口镜像变更，配错时唯一的恢复
// 途径是上宿主机敲命令——而那正是这个功能要避免的事情。
func TestEnableAlwaysArmsWatchdog(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	m := seedMirror(t, svc, "m1", []string{"tap0"}, []string{"mirror-sw"}, model.MirrorIngress)

	// 传 0 也要拿到一个看门狗，而不是「不设」。
	result, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
		WatchdogSeconds: 0, Acknowledge: true,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if !result.Applied {
		t.Fatal("应已启用")
	}
	if result.WatchdogSeconds <= 0 {
		t.Error("看门狗时长必须为正——没有兜底的镜像变更不该被允许")
	}
	if result.Mirror == nil || !result.Mirror.AwaitingConfirm {
		t.Error("启用后应处于「等待确认」窗口里")
	}
	if result.Mirror.WatchdogSecondsLeft <= 0 {
		t.Error("剩余秒数应由服务端算好下发")
	}
}

func TestWatchdogWindowBounds(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	for _, seconds := range []int{10, 59, 1801, 99999} {
		m := seedMirror(t, svc, "m-"+string(rune('a'+seconds%26)),
			[]string{"tap-x"}, []string{"sw-x"}, model.MirrorBoth)
		_, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
			WatchdogSeconds: seconds, Acknowledge: true,
		}, admin(), "root", "")
		assertStatus(t, err, 400)
	}
}

// TestRiskyWarningsBlockEnable 覆盖「风险必须被确认」。
func TestRiskyWarningsBlockEnable(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	first := seedMirror(t, svc, "first", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)
	if _, err := svc.Enable(ctx, first.ID, portmirror.EnableRequest{
		Acknowledge: true,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("首次启用失败: %v", err)
	}

	// 第二条复用了第一条的来源接口 → 那个口的流量会被复制两份。
	second := seedMirror(t, svc, "second", []string{"tap0"}, []string{"sw-b"}, model.MirrorIngress)
	result, err := svc.Enable(ctx, second.ID, portmirror.EnableRequest{
		Acknowledge: false,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("未确认时应返回结果而不是报错——那是一个岔路口，不是一次失败: %v", err)
	}
	if result.Applied {
		t.Error("有未确认的风险警告时不该下发")
	}
	if len(result.Warnings) == 0 {
		t.Error("应把警告原样返回给调用方")
	}

	// 确认后可以启用。
	result, err = svc.Enable(ctx, second.ID, portmirror.EnableRequest{
		Acknowledge: true,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("确认后启用失败: %v", err)
	}
	if !result.Applied {
		t.Error("确认后应下发")
	}
}

// TestBackgroundNoteDoesNotBlockEnable 覆盖一处会让确认框失去意义的细节。
//
// 预检输出混了两类东西：真正的风险，以及**背景说明**（预检能力的边界）。
// 后者不该拦住一次启用——把它也算成「需要确认」会让用户习惯性地点过确认
// 框，而那时真正的风险提示也就失去了作用。
func TestBackgroundNoteDoesNotBlockEnable(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	m := seedMirror(t, svc, "solo", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)

	warnings, err := svc.Precheck(ctx, m.ID)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	// 单条规则、单向、无重叠时，只应有那一句背景说明。
	if len(warnings) == 0 {
		t.Fatal("预检应始终说明自己的能力边界")
	}
	if !strings.Contains(strings.Join(warnings, " | "), "预检只看控制面记录") {
		t.Errorf("背景说明缺失: %v", warnings)
	}

	// 不确认也能启用——因为没有真正的风险。
	result, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
		Acknowledge: false,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	if !result.Applied {
		t.Error("只有背景说明时不该拦住启用——否则确认框会变成习惯性点击")
	}
}

// TestConfirmClearsWatchdog 覆盖「确认保持」。
func TestConfirmClearsWatchdog(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	m := seedMirror(t, svc, "c1", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)

	if _, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
		Acknowledge: true,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	view, err := svc.Confirm(ctx, m.ID, admin(), "root", "")
	if err != nil {
		t.Fatalf("确认失败: %v", err)
	}
	if view.AwaitingConfirm {
		t.Error("确认后不该还在等待窗口里")
	}
	if !view.Enabled {
		t.Error("确认后镜像应保持生效")
	}

	var row model.PortMirror
	db.Where("id = ?", m.ID).First(&row)
	if row.WatchdogUntil != nil {
		t.Error("数据库里的看门狗时刻未被清掉")
	}

	// 重复确认应给出明确提示，而不是静默成功。
	if _, err := svc.Confirm(ctx, m.ID, admin(), "root", ""); err == nil {
		t.Error("没有窗口时确认应被拒绝")
	}
}

// TestReconcileAlignsExpiredWatchdogs 覆盖控制面与节点的分工。
//
// 回滚的**执行者**是节点；这里只让控制面的记录追上节点已经做完的事。
// 两者必须分开：如果只有控制面这一侧扫，那么在一次面板停机或网络中断
// 期间启用的镜像就永远不会被撤销。
func TestReconcileAlignsExpiredWatchdogs(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	m := seedMirror(t, svc, "exp", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)
	if _, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
		Acknowledge: true,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	// 把看门狗时刻挪到过去，模拟节点已经自行撤销。
	if err := db.Model(&model.PortMirror{}).Where("id = ?", m.ID).
		Update("watchdog_until", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("改看门狗时刻失败: %v", err)
	}

	n, err := svc.Reconcile(ctx)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if n != 1 {
		t.Errorf("对齐了 %d 条, 期望 1", n)
	}

	items, _ := svc.List(ctx, 1)
	if items[0].Enabled {
		t.Error("看门狗到期后应标记为已关闭")
	}
}

// TestEnabledMirrorCannotBeEdited 覆盖一处会绕过看门狗的路径。
//
// 直接改一条生效中的镜像等于在一次操作里同时撤掉旧的、建起新的，而新的
// 那一段没有被看门狗保护。
func TestEnabledMirrorCannotBeEdited(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	m := seedMirror(t, svc, "e1", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)
	if _, err := svc.Enable(ctx, m.ID, portmirror.EnableRequest{
		Acknowledge: true,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	_, err := svc.Update(ctx, m.ID, portmirror.Request{
		Name: "changed", SourcePorts: []string{"tap1"}, TargetSwitches: []string{"sw-b"},
	}, admin(), "root", "")
	assertStatus(t, err, 422)
	if !strings.Contains(err.Error(), "先关闭") {
		t.Errorf("拒绝文案应说明该怎么办: %v", err)
	}

	if err := svc.Delete(ctx, m.ID, admin(), "root", ""); err == nil {
		t.Error("生效中的镜像不应可删除")
	}
}

func TestDuplicateSourcesRejected(t *testing.T) {
	svc, _ := newTestEnv(t)

	// 同一个口的流量被复制两份是**静默的流量放大**，而它在抓包里表现为
	// 「每个包都出现两次」，很容易被误判成对端在重传。
	_, err := svc.Create(context.Background(), portmirror.Request{
		NodeID: 1, SourcePorts: []string{"tap0", "tap0"},
		TargetSwitches: []string{"sw-a"},
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

func TestSourceCannotBeTarget(t *testing.T) {
	svc, _ := newTestEnv(t)

	// 同一个对象既当来源又当目标：流量会被复制回它自己。
	_, err := svc.Create(context.Background(), portmirror.Request{
		NodeID: 1, SourcePorts: []string{"same"},
		TargetSwitches: []string{"same"},
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

func TestRequiresBothSides(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, portmirror.Request{
		NodeID: 1, TargetSwitches: []string{"sw-a"},
	}, admin(), "root", "")
	assertStatus(t, err, 400)

	// 没有目标时镜像出去的流量没地方接，会堆在宿主机的发送队列里。
	_, err = svc.Create(ctx, portmirror.Request{
		NodeID: 1, SourcePorts: []string{"tap0"},
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

// TestDisableIsIdempotent 覆盖关闭的幂等性。
//
// 关闭是一个「我要它停下来」的动作，重复点不该报错——尤其在网络已经
// 出问题的时候，用户会连点好几次。
func TestDisableIsIdempotent(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	m := seedMirror(t, svc, "d1", []string{"tap0"}, []string{"sw-a"}, model.MirrorIngress)

	for i := 0; i < 3; i++ {
		view, err := svc.Disable(ctx, m.ID, admin(), "root", "")
		if err != nil {
			t.Fatalf("第 %d 次关闭失败: %v", i+1, err)
		}
		if view.Enabled {
			t.Error("关闭后仍显示为启用")
		}
	}
}

func TestCreateRequiresEnrolledNode(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.Create(context.Background(), portmirror.Request{
		NodeID: 999, SourcePorts: []string{"tap0"}, TargetSwitches: []string{"sw-a"},
	}, admin(), "root", "")
	assertStatus(t, err, 404)
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}
