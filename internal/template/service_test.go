package template_test

import (
	"context"
	"errors"
	"path/filepath"
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
	"k_cockpit/internal/template"
)

// statusClient 让 OpVMStatus 返回指定状态。
//
// mock 固定返回 running，而本包绝大多数用例需要一个**已关机**的源虚拟机
// ——既然「必须先关机」正是要验证的规则之一，就不能把这条规则绕过去。
type statusClient struct {
	*agent.MockClient
	status string
}

func (c *statusClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind == agent.OpVMStatus {
		return &agent.Result{Success: true, Data: map[string]any{agent.StatusDataKey: c.status}}, nil
	}
	return c.MockClient.Execute(ctx, op)
}

func newTestEnv(t *testing.T, status string) (*template.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "template.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.Template{}, &model.VM{}, &model.Task{}, &model.TaskStage{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	client := &statusClient{MockClient: agent.NewMockClient(), status: status}
	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	queue.Register(template.NewPrepareExecutor(db, client))
	queue.Register(template.NewDeleteExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return template.NewService(db, queue, recorder, client), db
}

func seedVM(t *testing.T, db *gorm.DB, name string, owner int64) *model.VM {
	t.Helper()
	row := model.VM{
		NodeID: 1, Name: name, Status: model.VMStatusStopped,
		VCPU: 2, MemoryMB: 2048, DiskGB: 20, Present: true,
		OwnerID: &owner,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &row
}

// TestPrepareRequiresStoppedVM 覆盖一条**不能只看投影**的规则。
//
// 运行中的系统盘在被复制的同时还在被写入，复制出来的模板是崩溃一致性的
// 快照——而它会被反复克隆成新机器，于是**每一台**克隆机开机都要做 fsck，
// 运气不好就是只读挂载。这类现象几乎不可能被追溯到模板制备的那一刻。
func TestPrepareRequiresStoppedVM(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusRunning)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-x", 7)

	_, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-x",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")

	assertStatus(t, err, 422)
}

func TestPrepareWritesTemplateAfterNodeSucceeds(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-y", 7)

	tk, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-y", OSType: "linux",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "tpl-y").Count(&n)
		return n == 1
	})

	views, err := svc.List(ctx, template.ListFilter{Viewer: authz.Viewer{UserID: 7}})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("模板数 = %d, 期望 1", len(views))
	}
	tpl := views[0]
	if tpl.Status != model.TemplateReady {
		t.Errorf("状态 = %q, 期望 ready", tpl.Status)
	}
	// 默认硬件参数取自源虚拟机：不取的话每次克隆都要重新填一遍，
	// 而用户多半会填得和源机器不一样——那不是他想要的。
	if tpl.DefaultCPU != 2 || tpl.DefaultMemoryMB != 2048 {
		t.Errorf("默认参数 = %d 核 / %d MB，期望 2 / 2048",
			tpl.DefaultCPU, tpl.DefaultMemoryMB)
	}
	if tpl.Published {
		t.Error("默认应为私有（未发布），共享必须是显式动作")
	}
	_ = tk
}

func TestPrepareRejectsDuplicateName(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-z", 7)

	if _, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-dup",
	}, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Fatalf("首次受理失败: %v", err)
	}
	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "tpl-dup").Count(&n)
		return n == 1
	})

	// 同名：给出「已有同名模板」而不是让数据库抛一句唯一约束冲突。
	_, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-dup",
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertStatus(t, err, 409)
}

// TestDeleteRejectedWhenLinkedClonesExist 覆盖 F-3-02 最有杀伤力的那条规则。
//
// linked 克隆体的磁盘只是一个 overlay，父盘一删，那些虚拟机的数据就不可用了
// ——而且**不会立刻报错**，要等到下次开机或读到某个未缓存的数据块时才暴露。
func TestDeleteRejectedWhenLinkedClonesExist(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-w", 7)

	if _, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-w",
	}, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "tpl-w").Count(&n)
		return n == 1
	})

	var tpl model.Template
	db.Where("name = ?", "tpl-w").First(&tpl)

	// 一台链式克隆依赖它。
	if err := db.Create(&model.VM{
		NodeID: 1, Name: "clone-1", Status: model.VMStatusStopped,
		TemplateID: &tpl.ID, CloneMode: model.CloneLinked, Present: true,
	}).Error; err != nil {
		t.Fatalf("创建克隆体失败: %v", err)
	}

	_, err := svc.Delete(ctx, tpl.ID, authz.Viewer{UserID: 7}, "alice", "")
	assertStatus(t, err, 409)

	// 完整克隆**不**阻止删除：它与模板完全独立，模板删了也不影响。
	if err := db.Model(&model.VM{}).Where("name = ?", "clone-1").
		Update("clone_mode", model.CloneFull).Error; err != nil {
		t.Fatalf("改为完整克隆失败: %v", err)
	}
	if _, err := svc.Delete(ctx, tpl.ID, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Errorf("没有链式依赖时应可删除: %v", err)
	}
}

