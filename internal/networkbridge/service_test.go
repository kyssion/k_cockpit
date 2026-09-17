package networkbridge_test

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
	"k_cockpit/internal/networkbridge"
)

// deadAgent 让所有下发都失败，用于验证「失败是数据，不是异常」。
type deadAgent struct{ agent.Client }

func (deadAgent) Execute(context.Context, agent.Operation) (*agent.Result, error) {
	return nil, errors.New("node unreachable")
}

func newTestEnv(t *testing.T, client agent.Client) (*networkbridge.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "net.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.NetworkBridge{}, &model.Node{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return networkbridge.NewService(db, client, audit.NewRecorder(db)), db
}

func admin() authz.Viewer { return authz.Viewer{UserID: 1, IsAdmin: true} }

func seedBridge(t *testing.T, db *gorm.DB, name string, system bool, backend string) *model.NetworkBridge {
	t.Helper()
	row := model.NetworkBridge{
		NodeID: 1, Name: name, Backend: backend,
		Mode: model.BridgeModeIsolated, IsSystem: system, Status: model.BridgeActive,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建网络失败: %v", err)
	}
	return &row
}

// TestOverviewNeverBlocksOnProbeFailure 是本模块最核心的一条。
//
// 规格要求「网络配置失败不阻断主流程」。它的意思是：网络出问题时，面板
// **仍然要能打开**——而用户点进这个页面，本来就是为了看网络出了什么问题。
// 如果这里给他一个白屏或 500，他连「网络坏了」这个结论都拿不到。
func TestOverviewNeverBlocksOnProbeFailure(t *testing.T) {
	svc, _ := newTestEnv(t, deadAgent{})

	view, err := svc.Overview(context.Background(), 1)
	if err != nil {
		t.Fatalf("探测失败不该让整个接口失败: %v", err)
	}
	if view.ProbeOK {
		t.Error("探测失败时 ProbeOK 应为 false")
	}
	if view.ProbeError == "" {
		t.Error("应给出可读的失败原因，而不是让界面显示一个空白")
	}
	if view.Capability != nil {
		t.Error("探测失败时不该给出能力数据——那会让界面把「不知道」显示成「什么都没有」")
	}
	// 桥列表仍然要能读到：数据库没坏。
	if view.BridgesError != "" {
		t.Errorf("桥列表不该受影响: %s", view.BridgesError)
	}
}

// TestProbeFailureDoesNotClaimDegraded 覆盖「不知道」与「没有」的区别。
//
// 探测失败时**不宣称降级**：把两者混为一谈会让用户去修一个并不存在的问题
// （他的节点上可能本来就有 OVS，只是面板连不上它）。
func TestProbeFailureDoesNotClaimDegraded(t *testing.T) {
	svc, _ := newTestEnv(t, deadAgent{})

	view, err := svc.Overview(context.Background(), 1)
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if view.Degraded {
		t.Error("探测不到能力时不该宣称「已降级」——那是把「不知道」当成了「没有」")
	}
	if len(view.DegradedReasons) != 0 {
		t.Errorf("同样不该给出降级说明: %v", view.DegradedReasons)
	}
}

