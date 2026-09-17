package vm_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
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
		&model.VM{}, &model.Task{}, &model.TaskStage{}, &model.AuditLog{}, &model.Node{},
		&model.VMCredential{}, &model.VMInterface{}, &model.StaticIP{},
		&model.VpcSwitch{}, &model.VMSnapshot{}, &model.SystemSetting{},
		&model.PortForward{}, &model.VMLock{}, &model.Template{},
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
	queue.Register(vm.NewSnapshotCreateExecutor(db, client))
	queue.Register(vm.NewSnapshotRestoreExecutor(db, client))
	queue.Register(vm.NewSnapshotDeleteExecutor(db, client))
	queue.Register(vm.NewConfigUpdateExecutor(db, client))
	queue.Register(vm.NewInterfaceChangeExecutor(db, client))
	queue.Register(vm.NewStaticIPChangeExecutor(db, client))
	queue.Register(vm.NewPortForwardChangeExecutor(db, client))
	queue.Register(vm.NewEnterRescueExecutor(db, client))
	queue.Register(vm.NewExitRescueExecutor(db, client))

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

// --- 快照 ---

func TestSnapshotListComputesCapabilities(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-snap", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	db.Create(&model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "s-ok",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady,
	})
	db.Create(&model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "s-child",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady, HasChildren: true,
	})
	db.Create(&model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "s-current",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady, IsCurrent: true,
	})
	db.Create(&model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "s-broken",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotError,
	})

	list, err := svc.Snapshots(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("查询快照失败: %v", err)
	}
	if list.Used != 4 {
		t.Errorf("已用配额 = %d, 期望 4", list.Used)
	}
	if list.Quota <= 0 {
		t.Error("未返回配额上限，界面无法显示「已用/上限」")
	}

	byName := map[string]vm.SnapshotView{}
	for _, s := range list.Items {
		byName[s.Name] = s
	}

	// 可删可恢复的正常快照。
	if !byName["s-ok"].CanDelete || !byName["s-ok"].CanRestore {
		t.Error("就绪且无子快照的普通快照应可删可恢复")
	}
	// 有子快照：删掉它会让子快照失去依赖。
	if byName["s-child"].CanDelete {
		t.Error("有子快照的快照不应可删")
	}
	// 当前快照：虚拟机正运行在它上面。
	if byName["s-current"].CanDelete || byName["s-current"].CanRestore {
		t.Error("当前快照既不应当可删，也不应当可恢复（恢复它没有意义）")
	}
	// 失败的快照不能用于恢复——它不可信。
	if byName["s-broken"].CanRestore {
		t.Error("创建失败的快照不应可恢复")
	}
}

func TestSnapshotCreateEnqueuesAndReservesQuota(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-snap2", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	tk, err := svc.CreateSnapshot(ctx, row.ID, vm.CreateSnapshotRequest{
		Name: "before-upgrade",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if tk.Type != model.TaskVMSnapshotCreate {
		t.Errorf("任务类型 = %q, 期望 %q", tk.Type, model.TaskVMSnapshotCreate)
	}
	// 资源锁键必须是 vm:<id>：这样快照操作与电源操作天然互斥，
	// 恢复快照时不会有并发的开机请求插进来。
	if rt, rid := tk.TaskResource(); rt != "vm" || rid != row.ID {
		t.Errorf("资源锁键 = %s:%d, 期望 vm:%d", rt, rid, row.ID)
	}

	// **记录先于执行存在**：执行器需要它的 ID 才能把结果写回来。
	var count int64
	db.Model(&model.VMSnapshot{}).Where("vm_id = ?", row.ID).Count(&count)
	if count != 1 {
		t.Errorf("快照记录数 = %d, 期望 1（创建期间就必须有一条可被引用的记录）", count)
	}
}

func TestSnapshotCreateRejectsDuplicateName(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-dup-snap", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	db.Create(&model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "daily",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady,
	})

	// 同名冲突给出 409 而不是 500：这是一个用户可以自己解决的问题。
	_, err := svc.CreateSnapshot(ctx, row.ID, vm.CreateSnapshotRequest{Name: "daily"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)
}

