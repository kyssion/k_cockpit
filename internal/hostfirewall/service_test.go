package hostfirewall_test

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
	"k_cockpit/internal/hostfirewall"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *hostfirewall.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "hf.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.HostFirewallPolicy{}, &model.HostFirewallRule{},
		&model.Node{}, &model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(hostfirewall.NewExecutor(db, client))
	// 面板端口 8080，SSH 22。
	return db, hostfirewall.NewService(db, q, client, recorder, 8080, []int{22})
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

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

// TestPanelAndSSHAreFixedProtectedRules 覆盖合成的保护规则。
//
// 它们**不在表里**，由面板按当前配置算出来：端口是配置项，存进表之后
// 改配置表里那条就过期了——而一条过期的"保护规则"比没有更糟，它保护着
// 一个不再监听的端口，而真正的端口没有任何规则挡着。
func TestPanelAndSSHAreFixedProtectedRules(t *testing.T) {
	_, svc := newEnv(t)

	_, rules, err := svc.GetPolicy(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	fixed := 0
	ports := map[int]bool{}
	for _, r := range rules {
		if r.Fixed {
			fixed++
			if r.PortStart == nil {
				t.Errorf("合成的保护规则应有端口: %+v", r)
				continue
			}
			ports[*r.PortStart] = true
			if !r.IsProtected {
				t.Error("合成规则必须是保护的")
			}
			if r.FixedReason == "" {
				t.Error("合成规则要说清为什么不能改——否则用户会以为界面坏了")
			}
		}
	}
	if fixed != 2 {
		t.Fatalf("应有 2 条合成规则（面板 + SSH），实际 %d", fixed)
	}
	if !ports[8080] || !ports[22] {
		t.Errorf("端口不对: %v", ports)
	}
}

// TestProtectedRuleCannotBeDeleted 覆盖服务端强制的保护。
//
// 让界面决定能不能删，等于把一个**不可恢复**的操作交给一次点击：删掉保护
// 规则之后管理员可能立刻连不上面板，而那时他已经没有界面可以把它加回来。
func TestProtectedRuleCannotBeDeleted(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	// 直接造一条保护的存储规则。
	port := 22
	row := model.HostFirewallRule{
		NodeID: 1, Action: model.FirewallAccept, Protocol: "tcp",
		PortStart: &port, IsProtected: true,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("造数据失败: %v", err)
	}

	err := svc.DeleteRule(ctx, 1, row.ID, admin(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "保护") {
		t.Errorf("报错应说明这是保护规则: %v", err)
	}

	// 它必须还在。
	var n int64
	db.Model(&model.HostFirewallRule{}).Where("id = ?", row.ID).Count(&n)
	if n != 1 {
		t.Error("被拒绝后规则不该消失")
	}
}

// TestCreateRuleIsNeverProtected 覆盖**不接受客户端传保护标记**。
//
// 否则用户可以把一条保护规则改成不保护、删掉它，然后把自己锁在门外——
// 而整个过程看起来完全合法。
func TestCreateRuleIsNeverProtected(t *testing.T) {
	db, svc := newEnv(t)

	port := 8080
	if _, err := svc.CreateRule(context.Background(), hostfirewall.RuleRequest{
		NodeID: 1, Action: model.FirewallAccept, Protocol: "tcp", PortStart: &port,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	var row model.HostFirewallRule
	db.Where("node_id = ?", 1).First(&row)
	if row.IsProtected {
		t.Error("新建的规则永远不该是保护的——保护标记只能由服务端按配置合成")
	}
}

// TestEmptyWhitelistWithDenyWarns 覆盖最危险的组合。
//
// 白名单为空 + 默认拒绝 = **任何来源都连不上这台机器**，包括 SSH 与面板。
// 而应用之后用户已经进不去界面把它改回来。
func TestEmptyWhitelistWithDenyWarns(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	_, warnings, err := svc.Precheck(ctx, 1)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	joined := strings.Join(warnings, " ")
	if !strings.Contains(joined, "无法再连上") {
		t.Fatalf("白名单为空且默认拒绝时必须给出警告，实际 %v", warnings)
	}
	if !strings.Contains(joined, "白名单") {
		t.Error("警告应给出可操作的建议（先把管理地址加进白名单）")
	}

	// 加了白名单之后警告应消失。
	wl := "203.0.113.9/32"
	if _, err := svc.UpdatePolicy(ctx, 1, hostfirewall.PolicyRequest{Whitelist: &wl},
		admin(), "root", ""); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	_, warnings, _ = svc.Precheck(ctx, 1)
	if strings.Contains(strings.Join(warnings, " "), "无法再连上") {
		t.Error("有了白名单就不该再警告")
	}
}

// TestRollbackSkipsVersionCheck 覆盖回滚的自救属性。
//
// 它要能在"已经出事了"的那一刻还能用。校验版本或要求二次验证，等于在最
// 需要它的时候把它关掉。
func TestRollbackSkipsVersionCheck(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	t1, err := svc.Rollback(ctx, 1, admin(), "root", "")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if t1 == nil {
		t.Fatal("回滚应产生任务")
	}

	var policy model.HostFirewallPolicy
	db.Where("node_id = ?", 1).First(&policy)
	if policy.Enabled {
		t.Error("回滚后应停用")
	}
	if policy.LastRollbackAt == nil {
		t.Error("回滚要留痕——它意味着刚才那次应用把机器弄坏了")
	}
}

// TestApplyVersionMustMatch 覆盖「预览 → 应用」的版本绑定。
//
// 不校验的话，用户批准的是 A、落下去的是 B——而这类问题不报错。
func TestApplyVersionMustMatch(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	// 先改一次策略，让 version 前进。
	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, hostfirewall.PolicyRequest{Enabled: &enabled},
		admin(), "root", ""); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	pv, _, _ := svc.GetPolicy(ctx, 1)

	// 用过期版本应用。
	_, err := svc.Apply(ctx, 1, int64(pv.Version)-1, admin(), "root", "")
	assertStatus(t, err, 409)

	// 用当前版本可以。
	if _, err := svc.Apply(ctx, 1, int64(pv.Version), admin(), "root", ""); err != nil {
		t.Fatalf("用当前版本应能应用: %v", err)
	}
}

// TestICMPRejectsPort 覆盖一条「看起来有限制、实际没有」的规则。
func TestICMPRejectsPort(t *testing.T) {
	_, svc := newEnv(t)

	port := 80
	_, err := svc.CreateRule(context.Background(), hostfirewall.RuleRequest{
		NodeID: 1, Action: model.FirewallAccept, Protocol: "icmp", PortStart: &port,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "端口") {
		t.Errorf("报错应说明 ICMP 不区分端口: %v", err)
	}
}

func TestInvalidPortRangeRejected(t *testing.T) {
	_, svc := newEnv(t)

	start, end := 100, 50
	_, err := svc.CreateRule(context.Background(), hostfirewall.RuleRequest{
		NodeID: 1, Action: model.FirewallAccept, Protocol: "tcp",
		PortStart: &start, PortEnd: &end,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

// TestConnectionsMarkOwn 覆盖"哪条是你自己那条"。
//
// 关掉自己那条连接会让人以为面板挂了，而界面上必须能提前看出来。
func TestConnectionsMarkOwn(t *testing.T) {
	db, svc := newEnv(t)
	_ = db

	conns, err := svc.Connections(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取连接失败: %v", err)
	}
	if len(conns) == 0 {
		t.Fatal("mock 应返回若干条连接")
	}
	// Own 由控制面在 handler 里标记，服务层不该自己猜——节点无从知道
	// 请求方是谁。这里断言它默认为 false 而不是被节点乱填。
	for _, c := range conns {
		if c.Own {
			t.Error("节点侧不该自己填 Own——它无从知道请求方是谁")
		}
	}
}

func TestCloseConnectionRequiresTarget(t *testing.T) {
	_, svc := newEnv(t)

	err := svc.CloseConnection(context.Background(), 1, "  ", admin(), "root", "")
	assertStatus(t, err, 400)
}