// TestDegradationNamesAffectedBridges 覆盖「必须说明影响了什么」。
//
// f-4-01 明确要求不能在能力缺失时静默失败。一句「已降级」对用户没有任何
// 可操作性——他需要知道**具体是哪几张网络不能用了**。
func TestDegradationNamesAffectedBridges(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	seedBridge(t, db, "ovs-net", false, model.BackendOVS)
	seedBridge(t, db, "plain-net", false, model.BackendBridge)

	view, err := svc.Overview(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !view.ProbeOK {
		t.Fatal("mock 探测应成功")
	}
	if view.Capability.OVSAvailable {
		t.Fatal("mock 里 OVS 不可用，用例假设不成立")
	}
	if !view.Degraded {
		t.Fatal("OVS 缺失时应判为降级")
	}

	joined := strings.Join(view.DegradedReasons, " | ")
	if !strings.Contains(joined, "ovs-net") {
		t.Errorf("降级说明应**具体指出**受影响的网络: %s", joined)
	}
	if strings.Contains(joined, "plain-net") {
		t.Errorf("不该把不依赖 OVS 的网络也算进受影响范围: %s", joined)
	}
}

// TestUplinkRequiresAcknowledge 覆盖物理口入桥的第一道防线。
func TestUplinkRequiresAcknowledge(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	// mock 的候选里 eth0 有 IP——那正是最危险的那一种。
	bridge := seedBridge(t, db, "b1", false, model.BackendBridge)

	result, err := svc.AttachUplink(context.Background(), bridge.ID, "eth0", 300, false,
		admin(), "root", "")
	if err != nil {
		t.Fatalf("未确认时应返回结果而不是报错——那是一个岔路口，不是一次失败: %v", err)
	}
	if result.Applied {
		t.Error("有未确认的警告时不该下发")
	}
	if len(result.Warnings) == 0 {
		t.Error("应把警告原样返回")
	}
	if !strings.Contains(strings.Join(result.Warnings, " | "), "IP") {
		t.Errorf("应指出该口上已有 IP（很可能是管理口）: %v", result.Warnings)
	}
}

func TestUplinkUnknownPortWarns(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	bridge := seedBridge(t, db, "b2", false, model.BackendBridge)

	result, err := svc.AttachUplink(context.Background(), bridge.ID, "nosuch0", 300, false,
		admin(), "root", "")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if result.Applied {
		t.Error("未知端口应被警告拦住")
	}
	if !strings.Contains(strings.Join(result.Warnings, " | "), "nosuch0") {
		t.Errorf("应指出找不到该端口: %v", result.Warnings)
	}
}

// TestUplinkArmsWatchdog 覆盖第二道防线：到点自动回滚。
func TestUplinkArmsWatchdog(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	bridge := seedBridge(t, db, "b3", false, model.BackendBridge)

	result, err := svc.AttachUplink(context.Background(), bridge.ID, "eth1", 120, true,
		admin(), "root", "")
	if err != nil {
		t.Fatalf("入桥失败: %v", err)
	}
	if !result.Applied {
		t.Fatal("应已下发")
	}
	if result.WatchdogSeconds != 120 {
		t.Errorf("窗口 = %d, 期望 120", result.WatchdogSeconds)
	}
	if result.Bridge == nil || !result.Bridge.AwaitingUplinkConfirm {
		t.Error("应处于等待确认窗口里")
	}
	if result.Bridge.UplinkSecondsLeft <= 0 {
		t.Error("剩余秒数应由服务端算好下发")
	}
}

func TestUplinkWatchdogBounds(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())

	for i, seconds := range []int{10, 59, 1801} {
		b := seedBridge(t, db, "w"+strconv.Itoa(i), false, model.BackendBridge)
		_, err := svc.AttachUplink(context.Background(), b.ID, "eth1", seconds, true,
			admin(), "root", "")
		assertStatus(t, err, 400)
	}
}

// TestReconcileAlignsExpiredUplink 覆盖控制面与节点的分工。
func TestReconcileAlignsExpiredUplink(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	bridge := seedBridge(t, db, "b4", false, model.BackendBridge)

	if _, err := svc.AttachUplink(context.Background(), bridge.ID, "eth1", 300, true,
		admin(), "root", ""); err != nil {
		t.Fatalf("入桥失败: %v", err)
	}
	if err := db.Model(&model.NetworkBridge{}).Where("id = ?", bridge.ID).
		Update("uplink_watchdog_until", time.Now().Add(-time.Minute)).Error; err != nil {
		t.Fatalf("改窗口时刻失败: %v", err)
	}

	n, err := svc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if n != 1 {
		t.Errorf("对齐了 %d 条, 期望 1", n)
	}

	items, _ := svc.ListBridges(context.Background(), 1)
	if items[0].UplinkIf != "" {
		t.Error("窗口到期后应把入桥记录一起清掉——节点侧已经摘出来了")
	}
}