func TestSnapshotCreateRejectsOverQuota(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-quota", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 占满配额。
	for i := 0; i < 10; i++ {
		db.Create(&model.VMSnapshot{
			VMID: row.ID, NodeID: 1, Name: "s" + strconv.Itoa(i),
			Kind: model.SnapshotKindInternal, Status: model.SnapshotReady,
		})
	}

	// 配额在**创建之前**检查：先创建再发现超限，会留下一个需要回滚的快照，
	// 而回滚本身也可能失败。
	_, err := svc.CreateSnapshot(ctx, row.ID, vm.CreateSnapshotRequest{Name: "one-more"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)
}

func TestSnapshotDeleteReasonsAreSpecific(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-del-snap", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	withChild := model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "with-child",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady, HasChildren: true,
	}
	db.Create(&withChild)

	current := model.VMSnapshot{
		VMID: row.ID, NodeID: 1, Name: "current",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady, IsCurrent: true,
	}
	db.Create(&current)

	// 拒绝理由必须**具体**：「有子快照」与「是当前快照」需要用户做的事
	// 完全不同，笼统的「不能删除」会让他无从下手。
	_, err := svc.DeleteSnapshot(ctx, row.ID, withChild.ID,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
	var apiErr *api.Error
	if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "子快照") {
		t.Errorf("拒绝文案未说明原因: %q", apiErr.Message)
	}

	_, err = svc.DeleteSnapshot(ctx, row.ID, current.ID,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
	if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "当前") {
		t.Errorf("拒绝文案未说明原因: %q", apiErr.Message)
	}
}

func TestSnapshotRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-theirs-snap", OwnerID: ptr(int64(20))}
	db.Create(&theirs)
	snap := model.VMSnapshot{
		VMID: theirs.ID, NodeID: 1, Name: "s",
		Kind: model.SnapshotKindInternal, Status: model.SnapshotReady,
	}
	db.Create(&snap)

	// 404 而非 403：403 会确认「这个 ID 存在」，可被用来枚举他人资源。
	_, err := svc.Snapshots(ctx, theirs.ID, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)

	// 删除也要走归属校验，而不是只查快照 ID——后者会让知道 ID 的人
	// 直接删掉别人的快照。
	_, err = svc.DeleteSnapshot(ctx, theirs.ID, snap.ID,
		authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}

// --- 编辑配置 ---

func TestEditFormCarriesTheMatrix(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-edit", VCPU: 2, MemoryMB: 2048, OwnerID: ptr(int64(7))}
	db.Create(&row)

	form, err := svc.EditFormOf(ctx, row.ID, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取编辑表单失败: %v", err)
	}

	byKey := map[string]vm.EditField{}
	for _, f := range form.Fields {
		byKey[f.Key] = f
	}

	// 纯控制面元数据**不需要下发**，因此也**不受运行态限制**——
	// 虚拟机开着也能改备注，这一点必须在矩阵里体现出来。
	for _, key := range []string{"remark", "group_name"} {
		f, ok := byKey[key]
		if !ok {
			t.Fatalf("矩阵缺少 %s", key)
		}
		if f.RequiresNode {
			t.Errorf("%s 是纯控制面元数据，不应要求下发到节点", key)
		}
		if f.RequiresShutdown {
			t.Errorf("%s 不应要求关机", key)
		}
	}

	// 硬件配置需要下发且需要关机。
	for _, key := range []string{"vcpu", "memory_mb"} {
		f := byKey[key]
		if !f.RequiresNode || !f.RequiresShutdown {
			t.Errorf("%s 应标记为需下发且需关机（实际 %+v）", key, f)
		}
	}

	// Guest Agent 是**探测结果**，必须标为只读——把它做成可编辑的输入框
	// 会让人以为「勾上它就能让 Guest Agent 跑起来」。
	if f := byKey["guest_agent"]; !f.ReadOnly {
		t.Error("guest_agent 是节点上报的状态，应标为只读")
	}

	// 子选项卡要带上：界面按它渲染，新增一个只改后端一处。
	if len(form.Groups) == 0 {
		t.Error("未下发子选项卡定义，界面只能硬编码——那正是矩阵要消灭的东西")
	}

	// 枚举字段必须带可选值：没有它，界面只能渲染成自由文本输入框，
	// 而用户可以填任何东西进去。
	for _, key := range []string{"firmware", "machine_type", "watchdog"} {
		f := byKey[key]
		if f.Kind != vm.EditKindSelect || len(f.Options) == 0 {
			t.Errorf("%s 应是带可选值的枚举字段（实际 kind=%s options=%d）",
				key, f.Kind, len(f.Options))
		}
	}

	// mock 探测固定返回 running，因此当前不可提交需要关机的改动。
	if form.EditableNow {
		t.Error("运行态下 EditableNow 应为 false")
	}
	if form.CurrentStatus != model.VMStatusRunning {
		t.Errorf("当前状态 = %q, 期望 running", form.CurrentStatus)
	}
}

