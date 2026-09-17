package firewall_test

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
	"k_cockpit/internal/firewall"
	"k_cockpit/internal/model"
)

func newTestEnv(t *testing.T) (*firewall.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "fw.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.FirewallPolicy{}, &model.FirewallRule{}, &model.FirewallVMPolicy{},
		&model.Node{}, &model.VM{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return firewall.NewService(db, agent.NewMockClient(), audit.NewRecorder(db)), db
}

func admin() authz.Viewer { return authz.Viewer{UserID: 1, IsAdmin: true} }

func seedVM(t *testing.T, db *gorm.DB, name string) *model.VM {
	t.Helper()
	owner := int64(7)
	vm := model.VM{NodeID: 1, Name: name, Status: model.VMStatusStopped, OwnerID: &owner, Present: true}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &vm
}

func port(n int) *int { return &n }

// TestPolicyStartsDisabledAndDeny 覆盖一处会让「查看页面」变成事故的设计。
//
// 首次访问时建默认策略，而它必须是**关闭**的：默认建一条启用的 deny 会让
// 所有未配规则的节点立刻开始拦截流量，包括面板自己——那是一次由「打开
// 页面」触发的停机。
func TestPolicyStartsDisabledAndDeny(t *testing.T) {
	svc, _ := newTestEnv(t)

	view, err := svc.GetPolicy(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询策略失败: %v", err)
	}
	if view.Enabled {
		t.Error("新建的策略默认必须是关闭的——否则「打开页面」会立刻开始拦截流量")
	}
	if view.DefaultAction != model.FirewallDeny {
		t.Errorf("默认处置 = %q, 期望 deny（防火墙的价值就在默认拒绝）", view.DefaultAction)
	}
}

// TestProtectedRuleCannotBeDeleted 是这块最重要的一条。
//
// 保护规则保护的是管理通道本身。一次「清理规则」的操作就能把 SSH 或面板
// 端口关掉，而那种事故**无法通过面板恢复**——那时已经连不上了，只能上
// 宿主机敲命令。因此保护必须由**服务端**强制，而不是靠界面上不给按钮。
func TestProtectedRuleCannotBeDeleted(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	rule := model.FirewallRule{
		NodeID: 1, Action: model.FirewallAccept, Protocol: model.ProtocolTCP,
		PortStart: port(22), SourceCIDR: strPtr("0.0.0.0/0"),
		IsProtected: true,
	}
	if err := db.Create(&rule).Error; err != nil {
		t.Fatalf("创建保护规则失败: %v", err)
	}

	err := svc.DeleteRule(ctx, 1, rule.ID, admin(), "root", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "保护") {
		t.Errorf("拒绝文案应说明这是保护规则: %v", err)
	}

	// 规则必须还在。
	var n int64
	db.Model(&model.FirewallRule{}).Where("id = ?", rule.ID).Count(&n)
	if n != 1 {
		t.Fatal("保护规则被删掉了——这种事故无法通过面板恢复")
	}
}

func TestNormalRuleCanBeDeleted(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	// 只测「保护规则删不掉」拦不住一个「全都删不掉」的实现。
	view, err := svc.CreateRule(ctx, 1, firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolTCP,
		PortStart: port(8080), SourceCIDR: "0.0.0.0/0",
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("新增规则失败: %v", err)
	}
	if err := svc.DeleteRule(ctx, 1, view.ID, admin(), "root", ""); err != nil {
		t.Errorf("普通规则应可删除: %v", err)
	}
}

// TestPrecheckWarnsWhenManagementWouldBeCut 覆盖预检的核心用途。
//
// 它防的是「点下应用的那一刻，用户失去了继续操作的能力」。
func TestPrecheckWarnsWhenManagementWouldBeCut(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, firewall.UpdatePolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}

	// 默认拒绝 + 白名单为空：任何未被放行的来源都会被挡掉。
	warnings, err := svc.Precheck(ctx, 1, "203.0.113.9")
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if len(warnings) == 0 {
		t.Fatal("默认拒绝 + 白名单为空时应给出警告")
	}
	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "203.0.113.9") {
		t.Errorf("警告应指出当前来源会被挡掉: %s", joined)
	}
}

