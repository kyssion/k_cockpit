package securitygroup_test

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
	"k_cockpit/internal/securitygroup"
	"k_cockpit/internal/task"
)

func newTestEnv(t *testing.T) (*securitygroup.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "sg.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.SecurityGroup{}, &model.SecurityGroupRule{}, &model.InterfaceSecurityGroup{},
		&model.Node{}, &model.VM{}, &model.VMInterface{}, &model.VpcSwitch{},
		&model.Task{}, &model.TaskStage{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	queue.Register(securitygroup.NewApplyExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return securitygroup.NewService(db, queue, client, recorder), db
}

func admin() authz.Viewer { return authz.Viewer{UserID: 1, IsAdmin: true} }

// seedVMWithIface 建一台虚拟机与一个网口，返回 (vmID, interfaceID)。
func seedVMWithIface(t *testing.T, db *gorm.DB, name string, groupID *int64) (int64, int64) {
	t.Helper()
	owner := int64(7)
	vm := model.VM{NodeID: 1, Name: name, Status: model.VMStatusStopped, OwnerID: &owner, Present: true}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	iface := model.VMInterface{
		VMID: vm.ID, NodeID: 1, Order: 0, IsPrimary: true,
		SecurityGroupID: groupID,
	}
	if err := db.Create(&iface).Error; err != nil {
		t.Fatalf("创建网口失败: %v", err)
	}
	return vm.ID, iface.ID
}

func seedGroup(t *testing.T, svc *securitygroup.Service, name string) int64 {
	t.Helper()
	view, err := svc.CreateGroup(context.Background(), securitygroup.CreateGroupRequest{
		NodeID: 1, Name: name,
	}, admin(), "admin", "")
	if err != nil {
		t.Fatalf("创建安全组 %s 失败: %v", name, err)
	}
	return view.ID
}

func addRule(t *testing.T, svc *securitygroup.Service, groupID int64, req securitygroup.RuleRequest) {
	t.Helper()
	if _, err := svc.CreateRule(context.Background(), groupID, req, admin(), "admin", ""); err != nil {
		t.Fatalf("新增规则失败: %v", err)
	}
}

func port(n int) *int { return &n }

// TestEffectiveMergesAndDedupes 覆盖 F-4-03 的核心：**多组叠加生效**。
//
// 同一条「允许入站 22 端口」写在两个组里，生效结果就是一条——列两遍只会让
// 用户以为放行了两次。而每条规则必须**带出来源组**，否则用户看到合并结果后
// 不知道该去哪儿改，只能在 5 个组里逐个翻。
func TestEffectiveMergesAndDedupes(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	web := seedGroup(t, svc, "Web 层")
	base := seedGroup(t, svc, "基础")
	vmID, ifaceID := seedVMWithIface(t, db, "vm-merge", &web)

	// 两个组都允许入站 22。
	addRule(t, svc, web, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(22), TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	})
	addRule(t, svc, base, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(22), TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	})
	// 基础组另开一条 Web 层没有的。
	addRule(t, svc, base, securitygroup.RuleRequest{
		Direction: model.DirectionEgress, Protocol: model.ProtocolTCP,
		PortStart: port(443), TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	})

	// 把基础组作为**附加组**挂上——这就是单个外键表达不了的地方。
	if err := svc.Attach(ctx, ifaceID, base, admin(), "admin", ""); err != nil {
		t.Fatalf("挂载附加组失败: %v", err)
	}

	preview, err := svc.Effective(ctx, vmID, admin())
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}

	if len(preview.Rules) != 2 {
		t.Fatalf("生效规则 = %d 条, 期望 2（22 端口两组合并成一条 + 443 一条）: %+v",
			len(preview.Rules), preview.Rules)
	}
	if len(preview.Groups) != 2 {
		t.Errorf("参与的组 = %v, 期望 2 个", preview.Groups)
	}

	var port22 *securitygroup.EffectiveRule
	for i := range preview.Rules {
		if preview.Rules[i].PortStart != nil && *preview.Rules[i].PortStart == 22 {
			port22 = &preview.Rules[i]
		}
	}
	if port22 == nil {
		t.Fatal("未找到 22 端口的生效规则")
	}
	// 来源必须两个都列出：合并成一条之后，用户仍然要知道它受哪几个组影响。
	if len(port22.Sources) != 2 {
		t.Errorf("22 端口规则的来源 = %v, 期望同时列出两个组", port22.Sources)
	}
}