// TestSystemBridgeCannotBeDeleted 覆盖一处不可恢复的后果。
//
// 新建虚拟机默认接的就是系统预置网络，删掉等于让「新建」从可用变成不可用，
// 而用户不会立刻把这两件事联系起来。
func TestSystemBridgeCannotBeDeleted(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	sys := seedBridge(t, db, "default", true, model.BackendBridge)

	err := svc.DeleteBridge(context.Background(), sys.ID, admin(), "root", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "系统预置") {
		t.Errorf("拒绝文案应说明原因: %v", err)
	}

	// 非系统预置的可以删——只测前者拦不住一个「全都删不掉」的实现。
	normal := seedBridge(t, db, "temp", false, model.BackendBridge)
	if err := svc.DeleteBridge(context.Background(), normal.ID, admin(), "root", ""); err != nil {
		t.Errorf("普通网络应可删除: %v", err)
	}
}

// TestRepairSeparatesFixedAndRemaining 覆盖修复结果的呈现。
//
// 只报告「修好了什么」会让用户以为没事了；只报告「还有问题」又看不出这次
// 操作做了什么。两者必须分开。
func TestRepairSeparatesFixedAndRemaining(t *testing.T) {
	svc, _ := newTestEnv(t, agent.NewMockClient())

	result, err := svc.Repair(context.Background(), 1, admin(), "root", "")
	if err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	if len(result.Fixed) == 0 {
		t.Error("应报告本次修好了什么")
	}
	if len(result.Remaining) == 0 {
		t.Error("应报告修复之后仍然存在的问题")
	}
}

// TestRepairFailureIsDataNotError 覆盖「用户点修复时网络已经坏了」这个场景。
//
// 修复本身失败时**不返回 error**，而是写进 Remaining——此时给他一个 500
// 对他没有任何帮助。
func TestRepairFailureIsDataNotError(t *testing.T) {
	svc, _ := newTestEnv(t, deadAgent{})

	result, err := svc.Repair(context.Background(), 1, admin(), "root", "")
	if err != nil {
		t.Fatalf("修复失败不该让接口也失败: %v", err)
	}
	if result.ProbeOK {
		t.Error("探测不成功时应如实标注")
	}
	if len(result.Remaining) == 0 {
		t.Error("应把「节点不可达」写进待处理项")
	}
	if result.Message == "" {
		t.Error("应给出可读的说明")
	}
}

func TestICMPNATBridgeRequiresDHCPFields(t *testing.T) {
	svc, _ := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	// 启用内置 DHCP 但不给地址池：DHCP 要告诉来客网关是谁，
	// 没有它就出不了网。
	_, err := svc.CreateBridge(ctx, networkbridge.BridgeRequest{
		NodeID: 1, Name: "bad", Mode: model.BridgeModeNAT, DHCPEnabled: true,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

func TestIsolatedBridgeRejectsDHCP(t *testing.T) {
	svc, _ := newTestEnv(t, agent.NewMockClient())

	// 空交换机不接外网，内置 DHCP 无从谈起。
	_, err := svc.CreateBridge(context.Background(), networkbridge.BridgeRequest{
		NodeID: 1, Name: "iso", Mode: model.BridgeModeIsolated, DHCPEnabled: true,
	}, admin(), "root", "")
	assertStatus(t, err, 422)
}

// TestCreateFailureKeepsRecord 覆盖一次「失败不算完」的处理。
//
// 下发失败时**不回滚记录**，而是把状态标成 error 并带上原因：记录留着，
// 用户才能看到「这条网络建了但没起来」，然后去「修复」；删掉记录只会让
// 那次失败凭空消失。
func TestCreateFailureKeepsRecord(t *testing.T) {
	svc, db := newTestEnv(t, deadAgent{})

	view, err := svc.CreateBridge(context.Background(), networkbridge.BridgeRequest{
		NodeID: 1, Name: "orphan", Mode: model.BridgeModeIsolated,
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("创建不该整体失败: %v", err)
	}
	if view.Status != model.BridgeError {
		t.Errorf("状态 = %q, 期望 error", view.Status)
	}
	if view.Detail == "" {
		t.Error("必须带上具体原因——只说「出错」用户唯一的动作是重试")
	}

	var n int64
	db.Model(&model.NetworkBridge{}).Where("name = ?", "orphan").Count(&n)
	if n != 1 {
		t.Error("下发失败时记录不该被删掉")
	}
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