func TestUpdateMetadataWorksWhileRunning(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-meta", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 运行中也能改：这是纯控制面数据，没有「运行中不能改」这回事。
	view, err := svc.UpdateMetadata(ctx, row.ID, vm.UpdateMetadataRequest{
		Remark: ptr("生产环境主库"), GroupName: ptr("prod"),
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("修改元数据失败: %v", err)
	}
	if view.Remark != "生产环境主库" || view.GroupName != "prod" {
		t.Errorf("元数据未生效: remark=%q group=%q", view.Remark, view.GroupName)
	}
}

func TestUpdateMetadataCanClearField(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-clear", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	if _, err := svc.UpdateMetadata(ctx, row.ID, vm.UpdateMetadataRequest{
		Remark: ptr("临时"),
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("首次修改失败: %v", err)
	}

	// 提交空字符串 = 用户明确要清空。这与「没提交这一项」是两件事，
	// 因此请求用指针区分（见 UpdateMetadataRequest 的说明）。
	view, err := svc.UpdateMetadata(ctx, row.ID, vm.UpdateMetadataRequest{
		Remark: ptr(""),
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("清空备注失败: %v", err)
	}
	if view.Remark != "" {
		t.Errorf("备注未被清空: %q", view.Remark)
	}
}

func TestUpdateConfigRejectsWhileRunning(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-cfg", VCPU: 2, MemoryMB: 2048,
		Status: model.VMStatusStopped, OwnerID: ptr(int64(7)),
	}
	db.Create(&row)

	// 探测（mock）返回 running，所以需要关机的改动必须被拒绝——
	// 注意投影写的是 stopped，判定必须基于实时探测（f-2-01 R-002）。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"vcpu": 4},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	var apiErr *api.Error
	if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "关机") {
		t.Errorf("拒绝文案未说明需要关机: %q", apiErr.Message)
	}
}

func TestUpdateConfigRejectsNoopChange(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-noop", VCPU: 2, MemoryMB: 2048, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 等值提交要拒绝，而不是照样入队：它会白白触发一次节点往返，
	// 在更复杂的场景下（需要重启的项）还会引发一次没有理由的重启。
	//
	// 注意这里传的是**字符串** "2"——界面上的输入框提交的就是字符串，
	// 而库里存的是 int。值比较必须跨过这一层转换，否则每个没动过的输入框
	// 都会被认为「变了」。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"vcpu": "2"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestUpdateConfigValidatesRange(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-range", VCPU: 2, MemoryMB: 2048, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 范围来自矩阵，与界面上的控件约束同源。
	for _, v := range []int{0, -1, 999} {
		_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
			Changes: map[string]any{"vcpu": v},
		}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("CPU=%d 应被拒绝", v)
		}
	}
}

func TestUpdateConfigEnqueuesWhenStopped(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-cfg2", VCPU: 2, MemoryMB: 2048,
		Status: model.VMStatusStopped, OwnerID: ptr(int64(7)),
	}
	db.Create(&row)

	tk, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		// 混合类型：数字、字符串、布尔都走同一条转换路径。界面上的输入框
		// 提交字符串、开关提交布尔、而 JSON 反序列化后的数字是 float64。
		Changes: map[string]any{"vcpu": 8, "memory_mb": "8192"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("关机态下应受理: %v", err)
	}
	if tk.Type != model.TaskVMConfigUpdate {
		t.Errorf("任务类型 = %q, 期望 %q", tk.Type, model.TaskVMConfigUpdate)
	}
	// 与电源操作共用资源锁键，因此不会出现「边改配置边开机」。
	if rt, rid := tk.TaskResource(); rt != "vm" || rid != row.ID {
		t.Errorf("资源锁键 = %s:%d, 期望 vm:%d", rt, rid, row.ID)
	}
}

func TestEditFormRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-theirs-edit", OwnerID: ptr(int64(20))}
	db.Create(&theirs)

	_, err := svc.EditFormOf(ctx, theirs.ID, authz.Viewer{UserID: 10})
	assertAPIError(t, err, 404)

	_, err = svc.UpdateMetadata(ctx, theirs.ID, vm.UpdateMetadataRequest{Remark: ptr("x")},
		authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}

