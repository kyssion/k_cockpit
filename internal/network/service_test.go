package network_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/network"
)

// backendClient 用可控的能力上报替换 mock。
//
// 本包要验证的核心是**三态的区分**，而 mock 只上报一种固定组合——
// 用它测出来的永远只有一条路径。
type backendClient struct {
	*agent.MockClient
	backend *agent.NetworkBackend
	// unreachable 模拟探测本身失败（节点不可达）。
	unreachable bool
}

func (c *backendClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind != agent.OpNodeNetwork {
		return c.MockClient.Execute(ctx, op)
	}
	if c.unreachable {
		return nil, errors.New("dial tcp: connection refused")
	}
	return &agent.Result{
		Success: true,
		Data:    map[string]any{agent.NetworkKey: *c.backend},
	}, nil
}

func newTestEnv(t *testing.T, client agent.Client) (*network.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "network.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(&model.VpcSwitch{}, &model.Node{}); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建测试节点失败: %v", err)
	}

	return network.NewService(db, client), db
}

func fullBackend() *agent.NetworkBackend {
	return &agent.NetworkBackend{
		Mode: agent.ModeBasic,
		Capabilities: []string{
			agent.CapabilityBridgeBasic,
			agent.CapabilityDHCP,
			agent.CapabilityNAT,
		},
		Missing: map[string]string{},
	}
}

