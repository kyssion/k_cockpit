package vm_test

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
	"k_cockpit/internal/task"
	"k_cockpit/internal/vm"
)

func newTestEnv(t *testing.T) (*vm.Service, *task.Queue, *gorm.DB) {
	t.Helper()
	return newTestEnvWithClient(t, agent.NewMockClient())
}

// newTestEnvWithClient 允许替换 agent 实现。
//
// 需要它的理由：mock 的探测固定返回 running，而「删除要求虚拟机不处于运行态」
// 这条分支用固定 mock 覆盖不到。测试需要可控的替身，而不是去改 mock ——
// mock 的职责是让生产代码跑通，不是让测试好写。
func newTestEnvWithClient(t *testing.T, client agent.Client) (*vm.Service, *task.Queue, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "vm.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.VM{}, &model.Task{}, &model.AuditLog{}, &model.Node{},
		&model.VMCredential{}, &model.VMInterface{}, &model.StaticIP{},
		&model.VpcSwitch{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	// 预置节点 1：多数用例以它为目标节点，逐处重复建节点只会淹没测试意图。
	// 「节点不存在」这条分支由 TestPowerRejectsMissingNode 单独覆盖。
	if err := db.Create(&model.Node{ID: 1, Name: "test-node"}).Error; err != nil {
		t.Fatalf("创建测试节点失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 2,
		PollInterval:  20 * time.Millisecond,
	})
	// 用真实的 mock agent：业务链路端到端走通，只有「执行」那一步是假的。
	queue.Register(vm.NewCreateExecutor(db, client))
	queue.Register(vm.NewPowerExecutor(db, client))
	queue.Register(vm.NewDeleteExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	// settings 传 nil：本包不依赖设置模块，阈值走内置默认值。
	return vm.NewService(db, queue, recorder, client, nil), queue, db
}

// probeClient 在 mock 之上覆盖**探测结果**，其余操作沿用 mock。
type probeClient struct {
	*agent.MockClient
	status string
}

func (c *probeClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind == agent.OpVMStatus {
		return &agent.Result{
			Success: true,
			Data:    map[string]any{agent.StatusDataKey: c.status},
		}, nil
	}
	return c.MockClient.Execute(ctx, op)
}

// unreachableClient 模拟**指令未送达**：返回 error 而不是 Success=false。
//
// 两者语义不同，调用方的处理也不同（见 agent.Client.Execute 的注释），
// 因此需要分别覆盖。
type unreachableClient struct {
	*agent.MockClient
}

func (c *unreachableClient) Execute(_ context.Context, _ agent.Operation) (*agent.Result, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func assertAPIError(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != status {
		t.Errorf("状态码 = %d, 期望 %d (%s)", apiErr.Status, status, apiErr.Message)
	}
}

// TestCreateFlowEndToEnd 验证完整异步链路：
// 接口入队 → 队列调度 → Executor 下发指令 → 写入投影 → 列表可见。
func TestCreateFlowEndToEnd(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	owner := authz.Viewer{UserID: 7}

	created, err := svc.Create(ctx, vm.CreateRequest{
		Name: "vm-web-01", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}, owner, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 接口必须立即返回任务标识，而不是等待创建完成（f-7-01 R-001）。
	if created.Status != model.TaskPending {
		t.Errorf("初始状态 = %q, 期望 pending", created.Status)
	}

	waitFor(t, "任务完成并写入投影", func() bool {
		var count int64
		db.Model(&model.VM{}).Where("name = ?", "vm-web-01").Count(&count)
		return count == 1
	})

	var got model.VM
	if err := db.Where("name = ?", "vm-web-01").First(&got).Error; err != nil {
		t.Fatalf("查询虚拟机失败: %v", err)
	}
	if got.NodeID != 1 || got.VCPU != 2 || got.MemoryMB != 2048 || got.DiskGB != 20 {
		t.Errorf("投影字段不正确: %+v", got)
	}
	if got.OwnerID == nil || *got.OwnerID != 7 {
		t.Error("归属未按当前用户写入")
	}
	// UUID 来自 agent 的返回；拿不到时应为 unknown 状态而不是猜一个 running。
	if got.UUID == nil || *got.UUID == "" {
		t.Error("未记录虚拟化层返回的 UUID")
	}
	if !strings.HasPrefix(*got.UUID, "mock-") {
		t.Errorf("UUID 形状异常: %s", *got.UUID)
	}
	if got.LastSyncedAt == nil {
		t.Error("未记录对账时间——没有它无法判断数据是否陈旧")
	}

	// 列表能看到刚创建的虚拟机。
	list, total, err := svc.List(ctx, vm.ListFilter{Viewer: owner})
	if err != nil {
		t.Fatalf("查询列表失败: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].Name != "vm-web-01" {
		t.Errorf("列表结果不正确: total=%d list=%v", total, list)
	}

	// 投影数据必须真实走通任务状态机。
	var taskRow model.Task
	db.Where("type = ?", model.TaskVMCreate).First(&taskRow)
	if taskRow.Status != model.TaskSuccess {
		t.Errorf("任务状态 = %q, 期望 success", taskRow.Status)
	}
}

func TestCreateValidatesInput(t *testing.T) {
	svc, _, _ := newTestEnv(t)
	ctx := context.Background()
	owner := authz.Viewer{UserID: 1}

	cases := map[string]vm.CreateRequest{
		"名称为空":     {Name: "", NodeID: 1, VCPU: 1, MemoryMB: 512, DiskGB: 10},
		"名称含空格":    {Name: "my vm", NodeID: 1, VCPU: 1, MemoryMB: 512, DiskGB: 10},
		"名称以连字符开头": {Name: "-bad", NodeID: 1, VCPU: 1, MemoryMB: 512, DiskGB: 10},
		"未指定节点":    {Name: "vm1", NodeID: 0, VCPU: 1, MemoryMB: 512, DiskGB: 10},
		"CPU 为 0":  {Name: "vm1", NodeID: 1, VCPU: 0, MemoryMB: 512, DiskGB: 10},
		"内存为负":     {Name: "vm1", NodeID: 1, VCPU: 1, MemoryMB: -1, DiskGB: 10},
	}

	for name, req := range cases {
		_, err := svc.Create(ctx, req, owner, "alice", "")
		if err == nil {
			t.Errorf("%s: 未被拒绝", name)
			continue
		}
		assertAPIError(t, err, 400)
	}
}

// 同名同节点的重复提交必须只产生一个任务（幂等键由业务语义构成）。
func TestCreateIsIdempotent(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	owner := authz.Viewer{UserID: 1}

	req := vm.CreateRequest{Name: "vm-dup", NodeID: 1, VCPU: 1, MemoryMB: 512, DiskGB: 10}

	first, err := svc.Create(ctx, req, owner, "alice", "")
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	second, err := svc.Create(ctx, req, owner, "alice", "")
	if err != nil {
		t.Fatalf("重复创建失败: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("重复提交产生了不同任务: %d vs %d", first.ID, second.ID)
	}

	var count int64
	db.Model(&model.Task{}).Count(&count)
	if count != 1 {
		t.Errorf("任务数 = %d, 期望 1", count)
	}
}

// 归属过滤：tenant 看不到他人虚拟机，且越权访问返回 404。
func TestOwnershipFiltering(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	// 直接写投影（跳过创建流程，专注验证读取侧的过滤）。
	mine := model.VM{NodeID: 1, Name: "mine", Status: model.VMStatusRunning, OwnerID: ptr(int64(10))}
	theirs := model.VM{NodeID: 1, Name: "theirs", Status: model.VMStatusRunning, OwnerID: ptr(int64(20))}
	db.Create(&mine)
	db.Create(&theirs)

	tenant := authz.Viewer{UserID: 10}
	list, total, err := svc.List(ctx, vm.ListFilter{Viewer: tenant})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 1 || len(list) != 1 || list[0].Name != "mine" {
		t.Errorf("tenant 看到了不该看到的虚拟机: total=%d list=%v", total, list)
	}

	_, err = svc.Get(ctx, theirs.ID, tenant)
	assertAPIError(t, err, 404)

	admin := authz.Viewer{UserID: 99, IsAdmin: true}
	_, adminTotal, _ := svc.List(ctx, vm.ListFilter{Viewer: admin})
	if adminTotal != 2 {
		t.Errorf("管理员应看到全部: total=%d", adminTotal)
	}
}

// 状态未知不代表离线：投影字段是 agent 上报的结果，控制面不应替它判断。
func TestStaleProjectionIsFlagged(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	old := time.Now().Add(-10 * vm.StaleThreshold)
	fresh := model.VM{NodeID: 1, Name: "fresh", Status: model.VMStatusRunning, LastSyncedAt: &old}
	db.Create(&fresh)
	now := time.Now()
	stale := model.VM{NodeID: 1, Name: "stale", Status: model.VMStatusRunning, LastSyncedAt: &now}
	db.Create(&stale)

	list, _, err := svc.List(ctx, vm.ListFilter{Viewer: authz.Viewer{IsAdmin: true}})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	flags := map[string]bool{}
	for _, v := range list {
		flags[v.Name] = v.Stale
	}
	if !flags["fresh"] {
		t.Error("长时间未对账的记录未标记为陈旧——把陈旧数据显示成当前状态会误导排障")
	}
	if flags["stale"] {
		t.Error("刚刚对账过的记录被误标为陈旧")
	}
}

// 名称关键词中的 LIKE 通配符必须被转义，否则输入 % 会匹配到全部记录。
func TestKeywordEscapesWildcards(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	db.Create(&model.VM{NodeID: 1, Name: "web-01", Status: model.VMStatusRunning})
	db.Create(&model.VM{NodeID: 1, Name: "db-01", Status: model.VMStatusRunning})

	_, total, err := svc.List(ctx, vm.ListFilter{
		Keyword: "%",
		Viewer:  authz.Viewer{IsAdmin: true},
	})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 0 {
		t.Errorf("通配符未被转义，匹配到了 %d 条记录", total)
	}
}

// --- 电源操作 ---

func TestPowerJudgesByLiveProbeNotProjection(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	// 投影写的是 stopped，但探测返回 running。
	//
	// 这个不一致正是本测试要表达的核心：判定必须基于**实时探测**（f-2-01
	// R-002）。若凭投影放行，就可能在虚拟机实际运行时执行只该对关机状态
	// 做的操作——而投影滞后恰恰是最常见的场景。
	row := model.VM{NodeID: 1, Name: "vm-probe", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Power(ctx, row.ID, "start", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	// 拒绝文案必须说明**当前真实状态**：只说「操作不被允许」会让用户以为
	// 是权限问题，而真正的原因是状态不匹配。
	var apiErr *api.Error
	if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "运行中") {
		t.Errorf("拒绝原因未说明当前状态: %q", apiErr.Message)
	}
}

func TestPowerEnqueuesAndUpdatesProjection(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-power", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	tk, err := svc.Power(ctx, row.ID, "shutdown", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if tk.Type != model.TaskVMPower {
		t.Errorf("任务类型 = %q, 期望 %q", tk.Type, model.TaskVMPower)
	}
	// 资源锁键必须是 vm:<id>：否则同一台虚拟机的并发电源操作不会串行，
	// 可能交错执行出错误的状态（f-2-01 R-005）。
	if rt, rid := tk.TaskResource(); rt != "vm" || rid != row.ID {
		t.Errorf("资源锁键 = %s:%d, 期望 vm:%d", rt, rid, row.ID)
	}

	waitFor(t, "投影状态更新为已关机", func() bool {
		var got model.VM
		db.First(&got, row.ID)
		return got.Status == model.VMStatusStopped
	})
}

func TestPowerRejectsUnknownAction(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-bad", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Power(ctx, row.ID, "explode", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestPowerRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-theirs", Status: model.VMStatusRunning, OwnerID: ptr(int64(20))}
	db.Create(&theirs)

	// 404 而非 403：403 会告诉对方「这个 ID 确实存在」，可被用来枚举资源。
	_, err := svc.Power(ctx, theirs.ID, "shutdown", authz.Viewer{UserID: 10}, "alice", "10.0.0.1")
	assertAPIError(t, err, 404)
}

func TestPowerBlockedInMaintenanceMode(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	db.Create(&model.Node{ID: 5, Name: "maintenance-node", MaintenanceMode: true})
	row := model.VM{NodeID: 5, Name: "vm-maint", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Power(ctx, row.ID, "shutdown", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestPowerUnavailableWhenAgentUnreachable(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &unreachableClient{agent.NewMockClient()})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-offline", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 探测不到状态时拒绝，而不是放行：猜错的方向可能是对运行中的虚拟机断电。
	_, err := svc.Power(ctx, row.ID, "shutdown", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 503)
}

// --- 删除 ---

func TestDeleteRequiresExplicitDiskAction(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-del", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 空值与非法值都必须拒绝：服务端**不替用户选默认值**（R-009）——
	// 默认连盘删除的误操作代价是数据永久丢失。
	for _, action := range []string{"", "drop", "Delete"} {
		_, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: action},
			authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		assertAPIError(t, err, 400)
	}
}

func TestDeleteRejectedWhileRunning(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-running", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 对运行中的虚拟机执行删除会强杀来宾进程并删除磁盘，代价不可逆。
	_, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestDeleteMarksNotPresentInsteadOfRemoving(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-to-delete", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	if _, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionDelete},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "记录被标记为已不在虚拟化层", func() bool {
		var got model.VM
		if err := db.First(&got, row.ID).Error; err != nil {
			return false
		}
		return !got.Present
	})

	// 记录必须**保留**：审计与历史任务都引用它，物理删除会让这些引用悬空。
	var count int64
	db.Model(&model.VM{}).Where("id = ?", row.ID).Count(&count)
	if count != 1 {
		t.Errorf("记录被物理删除了, 期望保留并标记 present=false")
	}

	// 列表按 present 过滤，删除后不再出现。
	list, total, err := svc.List(ctx, vm.ListFilter{Viewer: authz.Viewer{UserID: 7}})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 0 || len(list) != 0 {
		t.Errorf("已删除的虚拟机仍出现在列表中: total=%d", total)
	}
}

func TestDeleteRejectedWhenTaskInFlight(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-busy", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	running := model.Task{
		Type: model.TaskVMPower, Status: model.TaskRunning,
		ResourceType: ptr("vm"), ResourceID: ptr(row.ID),
	}
	db.Create(&running)

	// 删除排在在途任务后面执行，意味着「用户以为取消了的操作其实照样做了」，
	// 因此这里拒绝而不是排队。
	_, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)
}

func TestPowerRejectsMissingNode(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	// 节点在控制面之外被移除了：此时不能放行操作，否则指令会发往一个
	// 已不存在的目标。
	row := model.VM{NodeID: 999, Name: "vm-orphan", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Power(ctx, row.ID, "shutdown", authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// --- 网络 ---

func TestInterfacesSortedByOrderNotInsertion(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-nic", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)
	db.Create(&model.VpcSwitch{ID: 9, NodeID: 1, Name: "vpc-a", BridgeName: "br9"})

	// **故意乱序插入**：网卡顺序决定了它在来宾系统里是 eth0 还是 eth1，
	// 按插入顺序或 id 返回都会让用户对着与实际相反的编号做配置。
	db.Create(&model.VMInterface{
		VMID: row.ID, NodeID: 1, Order: 1, Model: model.NICModelE1000,
	})
	db.Create(&model.VMInterface{
		VMID: row.ID, NodeID: 1, Order: 0, Model: model.NICModelVirtio,
		IsPrimary: true, SwitchID: ptr(int64(9)),
	})

	nics, err := svc.Interfaces(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询网卡失败: %v", err)
	}
	if len(nics) != 2 {
		t.Fatalf("网卡数 = %d, 期望 2", len(nics))
	}
	if nics[0].Order != 0 || nics[1].Order != 1 {
		t.Errorf("网卡未按 order 排序: %d, %d", nics[0].Order, nics[1].Order)
	}
	if !nics[0].IsPrimary {
		t.Error("主网卡标记丢失")
	}
	// 交换机名要带上：只给一个 switch_id，用户还得自己去网络页面对照名字。
	if nics[0].SwitchName == nil || *nics[0].SwitchName != "vpc-a" {
		t.Errorf("交换机名未解析: %v", nics[0].SwitchName)
	}
	// 从未下发过（LastAppliedAt 为空）时必须是「尚未生效」。
	// 把它报成已生效，会让用户以为改配置没反应是别的原因。
	if nics[0].Applied {
		t.Error("从未下发的网卡被报成已生效")
	}
}

func TestInterfacesRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-theirs-nic", OwnerID: ptr(int64(20))}
	db.Create(&theirs)
	db.Create(&model.VMInterface{VMID: theirs.ID, NodeID: 1, Order: 0})

	// 404 而非 403：403 会确认「这个 ID 存在」，可被用来枚举他人资源。
	_, err := svc.Interfaces(ctx, theirs.ID, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)
}

func TestStaticIPsOnlyReturnsBoundOnes(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ip", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	db.Create(&model.StaticIP{NodeID: 1, VMID: &row.ID, IP: "10.0.0.20", AddressFamily: model.AddressFamilyIPv4})
	// 未绑定（vm_id 为空）的地址不属于任何虚拟机：把它显示在这里会让用户
	// 以为虚拟机已经拿到了那个 IP。
	db.Create(&model.StaticIP{NodeID: 1, VMID: nil, IP: "10.0.0.99", AddressFamily: model.AddressFamilyIPv4})

	ips, err := svc.StaticIPs(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询静态地址失败: %v", err)
	}
	if len(ips) != 1 || ips[0].IP != "10.0.0.20" {
		t.Errorf("静态地址 = %+v, 期望只含 10.0.0.20", ips)
	}
}

func ptr[T any](v T) *T { return &v }