// TestApplyRequiresMatchingVersion 覆盖 F-4-04 的关键：预览与应用之间绑定版本。
//
// 用户看到预览、判断没问题、点确认，这中间可能有几十秒，而别人完全可能改了
// 其中一个组。不校验版本的话，他批准的是 A，落下去的是 B——这类问题不报错，
// 只会让某天出现一条谁也想不起来什么时候加的放行规则。
func TestApplyRequiresMatchingVersion(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	group := seedGroup(t, svc, "版本组")
	vmID, _ := seedVMWithIface(t, db, "vm-version", &group)
	addRule(t, svc, group, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(22), TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	})

	preview, err := svc.Effective(ctx, vmID, admin())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if preview.Version == "" {
		t.Fatal("预览未给出版本号")
	}

	// 拿正确版本应用应当成功。
	if _, err := svc.Apply(ctx, vmID, preview.Version, admin(), "admin", ""); err != nil {
		t.Fatalf("版本一致时应用应成功: %v", err)
	}

	// 别人改了这个组。
	addRule(t, svc, group, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(8080), TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	})

	// 旧版本必须被拒绝——直接应用会下发一套用户没看过的规则。
	_, err = svc.Apply(ctx, vmID, preview.Version, admin(), "admin", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "重新预览") {
		t.Errorf("拒绝文案应说明该怎么办: %v", err)
	}

	// 重新预览后版本变化，可以应用。
	again, err := svc.Effective(ctx, vmID, admin())
	if err != nil {
		t.Fatalf("重新预览失败: %v", err)
	}
	if again.Version == preview.Version {
		t.Error("规则变更后版本号未变化——版本校验会形同虚设")
	}
	if _, err := svc.Apply(ctx, vmID, again.Version, admin(), "admin", ""); err != nil {
		t.Errorf("重新预览后应可应用: %v", err)
	}
}

func TestApplyRejectsEmptyVersion(t *testing.T) {
	svc, db := newTestEnv(t)
	group := seedGroup(t, svc, "空版本组")
	vmID, _ := seedVMWithIface(t, db, "vm-nover", &group)

	_, err := svc.Apply(context.Background(), vmID, "", admin(), "admin", "")
	assertStatus(t, err, 400)
}

// TestICMPAndAllRejectPorts 覆盖一条「看起来有限制、实际没有」的规则。
//
// ICMP 没有端口概念，`all` 覆盖全部协议因而也不能限定端口。允许填端口会让
// 用户以为只放行了 22，实际整个协议都通着——而这种偏差不会以任何形式报错。
func TestICMPAndAllRejectPorts(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()
	group := seedGroup(t, svc, "协议组")

	for _, proto := range []string{model.ProtocolICMP, model.ProtocolAll} {
		_, err := svc.CreateRule(ctx, group, securitygroup.RuleRequest{
			Direction: model.DirectionIngress, Protocol: proto,
			PortStart:  port(22),
			TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
		}, admin(), "admin", "")
		assertStatus(t, err, 422)
	}

	// 不填端口时是合法的。
	if _, err := svc.CreateRule(ctx, group, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolICMP,
		TargetType: model.TargetCIDR, TargetValue: "0.0.0.0/0",
	}, admin(), "admin", ""); err != nil {
		t.Errorf("ICMP 不填端口应被接受: %v", err)
	}
}

func TestCIDRTargetRequiresValue(t *testing.T) {
	svc, _ := newTestEnv(t)
	group := seedGroup(t, svc, "目标组")

	// 留空表示「任意来源」在界面上很好理解，但它与「忘了填」长得一模一样。
	// 要求显式写 0.0.0.0/0 能让两者分开。
	_, err := svc.CreateRule(context.Background(), group, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(22), TargetType: model.TargetCIDR, TargetValue: "",
	}, admin(), "admin", "")
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "0.0.0.0/0") {
		t.Errorf("拒绝文案应给出正确的写法: %v", err)
	}
}

// TestDeleteGroupRejectsAttached 覆盖一条会**静默放开流量**的路径。
//
// 删掉一个正在被使用的组会放开一批机器的流量——那是一次方向与用户预期相反的
// 变更，而且不报任何错。
func TestDeleteGroupRejectsAttached(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	group := seedGroup(t, svc, "在用组")
	_, ifaceID := seedVMWithIface(t, db, "vm-attached", &group)

	err := svc.DeleteGroup(ctx, group, admin(), "admin", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "1 个网口") {
		t.Errorf("拒绝文案应给出挂载数量: %v", err)
	}

	_ = ifaceID
}