func capabilityOf(t *testing.T, view *network.StatusView, key string) network.Capability {
	t.Helper()
	for _, c := range view.Capabilities {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("能力清单中缺少 %s", key)
	return network.Capability{}
}

func TestStatusAllCapabilitiesAvailable(t *testing.T) {
	svc, _ := newTestEnv(t, &backendClient{MockClient: agent.NewMockClient(), backend: fullBackend()})

	view, err := svc.Status(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if view.ProbeFailed {
		t.Error("探测成功却标记为失败")
	}
	if view.Degraded {
		t.Error("能力齐全却标记为降级")
	}
	if view.Mode != network.ModeBasic {
		t.Errorf("模式 = %q", view.Mode)
	}
	if got := capabilityOf(t, view, agent.CapabilityDHCP); got.State != network.StateAvailable {
		t.Errorf("DHCP 状态 = %q, 期望 available", got.State)
	}
}

func TestMissingOptionalCapabilityDoesNotDegrade(t *testing.T) {
	// 缺 OVS：**不阻断服务**（R-004）。它只影响 VPC 相关的进阶能力，
	// 而「虚拟机能不能联网」不受影响。把它算作降级会让用户以为系统坏了。
	backend := fullBackend()
	backend.Missing[agent.CapabilityOVS] = "未检测到 Open vSwitch"

	svc, _ := newTestEnv(t, &backendClient{MockClient: agent.NewMockClient(), backend: backend})
	view, err := svc.Status(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	if view.Degraded {
		t.Error("缺少非必需能力不应标记为降级")
	}

	ovs := capabilityOf(t, view, agent.CapabilityOVS)
	if ovs.State != network.StateUnavailable {
		t.Errorf("OVS 状态 = %q, 期望 unavailable", ovs.State)
	}
	// 必须说清「缺什么 → 影响什么 → 怎么修」（R-011）。
	if ovs.Fix == "" {
		t.Error("缺失的能力未给出修复方式")
	}
	if len(ovs.AffectedFeatures) == 0 {
		t.Error("缺失的能力未说明影响范围")
	}
}

func TestMissingRequiredCapabilityDegrades(t *testing.T) {
	backend := fullBackend()
	// 去掉 DHCP：虚拟机拿不到 IP，这属于必须让用户知道的功能缺失。
	backend.Capabilities = []string{agent.CapabilityBridgeBasic, agent.CapabilityNAT}
	backend.Missing[agent.CapabilityDHCP] = "未检测到 dnsmasq"

	svc, _ := newTestEnv(t, &backendClient{MockClient: agent.NewMockClient(), backend: backend})
	view, err := svc.Status(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	if !view.Degraded {
		t.Error("缺少必需能力应标记为降级")
	}

	dhcp := capabilityOf(t, view, agent.CapabilityDHCP)
	if dhcp.State != network.StateUnavailable || dhcp.Fix == "" {
		t.Errorf("DHCP 能力信息不完整: %+v", dhcp)
	}
}

func TestProbeFailureIsUnknownNotUnavailable(t *testing.T) {
	// 探测失败 → 所有能力是「未知」而不是「缺失」（Q-003）。
	//
	// 这个区分是实的：缺失要装东西，未知要重试。显示成缺失会让用户去装
	// 一个其实已经装好的包——而这看起来像是「系统检测不准」，比报错更糟。
	svc, _ := newTestEnv(t, &backendClient{
		MockClient:  agent.NewMockClient(),
		unreachable: true,
	})

	view, err := svc.Status(context.Background(), 1)
	if err != nil {
		t.Fatalf("探测失败不应返回错误，而应给出「未知」状态: %v", err)
	}

	if !view.ProbeFailed {
		t.Error("未标记探测失败")
	}
	if view.ProbeMessage == "" {
		t.Error("未给出探测失败原因")
	}
	// 探测失败时不能宣称降级：我们并不知道它缺什么。
	if view.Degraded {
		t.Error("探测失败时不应断言降级")
	}

	for _, c := range view.Capabilities {
		if c.State != network.StateUnknown {
			t.Errorf("能力 %s 状态 = %q, 期望 unknown", c.Key, c.State)
		}
		// 未知时也不能给出修复方式：我们并不知道它缺什么。
		if c.Fix != "" {
			t.Errorf("能力 %s 在未知状态下给出了修复方式", c.Key)
		}
	}
}

func TestSystemNetworkIsCreatedOnce(t *testing.T) {
	client := &backendClient{MockClient: agent.NewMockClient(), backend: fullBackend()}
	svc, db := newTestEnv(t, client)
	ctx := context.Background()

	first, err := svc.Networks(ctx, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("网络数量 = %d, 期望 1", len(first))
	}
	if !first[0].IsSystem {
		t.Error("系统基础网络未标记 is_system")
	}

	// 幂等（R-008）：重复执行不得产生重复网桥。
	second, err := svc.Networks(ctx, 1)
	if err != nil {
		t.Fatalf("二次查询失败: %v", err)
	}
	if len(second) != 1 || second[0].ID != first[0].ID {
		t.Errorf("重复查询产生了新记录: %+v", second)
	}

	var count int64
	db.Model(&model.VpcSwitch{}).Where("node_id = ?", 1).Count(&count)
	if count != 1 {
		t.Errorf("库中记录数 = %d, 期望 1", count)
	}
}

func TestUnknownNodeNotFound(t *testing.T) {
	svc, _ := newTestEnv(t, &backendClient{MockClient: agent.NewMockClient(), backend: fullBackend()})

	_, err := svc.Status(context.Background(), 999)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("节点不存在应返回 404, 实际 %v", err)
	}

	_, err = svc.Networks(context.Background(), 999)
	if !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("节点不存在应返回 404, 实际 %v", err)
	}
}

func TestCapabilitiesCoverCatalog(t *testing.T) {
	// 上报了清单里没有的能力时，它应当被忽略而不是崩溃；反之，清单里的
	// 每一项都必须在结果中出现（否则前端会漏渲染某项能力）。
	backend := fullBackend()
	backend.Capabilities = append(backend.Capabilities, "network.unknown-future-thing")

	svc, _ := newTestEnv(t, &backendClient{MockClient: agent.NewMockClient(), backend: backend})
	view, err := svc.Status(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	for _, key := range []string{
		agent.CapabilityBridgeBasic,
		agent.CapabilityDHCP,
		agent.CapabilityNAT,
		agent.CapabilityOVS,
	} {
		capabilityOf(t, view, key)
	}
}