func TestUpdateConfigRejectsUnknownField(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-unknown", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 不在矩阵里的字段必须**拒绝**而不是静默丢弃。
	// 静默丢弃的表现是「点了保存、提示成功、但配置没变」——用户会以为
	// 是节点没生效，而实际上请求根本没被受理。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"disk_gb": 100},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestUpdateConfigRejectsReadOnlyField(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ro", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// guest_agent 是节点上报的探测结果。允许修改会让用户以为
	// 「勾上它就能让 Guest Agent 跑起来」——而它取决于来宾里装没装。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"guest_agent": true},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestUpdateConfigRejectsInvalidEnum(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-enum", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 枚举值必须落在矩阵声明的范围内。放行未知值会让它一路传到节点，
	// 而节点报的错通常是一句看不懂的参数错误。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"firmware": "coreboot"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)

	// 合法值应通过。
	if _, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"firmware": "uefi"},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Errorf("合法枚举值被拒绝: %v", err)
	}
}

// TestHotChangeableFieldWorksWhileRunning 覆盖矩阵里**真正的「可热改」**项。
//
// 自启开关不需要关机——它只影响下次宿主机启动时的行为。这类项如果被误标为
// 需关机，用户会白白停机一次；反过来如果该关机的项没标，则会在运行中改出
// 一个不一致的配置。矩阵里每一项的这两条标记都需要有意为之。
func TestHotChangeableFieldWorksWhileRunning(t *testing.T) {
	svc, _, db := newTestEnv(t) // mock 探测固定返回 running
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-hot", OwnerID: ptr(int64(7))}
	db.Create(&row)

	tk, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"auto_start": true},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("运行态下应可修改「随宿主机自启」: %v", err)
	}
	if tk.Type != model.TaskVMConfigUpdate {
		t.Errorf("任务类型 = %q", tk.Type)
	}
}

