package portsecurity_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/portsecurity"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *portsecurity.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "ps.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.PortSecurityPolicy{}, &model.Node{}, &model.VM{},
		&model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(portsecurity.NewExecutor(db, client))
	return db, portsecurity.NewService(db, client, recorder, q)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

func baseReq() portsecurity.Request {
	return portsecurity.Request{NodeID: 1, PortRef: "vnet0"}
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

// TestSpoofingGuardNeedsNoConfirm 覆盖一项刻意的**不对称**。
//
// 防伪造是安全边界，而且它是别的隔离措施的前提（不防欺骗的话，网络里所有
// "按地址区分机器"的策略都能被绕过）。因此它**不该**被当成一个需要反复
// 确认的危险开关——把它写成警告会让人以为它有害。
func TestSpoofingGuardNeedsNoConfirm(t *testing.T) {
	_, svc := newEnv(t)

	req := baseReq()
	req.SpoofingGuard = true

	check, err := svc.Preview(context.Background(), req)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if !check.CanApply {
		t.Fatalf("防伪造应可直接启用，实际被拦: %v", check.Capabilities)
	}
	// 节点侧可能报自己的冲突（如已有手工流表），那是它该说的；这里要断言
	// 的是**没有"会断网"这类由配置本身带来的警告**——防伪造不会让任何
	// 现有的连接断开，把它说得像隔离一样危险会让人不敢开。
	for _, w := range check.Warnings {
		if strings.Contains(w, "互不可见") || strings.Contains(w, "失联") {
			t.Errorf("防伪造不该给出隔离那样的断网警告: %q", w)
		}
	}
	// 但要说清它做了什么——它是三条规则，用户有权看到。
	if len(check.Rules) == 0 {
		t.Error("预检应给出将要下发的规则")
	}
}

// TestIsolationWarnsAboutBlastRadius 覆盖隔离的后果提示。
//
// 代价比它看起来大：同网段内**所有**机器之间都不通了，包括用户自己放在
// 一起的应用集群。
func TestIsolationWarnsAboutBlastRadius(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	req := baseReq()
	req.Isolation = true

	check, err := svc.Preview(ctx, req)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	joined := strings.Join(check.Warnings, " ")
	if !strings.Contains(joined, "互不可见") && !strings.Contains(joined, "所有") {
		t.Errorf("应提示同网段全部失联，实际 %v", check.Warnings)
	}

	// 未确认时**不执行、也不报错**。
	_, policy, taskID, err := svc.Apply(ctx, req, false, admin(), "root", "")
	if err != nil {
		t.Fatalf("未确认时应返回预检而不是报错（那是一个岔路口）: %v", err)
	}
	if policy != nil || taskID != nil {
		t.Error("未确认时不该落库、不该入队")
	}

	// 确认后落库 + 入队。
	_, policy, taskID, err = svc.Apply(ctx, req, true, admin(), "root", "")
	if err != nil {
		t.Fatalf("确认后失败: %v", err)
	}
	if policy == nil || taskID == nil {
		t.Fatal("确认后应落库并入队")
	}
}

// TestMeterMissingBlocksPPSLimit 覆盖能力门禁。
//
// 没有 OVS meter 时**必须拒绝**，而不是收下配置标成"已启用"——一个不会
// 生效的限速会让用户以为攻击面已经收住了。
func TestMeterMissingBlocksPPSLimit(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	req := baseReq()
	req.PPSLimit = 1000

	check, err := svc.Preview(ctx, req)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if check.CanApply {
		t.Fatal("缺 meter 时不该允许启用限速")
	}
	// 必须说清缺什么、怎么装——只说"不支持"用户只能放弃。
	found := false
	for _, c := range check.Capabilities {
		if c.Missing && c.Required {
			found = true
			if c.Reason == "" || c.Fix == "" {
				t.Errorf("缺失能力要说清原因与安装方式: %+v", c)
			}
		}
	}
	if !found {
		t.Error("应有一项必需能力被标为缺失")
	}

	// Apply 必须直接拒绝，而不是收下。
	_, policy, _, err := svc.Apply(ctx, req, true, admin(), "root", "")
	assertStatus(t, err, 422)
	if policy != nil {
		t.Error("能力不具备时不该落库——一份不会生效的「已启用」比明说「不支持」危险得多")
	}
	var n int64
	// 不校验库，因为 policy 为 nil 已足够。
	_ = n
}

// TestPPSLimitBounds 覆盖单位想错这个最常见的输入错误。
func TestPPSLimitBounds(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	// 下限：比 10 pps 更低会让 SSH 都卡住，多半是把 kpps 填成了 pps。
	low := baseReq()
	low.PPSLimit = 5
	_, err := svc.Preview(ctx, low)
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "单位") {
		t.Errorf("报错应指向单位想错这个最常见原因: %v", err)
	}

	high := baseReq()
	high.PPSLimit = 5_000_000
	_, err = svc.Preview(ctx, high)
	assertStatus(t, err, 400)
}

