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

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "vm.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.VM{}, &model.Task{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 2,
		PollInterval:  20 * time.Millisecond,
	})
	// 用真实的 mock agent：创建链路端到端走通，只有「执行」那一步是假的。
	client := agent.NewMockClient()
	queue.Register(vm.NewCreateExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return vm.NewService(db, queue, recorder), queue, db
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

func ptr[T any](v T) *T { return &v }