// TestWhitelistBeatsDeny 覆盖白名单的优先级。
//
// 白名单优先于一切拒绝，包括区域限制。这条优先级不是便利性设计，而是
// 安全底线：管理员从某个固定 IP 管理面板，而那个 IP 万一落在被区域规则
// 挡掉的范围里（用了代理、或区域数据不准），**他会把自己锁在门外**。
func TestWhitelistBeatsDeny(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, firewall.UpdatePolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny,
		// 区域限制只允许日本，但白名单里有我们的管理 IP。
		GeoipRegions: []string{"JP"},
		Whitelist:    []string{"203.0.113.9"},
	}, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}

	warnings, err := svc.Precheck(ctx, 1, "203.0.113.9")
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "203.0.113.9") {
			t.Errorf("白名单里的来源不该被警告为会被挡掉: %s", w)
		}
	}
}

// TestApplyRequiresAcknowledge 覆盖「警告必须被显式确认」。
//
// 有警告而未确认时**不下发、也不报错**，而是把警告原样返回。报错会让界面
// 把它显示成一次失败，而它实际是一个需要用户做决定的岔路口——两者的界面
// 完全不同。
func TestApplyRequiresAcknowledge(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, firewall.UpdatePolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}

	// 未确认：不下发，返回警告。
	result, err := svc.Apply(ctx, 1, false, "203.0.113.9", admin(), "root", "")
	if err != nil {
		t.Fatalf("未确认不应报错——它是一个岔路口而不是一次失败: %v", err)
	}
	if result.Applied {
		t.Error("有未确认的警告时不该下发")
	}
	if len(result.Warnings) == 0 {
		t.Error("应把警告原样返回给调用方")
	}

	// 确认后下发。
	result, err = svc.Apply(ctx, 1, true, "203.0.113.9", admin(), "root", "")
	if err != nil {
		t.Fatalf("确认后应用失败: %v", err)
	}
	if !result.Applied {
		t.Error("确认后应当下发")
	}
}

// TestRollbackNeedsNothing 覆盖有意的**不对称**。
//
// 通往事故的路要设卡（Apply 要确认警告），从事故里出来的路不能设卡。
// 一个被自己配错的防火墙关在门外的管理员，此刻唯一的诉求是「先让我进去」，
// 而任何一道额外确认都会成为压垮他的那一步。
func TestRollbackNeedsNothing(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, firewall.UpdatePolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny,
	}, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}
	if _, err := svc.Apply(ctx, 1, true, "203.0.113.9", admin(), "root", ""); err != nil {
		t.Fatalf("应用失败: %v", err)
	}

	// 回滚：不传确认、不做预检、不需要任何凭据。
	result, err := svc.Rollback(ctx, 1, admin(), "root", "")
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if !result.Applied {
		t.Error("回滚应当生效")
	}

	after, err := svc.GetPolicy(ctx, 1)
	if err != nil {
		t.Fatalf("查询策略失败: %v", err)
	}
	if after.Enabled {
		t.Error("回滚后防火墙应当处于关闭状态")
	}
}

// TestDuplicateRuleRejected 覆盖去重索引里的 coalesce。
//
// 唯一索引里 NULL 互不相等，于是两条「端口与来源都留空」的规则会被当成
// 不同的行——而它们显然是同一条。coalesce 把 NULL 归一化之后去重才成立。
func TestDuplicateRuleRejected(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	req := firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolICMP,
		SourceCIDR: "10.0.0.0/8",
	}
	if _, err := svc.CreateRule(ctx, 1, req, admin(), "root", ""); err != nil {
		t.Fatalf("首次新增失败: %v", err)
	}
	_, err := svc.CreateRule(ctx, 1, req, admin(), "root", "")
	assertStatus(t, err, 409)
}

func TestICMPRejectsPorts(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.CreateRule(context.Background(), 1, firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolICMP,
		PortStart: port(22), SourceCIDR: "0.0.0.0/0",
	}, admin(), "root", "")
	// 填了会得到一条看起来有限制、实际没有的规则。
	assertStatus(t, err, 422)
}