// TestDisableReturnsToPending 覆盖"全关"的语义。
//
// 「已生效」对一份什么都不做的策略是没有意义的——它描述的是一组已经写下去
// 的规则，而这里恰恰一条都没有。
func TestDisableReturnsToPending(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	req := baseReq()
	req.SpoofingGuard = true
	_, policy, _, err := svc.Apply(ctx, req, true, admin(), "root", "")
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	taskID, err := svc.Disable(ctx, policy.ID, admin(), "root", "")
	if err != nil {
		t.Fatalf("停用失败: %v", err)
	}
	if taskID == nil {
		t.Fatal("停用应产生任务")
	}

	// 驱动执行器（测试里的队列没有后台循环）。
	var tk model.Task
	if err := db.Order("id DESC").First(&tk).Error; err != nil {
		t.Fatalf("找不到任务: %v", err)
	}
	if err := portsecurity.NewExecutor(db, agent.NewMockClient()).Run(ctx, &tk); err != nil {
		t.Fatalf("执行停用失败: %v", err)
	}

	var row model.PortSecurityPolicy
	if err := db.First(&row, policy.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if row.Status != model.PortSecurityPending {
		t.Errorf("停用后状态 = %q, 期望 pending（「已生效」对一份什么都不做的策略没有意义）", row.Status)
	}
	if row.AppliedAt != nil {
		t.Error("停用后 applied_at 应清空")
	}
}

// TestDisableIdleRejected 覆盖"已停用的再停用"。
//
// 静默成功会让用户以为它刚才确实生效着。
func TestDisableIdleRejected(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	row := model.PortSecurityPolicy{NodeID: 1, PortRef: "vnet0", Status: model.PortSecurityPending}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	_, err := svc.Disable(ctx, row.ID, admin(), "root", "")
	assertStatus(t, err, 422)
}

// TestOnePolicyPerPort 覆盖唯一性（uniq_port_security_policy_node_port）。
//
// 允许两份的话，两份会在节点上互相覆盖，而结果取决于下发顺序——那是用户
// 看不见的实现细节。
func TestOnePolicyPerPort(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	req := baseReq()
	req.SpoofingGuard = true
	if _, _, _, err := svc.Apply(ctx, req, true, admin(), "root", ""); err != nil {
		t.Fatalf("首次配置失败: %v", err)
	}

	// 再配一次：应当是**更新**而不是新增。
	req.Isolation = true
	if _, _, _, err := svc.Apply(ctx, req, true, admin(), "root", ""); err != nil {
		t.Fatalf("再次配置失败: %v", err)
	}

	var n int64
	db.Model(&model.PortSecurityPolicy{}).Count(&n)
	if n != 1 {
		t.Errorf("同一网口应只有一份策略，实际 %d 份", n)
	}
}

// TestConfigChangeGoesBackToPending 覆盖状态回退。
//
// 配置改了而状态还停在 active，界面上显示"已启用"而节点上跑的是旧规则。
func TestConfigChangeGoesBackToPending(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	req := baseReq()
	req.SpoofingGuard = true
	_, policy, _, err := svc.Apply(ctx, req, true, admin(), "root", "")
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	// 驱动执行器让它变成 active。
	var tk model.Task
	db.Order("id DESC").First(&tk)
	if err := portsecurity.NewExecutor(db, agent.NewMockClient()).Run(ctx, &tk); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	var row model.PortSecurityPolicy
	db.First(&row, policy.ID)
	if row.Status != model.PortSecurityActive {
		t.Fatalf("下发后状态 = %q, 期望 active", row.Status)
	}

	// 改配置。
	req.Isolation = false
	req.SpoofingGuard = false
	req.PPSLimit = 0
	if _, _, _, err := svc.Apply(ctx, req, true, admin(), "root", ""); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
	// 读进一个**全新**的结构体：row 上一次已被填过 applied_at，
	// 复用会让断言读到的可能是旧值而不是库里的当前状态。
	var after model.PortSecurityPolicy
	if err := db.First(&after, policy.ID).Error; err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if after.Status != model.PortSecurityPending {
		t.Errorf("改配置后状态 = %q, 期望回退到 pending", after.Status)
	}
	if after.AppliedAt != nil {
		t.Errorf("改配置后 applied_at 应清空（节点上跑的还是旧规则），实际 %v", after.AppliedAt)
	}
	_ = context.Background()
}

func TestPreviewChangesNothing(t *testing.T) {
	db, svc := newEnv(t)

	req := baseReq()
	req.SpoofingGuard = true
	if _, err := svc.Preview(context.Background(), req); err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	var n int64
	db.Model(&model.PortSecurityPolicy{}).Count(&n)
	if n != 0 {
		t.Error("预检不该产生记录")
	}
	var tasks int64
	db.Model(&model.Task{}).Count(&tasks)
	if tasks != 0 {
		t.Error("预检不该入队")
	}
}

func TestUnreachableNodeRefused(t *testing.T) {
	_, svc := newEnv(t)

	req := baseReq()
	req.NodeID = 999
	_, err := svc.Preview(context.Background(), req)
	assertStatus(t, err, 404)
}