func TestIOPSLimitsAreMutuallyExclusive(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-iops", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 一次提交里同时给总量与读写分离：必须拒绝。
	_, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"disk_iops_total": 1000, "disk_iops_read": 500},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	// **校验的是变更后的最终状态**，而不是本次提交的字段：
	// 先设总量（成功），再设读限值（应被拒）——只检查本次提交会漏掉
	// 这种「两次操作叠加出互斥状态」的情况。
	if _, err := svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"disk_iops_total": 1000},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("设置总量失败: %v", err)
	}

	// 模拟第一次任务已执行完。测试环境里队列未启动，任务停在 pending，
	// 投影不会自动更新。
	//
	// 这里顺带说明一个**真实存在的窗口**：校验读的是投影，因此在「提交成功」
	// 到「任务执行完回写投影」之间再提交一次，两次操作叠加出的互斥状态不会被
	// 拦住。窗口的宽度是一次任务执行的时间，而这个窗口无法用投影消除——
	// 彻底的解法是在执行器里也校验一次，但那属于「节点侧的前置条件」，
	// 与这里的分工不同。
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Update("disk_iops_total", 1000).Error; err != nil {
		t.Fatalf("更新投影失败: %v", err)
	}

	_, err = svc.UpdateConfig(ctx, row.ID, vm.ConfigChangeRequest{
		Changes: map[string]any{"disk_iops_read": 500},
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// --- 网络管理的写操作 ---

func TestAddInterfaceAssignsNextOrder(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-nic-add", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 已有 order 0 与 2（中间那块被删过）。
	db.Create(&model.VMInterface{VMID: row.ID, NodeID: 1, Order: 0, IsPrimary: true, Model: model.NICModelVirtio})
	db.Create(&model.VMInterface{VMID: row.ID, NodeID: 1, Order: 2, Model: model.NICModelVirtio})

	tk, err := svc.AddInterface(ctx, row.ID, vm.InterfaceRequest{Model: model.NICModelE1000},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("新增网卡失败: %v", err)
	}
	if tk.Type != model.TaskVMInterfaceChange {
		t.Errorf("任务类型 = %q", tk.Type)
	}

	var added model.VMInterface
	if err := db.Where("vm_id = ? AND model = ?", row.ID, model.NICModelE1000).
		First(&added).Error; err != nil {
		t.Fatalf("未写入网卡记录: %v", err)
	}

	// 新序号取「当前最大 + 1」而不是「已有数量」：数量法会算出 2，
	// 与已存在的那块冲突，而冲突的表现是唯一约束报错——用户看到的是
	// 「添加失败」却完全不知道原因。
	if added.Order != 3 {
		t.Errorf("新网卡序号 = %d, 期望 3（已有 0 与 2，取最大+1）", added.Order)
	}

	// MAC 必须在受理时就确定并写入：交给节点随机分配的话，同一块网卡在
	// 每次重建后会得到不同的 MAC，而来宾里可能已经按它配好了网络。
	if added.MAC == nil || *added.MAC == "" {
		t.Error("未分配 MAC 地址")
	}
}

func TestInterfaceMACIsStableAcrossRebuild(t *testing.T) {
	// 同一台虚拟机的同一序号应当拿到同一个 MAC——这是「网卡重建后来宾
	// 仍然能上网」的前提。
	a := vm.MACFor(7, 0)
	b := vm.MACFor(7, 0)
	if a != b {
		t.Errorf("同一 (vm, order) 生成了不同的 MAC: %s vs %s", a, b)
	}
	// 不同虚拟机或不同序号必须不同，否则同一节点上会出现重复 MAC。
	if vm.MACFor(7, 0) == vm.MACFor(7, 1) {
		t.Error("同一虚拟机的不同网卡拿到了相同 MAC")
	}
	if vm.MACFor(7, 0) == vm.MACFor(8, 0) {
		t.Error("不同虚拟机拿到了相同 MAC")
	}
}

func TestAddInterfaceRejectsBadModel(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-nic-model", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 型号取值同时出现在迁移、模型与界面三处，因此校验要在服务端做一次。
	for _, bad := range []string{"", "vmxnet3", "VIRTIO"} {
		_, err := svc.AddInterface(ctx, row.ID, vm.InterfaceRequest{Model: bad},
			authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		assertAPIError(t, err, 400)
	}

	_, err := svc.AddInterface(ctx, row.ID, vm.InterfaceRequest{
		Model: model.NICModelVirtio, RateLimitMbps: -1,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

func TestRemoveInterfaceRejectsPrimary(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-nic-rm", OwnerID: ptr(int64(7))}
	db.Create(&row)

	primary := model.VMInterface{
		VMID: row.ID, NodeID: 1, Order: 0, IsPrimary: true, Model: model.NICModelVirtio,
	}
	db.Create(&primary)

	// 主网卡不可删除：重装系统（f-2-11）依赖它保持网络可达，
	// 删掉它就没有恢复路径了。
	_, err := svc.RemoveInterface(ctx, row.ID, primary.ID,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	var count int64
	db.Model(&model.VMInterface{}).Where("id = ?", primary.ID).Count(&count)
	if count != 1 {
		t.Error("被拒绝的删除不应移除记录")
	}
}

func TestBindStaticIPRejectsTakenAddress(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	mine := model.VM{NodeID: 1, Name: "vm-ip-mine", OwnerID: ptr(int64(7))}
	other := model.VM{NodeID: 1, Name: "vm-ip-other", OwnerID: ptr(int64(7))}
	db.Create(&mine)
	db.Create(&other)

	db.Create(&model.StaticIP{
		NodeID: 1, VMID: &other.ID, IP: "10.0.0.5",
		AddressFamily: model.AddressFamilyIPv4,
	})

	// 同一地址不能分配给两台虚拟机：只有一台能真正用上它，而另一台会
	// 表现为「网络时通时断」——这类问题极难排查。
	_, err := svc.BindStaticIP(ctx, mine.ID, vm.BindStaticIPRequest{IP: "10.0.0.5"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)

	// 未被占用的地址应通过。
	if _, err := svc.BindStaticIP(ctx, mine.ID, vm.BindStaticIPRequest{IP: "10.0.0.6"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Errorf("未被占用的地址应被接受: %v", err)
	}
}

func TestAddPortForwardRejectsTakenPort(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	a := model.VM{NodeID: 1, Name: "vm-pf-a", OwnerID: ptr(int64(7))}
	b := model.VM{NodeID: 1, Name: "vm-pf-b", OwnerID: ptr(int64(7))}
	db.Create(&a)
	db.Create(&b)

	if _, err := svc.AddPortForward(ctx, a.ID, vm.AddPortForwardRequest{
		Protocol: model.PortProtocolTCP, HostPort: 8080, TargetPort: 80,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("首次新增失败: %v", err)
	}

	// 端口在节点内独占。两个转发抢同一个端口只会让其中一个静默失效，
	// 而用户会以为两条规则都在工作。
	_, err := svc.AddPortForward(ctx, b.ID, vm.AddPortForwardRequest{
		Protocol: model.PortProtocolTCP, HostPort: 8080, TargetPort: 80,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 409)

	// 换协议或换端口都应通过——独占的粒度是 (协议, 端口)。
	if _, err := svc.AddPortForward(ctx, b.ID, vm.AddPortForwardRequest{
		Protocol: model.PortProtocolUDP, HostPort: 8080, TargetPort: 80,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Errorf("不同协议的同号端口应被接受: %v", err)
	}
}

func TestAddPortForwardValidatesPort(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-pf-port", OwnerID: ptr(int64(7))}
	db.Create(&row)

	for _, p := range []int{0, -1, 70000} {
		_, err := svc.AddPortForward(ctx, row.ID, vm.AddPortForwardRequest{
			Protocol: model.PortProtocolTCP, HostPort: p, TargetPort: 80,
		}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("端口 %d 应被拒绝", p)
		}
	}
}

func TestNetworkWriteRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-nic-theirs", OwnerID: ptr(int64(20))}
	db.Create(&theirs)
	nic := model.VMInterface{VMID: theirs.ID, NodeID: 1, Order: 0, Model: model.NICModelVirtio}
	db.Create(&nic)

	// 归属校验要落在**每一项资源**上，而不只是虚拟机上：只查网卡 ID
	// 会让知道 ID 的人改掉别人的网卡。
	_, err := svc.AddInterface(ctx, theirs.ID, vm.InterfaceRequest{Model: model.NICModelVirtio},
		authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)

	_, err = svc.UpdateInterface(ctx, theirs.ID, nic.ID, vm.InterfaceRequest{Model: model.NICModelVirtio},
		authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)

	_, err = svc.AddPortForward(ctx, theirs.ID, vm.AddPortForwardRequest{
		Protocol: model.PortProtocolTCP, HostPort: 9000, TargetPort: 80,
	}, authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}

// --- 批量操作 ---

func TestBatchPowerPartiallySucceeds(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	ok1 := model.VM{NodeID: 1, Name: "vm-b1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	ok2 := model.VM{NodeID: 1, Name: "vm-b2", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	theirs := model.VM{NodeID: 1, Name: "vm-b3", Status: model.VMStatusRunning, OwnerID: ptr(int64(20))}
	db.Create(&ok1)
	db.Create(&ok2)
	db.Create(&theirs)

	resp, err := svc.Batch(ctx, vm.BatchRequest{
		VMIDs:  []int64{ok1.ID, theirs.ID, ok2.ID},
		Action: "shutdown",
	}, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("批量操作本身不应失败: %v", err)
	}

	// **部分成功**：一台越权不该影响另外两台。
	if resp.Succeeded != 2 || resp.Failed != 1 {
		t.Errorf("成功 %d 台、失败 %d 台，期望 2 与 1（items=%+v）",
			resp.Succeeded, resp.Failed, resp.Items)
	}

	// 失败项必须带**原因**：只标一个红叉会让用户去猜是权限、状态还是网络问题。
	for _, item := range resp.Items {
		if !item.OK && item.Error == "" {
			t.Errorf("失败项未给出原因: %+v", item)
		}
		if item.OK && item.TaskID == 0 {
			t.Errorf("成功项未返回任务标识: %+v", item)
		}
	}
}

func TestBatchPowerRejectsWrongStatePerItem(t *testing.T) {
	// mock 探测固定返回 running，因此 start 会对每一台都失败。
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	a := model.VM{NodeID: 1, Name: "vm-c1", OwnerID: ptr(int64(7))}
	b := model.VM{NodeID: 1, Name: "vm-c2", OwnerID: ptr(int64(7))}
	db.Create(&a)
	db.Create(&b)

	resp, err := svc.Batch(ctx, vm.BatchRequest{
		VMIDs: []int64{a.ID, b.ID}, Action: "start",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("批量操作本身不应失败: %v", err)
	}

	// 动作非法 ≠ 请求非法：整个请求在动作拼错时会被拒（见下一个测试），
	// 而这里是「动作合法但每一台状态都不允许」——响应仍是 200，
	// 逐台给出原因。
	if resp.Succeeded != 0 || resp.Failed != 2 {
		t.Errorf("成功 %d 台、失败 %d 台，期望 0 与 2", resp.Succeeded, resp.Failed)
	}
	for _, item := range resp.Items {
		if !strings.Contains(item.Error, "运行中") {
			t.Errorf("失败原因未说明当前状态: %q", item.Error)
		}
	}
}

func TestBatchPowerDeduplicates(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	vm1 := model.VM{NodeID: 1, Name: "vm-d1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&vm1)

	// 同一个 ID 传两次只应受理一次。重复受理会产生两个任务，而第二个
	// 必然因为「状态已变」而失败——用户看到一条莫名的失败记录。
	resp, err := svc.Batch(ctx, vm.BatchRequest{
		VMIDs: []int64{vm1.ID, vm1.ID}, Action: "shutdown",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("批量操作失败: %v", err)
	}
	if len(resp.Items) != 1 || resp.Succeeded != 1 {
		t.Errorf("重复 ID 未被去重: items=%d succeeded=%d", len(resp.Items), resp.Succeeded)
	}
}

func TestBatchPowerRejectsBadRequests(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	vm1 := model.VM{NodeID: 1, Name: "vm-e1", OwnerID: ptr(int64(7))}
	db.Create(&vm1)

	// 空列表。
	_, err := svc.Batch(ctx, vm.BatchRequest{Action: "shutdown"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)

	// 动作拼错：整个请求就该被拒，而不是对每一台各失败一次。
	_, err = svc.Batch(ctx, vm.BatchRequest{
		VMIDs: []int64{vm1.ID}, Action: "explode",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)

	// 超出单次上限。
	ids := make([]int64, 51)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	_, err = svc.Batch(ctx, vm.BatchRequest{VMIDs: ids, Action: "shutdown"},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 400)
}

// --- 业务软锁（F-2-12）---

func TestLockBlocksDeleteAndUnlockRestores(t *testing.T) {
	// 探测返回 stopped，这样「能不能删」只由锁定决定，不受运行态干扰。
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-lock", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	view, err := svc.SetLock(ctx, row.ID,
		vm.LockRequest{Locked: true, Reason: "生产环境禁止删除"}, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("加锁失败: %v", err)
	}
	if !view.Locked || view.LockReason != "生产环境禁止删除" {
		t.Errorf("加锁后视图未反映状态: locked=%v reason=%q", view.Locked, view.LockReason)
	}

	// 删除被拒，且理由必须说明「怎么解决」而不只是「不能删」。
	_, err = svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		viewer, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if !strings.Contains(apiErr.Message, "锁定") {
			t.Errorf("拒绝文案未说明锁定: %q", apiErr.Message)
		}
		if !strings.Contains(apiErr.Message, "解锁") {
			t.Errorf("拒绝文案未说明该怎么办: %q", apiErr.Message)
		}
		// 原因要一并带出：一句「已锁定」不告诉用户该找谁、为什么不能删。
		if !strings.Contains(apiErr.Message, "生产环境禁止删除") {
			t.Errorf("拒绝文案未带出锁定原因: %q", apiErr.Message)
		}
	}

	// 解锁后应能正常删除。
	after, err := svc.SetLock(ctx, row.ID, vm.LockRequest{Locked: false},
		viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("解锁失败: %v", err)
	}
	if after.Locked {
		t.Error("解锁后视图仍显示已锁定")
	}
	// 解锁时把 reason 一起清掉：留着会得到「未锁定，但原因是『生产环境禁止删除』」
	// 这种自相矛盾的记录。
	if after.LockReason != "" {
		t.Errorf("解锁后锁定原因未清空: %q", after.LockReason)
	}

	if _, err := svc.Delete(ctx, row.ID, vm.DeleteRequest{DiskAction: vm.DiskActionKeep},
		viewer, "alice", "10.0.0.1"); err != nil {
		t.Errorf("解锁后应可删除: %v", err)
	}
}

func TestLockIsIdempotent(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-lock-idem", OwnerID: ptr(int64(7))}
	db.Create(&row)

	first, err := svc.SetLock(ctx, row.ID, vm.LockRequest{Locked: true, Reason: "第一次"},
		viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("首次加锁失败: %v", err)
	}
	firstAt := first.LockedAt

	// 重复加锁不刷新 LockedAt：「锁了多久」是排查时第一眼要看的信息，
	// 被一次重复点击重置成「刚刚」会让它失真。
	second, err := svc.SetLock(ctx, row.ID, vm.LockRequest{Locked: true, Reason: "第二次"},
		viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("重复加锁失败: %v", err)
	}
	if firstAt == nil || second.LockedAt == nil || !second.LockedAt.Equal(*firstAt) {
		t.Errorf("重复加锁刷新了加锁时间: %v → %v", firstAt, second.LockedAt)
	}
	if second.LockReason != "第一次" {
		t.Errorf("重复加锁覆盖了原原因: %q", second.LockReason)
	}

	// 解锁两次同样不报错。
	for i := 0; i < 2; i++ {
		if _, err := svc.SetLock(ctx, row.ID, vm.LockRequest{Locked: false},
			viewer, "alice", "10.0.0.1"); err != nil {
			t.Fatalf("第 %d 次解锁失败: %v", i+1, err)
		}
	}
}

// TestLockVisibleInListView 确认列表接口带出锁定状态。
//
// 批量操作要**提前**提示「其中 N 台已锁定」（f-2-01 R-010），而前端的判断
// 依据只能来自列表本身——让每个页面各自再查一次锁定，迟早会出现「界面上
// 没标锁定、点删除却被拒绝」的不一致。
func TestLockVisibleInListView(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	locked := model.VM{NodeID: 1, Name: "vm-l1", OwnerID: ptr(int64(7))}
	free := model.VM{NodeID: 1, Name: "vm-l2", OwnerID: ptr(int64(7))}
	db.Create(&locked)
	db.Create(&free)

	if _, err := svc.SetLock(ctx, locked.ID, vm.LockRequest{Locked: true, Reason: "别删"},
		viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("加锁失败: %v", err)
	}

	views, _, err := svc.List(ctx, vm.ListFilter{Viewer: viewer, PageSize: 50})
	if err != nil {
		t.Fatalf("列表查询失败: %v", err)
	}

	byName := map[string]vm.View{}
	for _, v := range views {
		byName[v.Name] = v
	}
	if !byName["vm-l1"].Locked {
		t.Error("已加锁的虚拟机在列表里未标记锁定")
	}
	if byName["vm-l1"].LockReason != "别删" {
		t.Errorf("列表未带出锁定原因: %q", byName["vm-l1"].LockReason)
	}
	if byName["vm-l2"].Locked {
		t.Error("未加锁的虚拟机被标记为锁定")
	}
}

// TestBatchDeleteReportsLockedPerItem 覆盖 R-010 的后半句：
// 批量删除含锁定项时**不静默跳过**——失败项必须出现在结果里。
func TestBatchDeleteReportsLockedPerItem(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	locked := model.VM{NodeID: 1, Name: "vm-bd1", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	ok1 := model.VM{NodeID: 1, Name: "vm-bd2", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	ok2 := model.VM{NodeID: 1, Name: "vm-bd3", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&locked)
	db.Create(&ok1)
	db.Create(&ok2)

	if _, err := svc.SetLock(ctx, locked.ID, vm.LockRequest{Locked: true, Reason: "受保护"},
		viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("加锁失败: %v", err)
	}

	resp, err := svc.Batch(ctx, vm.BatchRequest{
		VMIDs:      []int64{locked.ID, ok1.ID, ok2.ID},
		Action:     vm.BatchActionDelete,
		DiskAction: vm.DiskActionKeep,
	}, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("批量操作本身不应失败: %v", err)
	}

	if resp.Succeeded != 2 || resp.Failed != 1 {
		t.Errorf("成功 %d 台、失败 %d 台，期望 2 与 1（items=%+v）",
			resp.Succeeded, resp.Failed, resp.Items)
	}

	// 被锁的那台必须在结果里**出现**并且带原因——这正是「不静默跳过」。
	var found bool
	for _, item := range resp.Items {
		if item.VMID != locked.ID {
			continue
		}
		found = true
		if item.OK {
			t.Error("已锁定的虚拟机不应删除成功")
		}
		if !strings.Contains(item.Error, "锁定") {
			t.Errorf("失败原因未说明锁定: %q", item.Error)
		}
	}
	if !found {
		t.Error("已锁定的虚拟机未出现在结果里——这属于静默跳过")
	}
}

func TestBatchDeleteRequiresDiskAction(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-da", OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 磁盘处理方式不给默认值（R-009）：连盘删除不可逆、保留磁盘会留下
	// 孤儿数据，两者代价完全不同，服务端替用户选一个等于把决定藏起来。
	for _, bad := range []string{"", "unknown"} {
		_, err := svc.Batch(ctx, vm.BatchRequest{
			VMIDs: []int64{row.ID}, Action: vm.BatchActionDelete, DiskAction: bad,
		}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("磁盘处理方式 %q 应被拒绝", bad)
		}
	}
}

func TestLockRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnv(t)
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-lock-theirs", OwnerID: ptr(int64(20))}
	db.Create(&theirs)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.SetLock(ctx, theirs.ID, vm.LockRequest{Locked: true},
		authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}

func ptr[T any](v T) *T { return &v }