func TestSourceMustBeValidCIDR(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.CreateRule(context.Background(), 1, firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolTCP,
		PortStart: port(22), SourceCIDR: "not-an-ip",
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

// TestVersionBumpsOnRuleChange 覆盖版本与规则的联动。
//
// 版本是「当前这套配置」的指纹。只把策略字段的改动算进去的话，一次规则
// 变更会被预览与应用的校验放过。
func TestVersionBumpsOnRuleChange(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	before, err := svc.GetPolicy(ctx, 1)
	if err != nil {
		t.Fatalf("查询策略失败: %v", err)
	}

	if _, err := svc.CreateRule(ctx, 1, firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolTCP,
		PortStart: port(443), SourceCIDR: "0.0.0.0/0",
	}, admin(), "root", ""); err != nil {
		t.Fatalf("新增规则失败: %v", err)
	}

	after, err := svc.GetPolicy(ctx, 1)
	if err != nil {
		t.Fatalf("查询策略失败: %v", err)
	}
	if after.Version <= before.Version {
		t.Errorf("规则变更后版本未自增: %d → %d", before.Version, after.Version)
	}
	// 新规则还没下发，因此应标记为待应用。
	if !after.Pending {
		t.Error("新增规则后应标记为有待应用的改动")
	}
}

// TestVMPolicyReplacesNotStacks 覆盖覆盖层的语义。
//
// **替换**而不是叠加：叠加会让「这台机器到底受哪些约束」变成两个列表的
// 并集，而用户排查时需要在两份配置之间来回对照。替换让这台机器的策略是
// 自包含的——看这一条就够了。
func TestVMPolicyReplacesNotStacks(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	enabled := true
	if _, err := svc.UpdatePolicy(ctx, 1, firewall.UpdatePolicyRequest{
		Enabled: &enabled, DefaultAction: model.FirewallDeny,
		Whitelist:    []string{"10.1.0.0/16"},
		GeoipRegions: []string{"JP", "SG"},
	}, admin(), "root", ""); err != nil {
		t.Fatalf("更新策略失败: %v", err)
	}
	vm := seedVM(t, db, "vm-fw")

	// 节点级：区域 JP/SG、白名单 10.1.0.0/16。
	nodeLevel, err := svc.Effective(ctx, vm.ID)
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if nodeLevel.Source != "node" || nodeLevel.Overridden {
		t.Errorf("未设置覆盖时应来自节点基线: source=%s overridden=%v",
			nodeLevel.Source, nodeLevel.Overridden)
	}
	if len(nodeLevel.GeoipRegions) != 2 {
		t.Errorf("节点级区域 = %v, 期望 2 个", nodeLevel.GeoipRegions)
	}

	// 设覆盖：只允许 US，白名单换成 10.2.0.0/16。
	over, err := svc.SetVMPolicy(ctx, vm.ID, model.FirewallDeny, "10.2.0.0/16", nil, true,
		admin(), "root", "")
	if err != nil {
		t.Fatalf("设置覆盖失败: %v", err)
	}
	if !over.Overridden || over.Source != "vm" {
		t.Errorf("应显示为虚拟机级覆盖: %+v", over)
	}
	if len(over.Whitelist) != 1 || over.Whitelist[0] != "10.2.0.0/16/32" &&
		over.Whitelist[0] != "10.2.0.0/16" {
		t.Errorf("白名单应被覆盖而不是叠加: %v", over.Whitelist)
	}
	// 区域被清空（覆盖里没设），而不是与节点级的 JP/SG 合并。
	if len(over.GeoipRegions) != 0 {
		t.Errorf("区域应被覆盖为空而不是与节点级合并: %v", over.GeoipRegions)
	}

	// 清除覆盖后回落到基线。
	back, err := svc.ClearVMPolicy(ctx, vm.ID, admin(), "root", "")
	if err != nil {
		t.Fatalf("清除覆盖失败: %v", err)
	}
	if back.Overridden || len(back.GeoipRegions) != 2 {
		t.Errorf("清除覆盖后应回落到节点基线: %+v", back)
	}
}

func TestCreateRuleOnMissingNode(t *testing.T) {
	svc, _ := newTestEnv(t)

	_, err := svc.CreateRule(context.Background(), 999, firewall.RuleRequest{
		Action: model.FirewallAccept, Protocol: model.ProtocolTCP,
		PortStart: port(22), SourceCIDR: "0.0.0.0/0",
	}, admin(), "root", "")
	assertStatus(t, err, 404)
}

// --- 辅助 ---

func strPtr(s string) *string { return &s }

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