// TestSoftDeletedGroupNameCanBeReused 覆盖软删除与唯一索引的冲突。
//
// security_group 有 deleted_at，而索引建在 (node_id, name) 上且没有条件时，
// **被删除的组会永久占住名字**。用户删掉「web」后想再建一个「web」，会撞上
// 一句唯一约束冲突——而他刚刚明明把这个名字删掉了，那句报错他无法理解，
// 也无法自行解决（改名能绕过，但没人会想到问题出在一条看不见的记录上）。
func TestSoftDeletedGroupNameCanBeReused(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	first, err := svc.CreateGroup(ctx, securitygroup.CreateGroupRequest{
		NodeID: 1, Name: "web",
	}, admin(), "admin", "")
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	if err := svc.DeleteGroup(ctx, first.ID, admin(), "admin", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// 同名重建必须成功。
	if _, err := svc.CreateGroup(ctx, securitygroup.CreateGroupRequest{
		NodeID: 1, Name: "web",
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("删除后重建同名组应成功: %v", err)
	}
}

// TestDetachRejectsPrimaryGroup 覆盖一处容易误解的地方。
//
// 主组挂在 vm_interface.security_group_id 上，解除它等于把网口变成
// 「没有安全组」，而那与「不限制」不是一回事——因此单独拒绝并说明该怎么改。
func TestDetachRejectsPrimaryGroup(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	group := seedGroup(t, svc, "主组")
	_, ifaceID := seedVMWithIface(t, db, "vm-primary", &group)

	err := svc.Detach(ctx, ifaceID, group, admin(), "admin", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "主组") {
		t.Errorf("拒绝文案应说明这是主组: %v", err)
	}
}

func TestAttachRejectsPrimaryGroup(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	group := seedGroup(t, svc, "已主组")
	_, ifaceID := seedVMWithIface(t, db, "vm-dup", &group)

	// 已经是主组时不必再挂一次：生效规则取并集，重复挂载不会改变结果，
	// 却会让界面上出现两个相同的组标签。
	err := svc.Attach(ctx, ifaceID, group, admin(), "admin", "")
	assertStatus(t, err, 422)
}

// TestEffectiveWarnsWhenNoGroups 覆盖一个语义容易被误读的状态。
//
// 没有任何安全组时不返回空规则表——那看起来像「什么都不放行」，而实际含义
// 取决于宿主机的默认策略。让界面能明确显示这个状态。
func TestEffectiveWarnsWhenNoGroups(t *testing.T) {
	svc, db := newTestEnv(t)
	vmID, _ := seedVMWithIface(t, db, "vm-bare", nil)

	preview, err := svc.Effective(context.Background(), vmID, admin())
	if err != nil {
		t.Fatalf("汇总失败: %v", err)
	}
	if len(preview.Warnings) == 0 {
		t.Error("未挂载安全组时应给出提示")
	}
	if preview.Version == "" {
		t.Error("即使没有规则也应给出可用的版本号")
	}
}

func TestCrossNodeSwitchRejected(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	group := seedGroup(t, svc, "跨节点组")

	if err := db.Create(&model.Node{
		ID: 2, Name: "node-2", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	sw := model.VpcSwitch{NodeID: 2, Name: "sw-remote", BridgeName: "br-remote"}
	if err := db.Create(&sw).Error; err != nil {
		t.Fatalf("创建交换机失败: %v", err)
	}

	// 跨节点的交换机没有网络路径，规则会静默失效——那比报错更糟。
	_, err := svc.CreateRule(ctx, group, securitygroup.RuleRequest{
		Direction: model.DirectionIngress, Protocol: model.ProtocolTCP,
		PortStart: port(22), TargetType: model.TargetSwitch,
		TargetValue: strconv.FormatInt(sw.ID, 10),
	}, admin(), "admin", "")
	assertStatus(t, err, 422)
}

func TestTenantCannotSeeOthersGroup(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	owner := int64(20)
	other := model.SecurityGroup{NodeID: 1, Name: "别人的组", OwnerID: &owner}
	if err := db.Create(&other).Error; err != nil {
		t.Fatalf("创建安全组失败: %v", err)
	}

	// 404 而非 403：403 会确认「这个 ID 存在」，让租户能通过枚举推断出
	// 别人有多少安全组。
	tenant := authz.Viewer{UserID: 10}
	_, err := svc.ListRules(ctx, other.ID, tenant)
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
