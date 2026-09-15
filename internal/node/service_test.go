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
	if err := db.AutoMigrate(&model.Node{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return node.NewService(db, runtime, audit.NewRecorder(db)), db
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

	token, view, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("生成注册令牌失败: %v", err)
	}
	if len(token) != 48 {
		t.Errorf("令牌长度 = %d, 期望 48", len(token))
	}
	if view.EnrollState != model.NodeEnrollPending {
		t.Errorf("注册状态 = %q, 期望 pending", view.EnrollState)
	}

	// 明文令牌只在这一处返回；数据库里必须是哈希。
	var stored model.Node
	db.First(&stored, view.ID)
	if stored.EnrollTokenHash == nil || *stored.EnrollTokenHash == token {
		t.Fatal("数据库存的是令牌明文")
	}
	if *stored.EnrollTokenHash != sha256Hex(token) {
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
		if _, _, err := svc.CreateEnrollToken(ctx, name, 0, 1, "admin", ""); err == nil {
			t.Errorf("非法节点名 %q 未被拒绝", name)
		}
	}
}

func TestCreateEnrollTokenRejectsDuplicateName(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	if _, _, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", ""); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	_, _, err := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	// 唯一约束冲突应转成可操作的提示，而不是笼统的 500。
	assertAPIError(t, err, 409)
}

func TestRegisterSucceeds(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	token, created, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")

	view, err := svc.Register(ctx, token, "agent-abc", "1.2.3")
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if view.EnrollState != model.NodeEnrollEnrolled {
		t.Errorf("注册状态 = %q, 期望 enrolled", view.EnrollState)
	}

	var stored model.Node
	db.First(&stored, created.ID)
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

	_, _, _ = svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")

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

	token, _, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	if _, err := svc.Register(ctx, token, "agent-a", "1.0"); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}

	_, err := svc.Register(ctx, token, "agent-b", "1.0")
	assertAPIError(t, err, 403)
}

func TestRegisterRejectsExpiredToken(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	token, created, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	// 直接改库把过期时间挪到过去：CreateEnrollToken 会把非正 TTL 回落为
	// 默认有效期，因此无法通过参数构造出过期令牌。
	db.Model(&model.Node{}).Where("id = ?", created.ID).
		Update("enroll_expires_at", time.Now().Add(-time.Hour))

	_, err := svc.Register(ctx, token, "agent-a", "1.0")
	assertAPIError(t, err, 403)
}

// 运行态由心跳时间推导，而不是读 agent 上报的状态字符串。
func TestStatusDerivation(t *testing.T) {
	runtime := &fakeRuntime{snap: map[int64]*agent.Snapshot{}}
	svc, db := newTestService(t, runtime)
	ctx := context.Background()

	token, _, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	view, _ := svc.Register(ctx, token, "agent-a", "1.0")

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

	_, _, _ = svc.CreateEnrollToken(context.Background(), "node-1", 0, 1, "admin", "")

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

func TestRemoveIsSoftDelete(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	token, created, _ := svc.CreateEnrollToken(ctx, "node-1", 0, 1, "admin", "")
	_, _ = svc.Register(ctx, token, "agent-a", "1.0")

	if err := svc.Remove(ctx, created.ID, 1, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("移除失败: %v", err)
	}

	// 软删除：记录仍在（审计与历史任务需要引用），但列表不再返回。
	// 用 Unscoped 才能查到——普通查询会被 GORM 自动加上 deleted_at IS NULL，
	// 这正说明软删除过滤是生效的。
	var stored model.Node
	if err := db.Unscoped().First(&stored, created.ID).Error; err != nil {
		t.Fatalf("记录应保留: %v", err)
	}
	if !stored.DeletedAt.Valid {
		t.Error("未设置软删除标记")
	}

	nodes, _ := svc.List(ctx)
	if len(nodes) != 0 {
		t.Errorf("移除后的节点仍出现在列表中: %+v", nodes)
	}

	assertAPIError(t, svc.Remove(ctx, created.ID, 1, "admin", ""), 404)
}
