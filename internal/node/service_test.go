package node_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
)

// fakeRuntime 是可控的运行态提供者，用于覆盖 mock 覆盖不到的分支
// （离线、未知、取不到运行态）。
type fakeRuntime struct {
	snap map[int64]*agent.Snapshot
	err  error
}

func (f *fakeRuntime) Snapshot(_ context.Context, nodeID int64) (*agent.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snap[nodeID], nil
}

func onlineSnapshot() *agent.Snapshot {
	return &agent.Snapshot{
		Status:          agent.StatusOnline,
		LastHeartbeat:   time.Now(),
		AgentVersion:    "test-1.0",
		ProtocolVersion: 1,
		Capabilities:    []string{"vm.create", "vm.start"},
	}
}

func newTestService(t *testing.T, runtime agent.SnapshotProvider) (*node.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "node.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
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
	if err := db.AutoMigrate(&model.Node{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return node.NewService(db, runtime, audit.NewRecorder(db), nil), db
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

// sha256Hex 在测试端独立实现哈希，而不是调用生产代码的私有函数：
// 后者会让「实现改了、测试跟着改」变成必然，测试也就失去了把关作用。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestCreateEnrollToken(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	result, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("生成注册令牌失败: %v", err)
	}
	if len(result.Token) != 48 {
		t.Errorf("令牌长度 = %d, 期望 48", len(result.Token))
	}
	if result.Node.EnrollState != model.NodeEnrollPending {
		t.Errorf("注册状态 = %q, 期望 pending", result.Node.EnrollState)
	}
	if !result.ExpiresAt.After(time.Now()) {
		t.Error("未返回有效的过期时间")
	}

	// 明文令牌只在这一处返回；数据库里必须是哈希。
	var stored model.Node
	db.First(&stored, result.Node.ID)
	if stored.EnrollTokenHash == nil || *stored.EnrollTokenHash == result.Token {
		t.Fatal("数据库存的是令牌明文")
	}
	if *stored.EnrollTokenHash != sha256Hex(result.Token) {
		t.Error("存储的哈希与令牌不匹配")
	}
	if stored.EnrollExpiresAt == nil || !stored.EnrollExpiresAt.After(time.Now()) {
		t.Error("未设置有效的过期时间")
	}
}

func TestCreateEnrollTokenValidatesName(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	for _, name := range []string{"", "  ", "-bad", "has space", "节点"} {
		if _, err := svc.CreateEnrollToken(ctx, name, 0, 1, "admin", ""); err == nil {
			t.Errorf("非法节点名 %q 未被拒绝", name)
		}
	}
}

func TestCreateEnrollTokenRejectsDuplicateName(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	if _, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", ""); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	_, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	// 唯一约束冲突应转成可操作的提示，而不是笼统的 500。
	assertAPIError(t, err, 409)
}

func TestRegisterSucceeds(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	issued, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")

	view, err := svc.Register(ctx, issued.Token, "agent-abc", "1.2.3")
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if view.EnrollState != model.NodeEnrollEnrolled {
		t.Errorf("注册状态 = %q, 期望 enrolled", view.EnrollState)
	}

	var stored model.Node
	db.First(&stored, issued.Node.ID)
	if stored.AgentID == nil || *stored.AgentID != "agent-abc" {
		t.Error("agent 标识未写入")
	}
	// 令牌一次性：注册成功后必须销毁，否则它可以再次注册一台机器。
	if stored.EnrollTokenHash != nil {
		t.Error("注册成功后令牌哈希未销毁")
	}
	if stored.EnrollExpiresAt != nil {
		t.Error("注册成功后过期时间未清空")
	}
}

func TestRegisterRejectsInvalidToken(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	_, _ = svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")

	for name, token := range map[string]string{
		"不存在的令牌": "deadbeef",
		"空令牌":    "",
	} {
		_, err := svc.Register(ctx, token, "agent-x", "1.0")
		if err == nil {
			t.Errorf("%s 未被拒绝", name)
			continue
		}
		assertAPIError(t, err, 403)
	}
}

// 令牌是一次性的：注册成功后再次使用必须失败。
func TestRegisterTokenIsSingleUse(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	issued, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	if _, err := svc.Register(ctx, issued.Token, "agent-a", "1.0"); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}

	_, err := svc.Register(ctx, issued.Token, "agent-b", "1.0")
	assertAPIError(t, err, 403)
}