// TestUpdateIsSynchronous 覆盖发布是同步的。
//
// 发布与否决定了**别人能不能用**。做成任务会制造一个「界面说已发布、
// 实际还没发布」的窗口，而用户会拿这个状态去跟同事说「已经发你了」。
func TestUpdateIsSynchronous(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	vmRow := seedVM(t, db, "vm-p", 7)

	if _, err := svc.CreateFromVM(ctx, template.CreateFromVMRequest{
		VMID: vmRow.ID, Name: "tpl-p",
	}, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitFor(t, func() bool {
		var n int64
		db.Model(&model.Template{}).Where("name = ?", "tpl-p").Count(&n)
		return n == 1
	})
	var tpl model.Template
	db.Where("name = ?", "tpl-p").First(&tpl)

	yes := true
	view, err := svc.Update(ctx, tpl.ID, template.UpdateRequest{Published: &yes},
		authz.Viewer{UserID: 7}, "alice", "")
	if err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	// 返回的视图里就已经是发布状态——不必再查一次。
	if !view.Published {
		t.Error("发布后视图未反映，说明它是异步的")
	}

	var stored model.Template
	db.First(&stored, tpl.ID)
	if !stored.Published {
		t.Error("数据库未写入发布状态")
	}
}

// TestVisibilityFiltersTemplates 覆盖可见性规则。
func TestVisibilityFiltersTemplates(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()

	alice, bob := int64(7), int64(8)
	rows := []model.Template{
		{NodeID: 1, Name: "t-alice-private", Status: model.TemplateReady, CreatedBy: &alice},
		{NodeID: 1, Name: "t-bob-private", Status: model.TemplateReady, CreatedBy: &bob},
		{NodeID: 1, Name: "t-public", Status: model.TemplateReady, Published: true, CreatedBy: &bob},
	}
	for i := range rows {
		if err := db.Create(&rows[i]).Error; err != nil {
			t.Fatalf("创建模板失败: %v", err)
		}
	}

	// Alice 看到：自己私有的 + 别人已发布的。看不到 Bob 私有的。
	views, err := svc.List(ctx, template.ListFilter{Viewer: authz.Viewer{UserID: alice}})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	got := map[string]bool{}
	for _, v := range views {
		got[v.Name] = true
	}
	if !got["t-alice-private"] {
		t.Error("看不到自己创建的私有模板")
	}
	if !got["t-public"] {
		t.Error("看不到别人已发布的模板")
	}
	if got["t-bob-private"] {
		t.Error("看到了别人的私有模板——可见性过滤失效")
	}

	// 管理员看到全部。
	all, err := svc.List(ctx, template.ListFilter{Viewer: authz.Viewer{UserID: 99, IsAdmin: true}})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("管理员看到 %d 个模板, 期望 3", len(all))
	}
}

func TestGetRejectsOthersPrivateTemplate(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()

	bob := int64(8)
	row := model.Template{
		NodeID: 1, Name: "t-secret", Status: model.TemplateReady, CreatedBy: &bob,
	}
	db.Create(&row)

	// 404 而非 403：403 会确认「这个 ID 存在」，让 tenant 能通过枚举
	// 推断出别人有多少模板。
	_, err := svc.Get(ctx, row.ID, authz.Viewer{UserID: 7})
	assertStatus(t, err, 404)
}

// --- 辅助 ---

func asAPIError(err error, target **api.Error) bool {
	return errors.As(err, target)
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !asAPIError(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待模板记录建立超时")
}