func TestRegisterRejectsExpiredToken(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	issued, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	// 直接改库把过期时间挪到过去：CreateEnrollToken 会把非正 TTL 回落为
	// 默认有效期，因此无法通过参数构造出过期令牌。
	db.Model(&model.Node{}).Where("id = ?", issued.Node.ID).
		Update("enroll_expires_at", time.Now().Add(-time.Hour))

	_, err := svc.Register(ctx, issued.Token, "agent-a", "1.0")
	assertAPIError(t, err, 403)
}

// 运行态由心跳时间推导，而不是读 agent 上报的状态字符串。
func TestStatusDerivation(t *testing.T) {
	runtime := &fakeRuntime{snap: map[int64]*agent.Snapshot{}}
	svc, db := newTestService(t, runtime)
	ctx := context.Background()

	issued, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	view, _ := svc.Register(ctx, issued.Token, "agent-a", "1.0")

	// 未提供运行态、且数据库无心跳记录 → 未知。
	db.Model(&model.Node{}).Where("id = ?", view.ID).Update("last_heartbeat_at", nil)
	runtime.snap = map[int64]*agent.Snapshot{}
	got, err := svc.Get(ctx, view.ID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.Status != model.NodeStatusUnknown {
		t.Errorf("无心跳时状态 = %q, 期望 unknown", got.Status)
	}

	// 心跳新鲜 → 在线。
	runtime.snap = map[int64]*agent.Snapshot{view.ID: onlineSnapshot()}
	got, _ = svc.Get(ctx, view.ID)
	if got.Status != model.NodeStatusOnline {
		t.Errorf("心跳新鲜时状态 = %q, 期望 online", got.Status)
	}
	if got.AgentVersion != "test-1.0" || len(got.Capabilities) != 2 {
		t.Errorf("运行态字段未被采纳: %+v", got)
	}

	// 心跳陈旧 → 离线。**这是最关键的断言**：agent 宕机时若沿用上次
	// 上报的「在线」，界面上会把失联显示成正常。
	stale := onlineSnapshot()
	stale.LastHeartbeat = time.Now().Add(-2 * node.OfflineThreshold)
	runtime.snap = map[int64]*agent.Snapshot{view.ID: stale}
	got, _ = svc.Get(ctx, view.ID)
	if got.Status != model.NodeStatusOffline {
		t.Errorf("心跳陈旧时状态 = %q, 期望 offline", got.Status)
	}
}

// 取不到运行态不应让整个列表失败——节点可能从未接入。
func TestListToleratesRuntimeFailure(t *testing.T) {
	runtime := &fakeRuntime{err: errors.New("agent 通道不可用")}
	svc, _ := newTestService(t, runtime)

	_, _ = svc.CreateEnrollToken(context.Background(), "node-1", 0, 1, "admin", "")

	nodes, err := svc.List(context.Background())
	if err != nil {
		t.Fatalf("运行态不可用时列表不应失败: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("节点数 = %d, 期望 1", len(nodes))
	}
	if nodes[0].Status != model.NodeStatusUnknown {
		t.Errorf("状态 = %q, 期望 unknown", nodes[0].Status)
	}
}

// enrolledNode 建一个已接入的节点，供维护模式用例使用。
//
// 维护模式只对已接入的节点有意义：未接入的节点上跑不了虚拟机，
// 允许切换会给出一个「设置成功但没有任何效果」的反馈。
func enrolledNode(t *testing.T, svc *node.Service, db *gorm.DB) int64 {
	t.Helper()
	row := model.Node{
		Name:        "node-m",
		EnrollState: model.NodeEnrollEnrolled,
		Enabled:     true,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return row.ID
}

func TestSetMaintenanceTogglesFlag(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()
	id := enrolledNode(t, svc, db)

	view, err := svc.SetMaintenance(ctx, id, true, "升级内核", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("进入维护模式失败: %v", err)
	}
	if !view.MaintenanceMode {
		t.Error("视图未反映维护模式")
	}
	if view.MaintenanceReason != "升级内核" {
		t.Errorf("维护原因 = %q, 期望「升级内核」", view.MaintenanceReason)
	}
	if view.MaintenanceAt == nil {
		t.Error("未记录进入维护的时间")
	}

	stored, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("查询节点失败: %v", err)
	}
	if !stored.MaintenanceMode {
		t.Error("数据库未写入维护模式")
	}

	// 退出：原因与时刻必须一起清掉。
	//
	// 只翻标志会留下「已退出维护，但原因是『升级内核』」这种自相矛盾的
	// 记录——界面要么显示一个不生效的理由，要么得写额外判断忽略它。
	view, err = svc.SetMaintenance(ctx, id, false, "", 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("退出维护模式失败: %v", err)
	}
	if view.MaintenanceMode {
		t.Error("退出后仍显示维护中")
	}
	if view.MaintenanceReason != "" || view.MaintenanceAt != nil {
		t.Errorf("退出维护后原因/时刻未清空: %q / %v", view.MaintenanceReason, view.MaintenanceAt)
	}
}

// TestSetMaintenanceDoesNotTouchUserRemark 覆盖一条**曾经写错**的行为。
//
// 最初把维护原因写进了 node.remark。那个字段属于用户自己——写进去会把
// 他的备注悄悄覆盖掉，而他通常要到维护结束、发现备注变成一句已经不成立的
// 「升级内核」时才会注意到。两个不同用途的文本共用一列，代价总在事后才显现。
func TestSetMaintenanceDoesNotTouchUserRemark(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()
	id := enrolledNode(t, svc, db)

	const userRemark = "机架 B12，上联交换机 3 号口"
	if err := db.Model(&model.Node{}).Where("id = ?", id).
		Update("remark", userRemark).Error; err != nil {
		t.Fatalf("写入备注失败: %v", err)
	}

	if _, err := svc.SetMaintenance(ctx, id, true, "升级内核", 1, "admin", ""); err != nil {
		t.Fatalf("进入维护失败: %v", err)
	}
	if _, err := svc.SetMaintenance(ctx, id, false, "", 1, "admin", ""); err != nil {
		t.Fatalf("退出维护失败: %v", err)
	}

	after, err := svc.Get(ctx, id)
	if err != nil {
		t.Fatalf("查询节点失败: %v", err)
	}
	if after.Remark != userRemark {
		t.Errorf("用户备注被改动: %q → %q", userRemark, after.Remark)
	}
}

// TestSetMaintenanceIsIdempotent 覆盖重复切换。
//
// 不重复写库、也不重复记审计：事后追查「什么时候进的维护模式」时，
// 一串同一分钟的记录反而说不清是哪一次真正生效的。
func TestSetMaintenanceIsIdempotent(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()
	id := enrolledNode(t, svc, db)

	for i := 0; i < 3; i++ {
		if _, err := svc.SetMaintenance(ctx, id, true, "", 1, "admin", "10.0.0.1"); err != nil {
			t.Fatalf("第 %d 次进入维护失败: %v", i+1, err)
		}
	}

	var count int64
	db.Model(&model.AuditLog{}).
		Where("action = ?", "node.maintenance.enter").
		Count(&count)
	if count != 1 {
		t.Errorf("审计记录 = %d 条, 期望 1 条（重复切换不应重复记账）", count)
	}
}

func TestSetMaintenanceRejectsPendingNode(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	row := model.Node{Name: "node-pending", EnrollState: model.NodeEnrollPending}
	db.Create(&row)

	// 未接入的节点上跑不了虚拟机，维护模式对它没有意义。
	_, err := svc.SetMaintenance(ctx, row.ID, true, "", 1, "admin", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestSetMaintenanceOnMissingNode(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})

	_, err := svc.SetMaintenance(context.Background(), 9999, true, "", 1, "admin", "")
	assertAPIError(t, err, 404)
}

func TestRemoveIsSoftDelete(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	issued, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	_, _ = svc.Register(ctx, issued.Token, "agent-a", "1.0")

	if err := svc.Remove(ctx, issued.Node.ID, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("移除失败: %v", err)
	}

	// 软删除：记录仍在（审计与历史任务需要引用），但列表不再返回。
	// 用 Unscoped 才能查到——普通查询会被 GORM 自动加上 deleted_at IS NULL，
	// 这正说明软删除过滤是生效的。
	var stored model.Node
	if err := db.Unscoped().First(&stored, issued.Node.ID).Error; err != nil {
		t.Fatalf("记录应保留: %v", err)
	}
	if !stored.DeletedAt.Valid {
		t.Error("未设置软删除标记")
	}

	nodes, _ := svc.List(ctx)
	if len(nodes) != 0 {
		t.Errorf("移除后的节点仍出现在列表中: %+v", nodes)
	}

	assertAPIError(t, svc.Remove(ctx, issued.Node.ID, 1, "admin", ""), 404)
}

// newStatsDB 只建节点与虚拟机表，供指标统计用例使用。
func newStatsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "node-stats.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
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
	if err := db.AutoMigrate(&model.Node{}, &model.VM{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}
