package vm_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// seedTargetNode 建一台可用的目标节点（测试环境默认只有节点 1）。
func seedTargetNode(t *testing.T, db *gorm.DB, maintenance bool) int64 {
	t.Helper()
	row := model.Node{
		Name: "node-target", EnrollState: model.NodeEnrollEnrolled,
		MaintenanceMode: maintenance,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建目标节点失败: %v", err)
	}
	return row.ID
}

func seedMigratableVM(t *testing.T, db *gorm.DB, name string) *model.VM {
	t.Helper()
	row := model.VM{
		NodeID: 1, Name: name, Status: model.VMStatusStopped,
		DiskGB: 20, OwnerID: ptr(int64(7)), Present: true,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &row
}

// TestMigrateRejectsSameNode 覆盖最基础的一条。
func TestMigrateRejectsSameNode(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m1")
	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: 1},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
}

// TestMigrateRejectsMaintenanceTarget 覆盖一条**必须拦住**的前置条件。
//
// 维护模式的目标节点上什么都做不了（创建与电源操作都被拒绝）。迁过去之后
// 用户会发现这台机器动不了，而他会以为是迁移本身出了问题。
func TestMigrateRejectsMaintenanceTarget(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m2")
	toNode := seedTargetNode(t, db, true)

	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "维护") {
		t.Errorf("拒绝文案未说明是维护模式: %v", err)
	}
}

// TestMigrateRunningGoesLive 运行中的机器走热迁移（F-2-15）。
//
// 此前运行中一律拒绝（要求先关机）；热迁移落地后，运行状态对应 live
// 方式、停机状态对应 offline 方式，方式由实时探测决定并写进迁移记录。
func TestMigrateRunningGoesLive(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m3")
	row.Status = model.VMStatusRunning
	db.Save(row)
	toNode := seedTargetNode(t, db, false)

	task, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 7}, "alice", "")
	if err != nil {
		t.Fatalf("运行中的机器应按热迁移受理: %v", err)
	}

	var mig model.VMMigration
	if err := db.Where("vm_id = ?", row.ID).First(&mig).Error; err != nil {
		t.Fatalf("查询迁移记录失败: %v", err)
	}
	if mig.Mode != "live" {
		t.Errorf("迁移方式 = %q, 期望 live", mig.Mode)
	}
	// 任务参数必须带方式与探测状态：执行器据此向节点下发不同的迁移语义。
	if !strings.Contains(deref(task.Params), "\"mode\":\"live\"") {
		t.Errorf("任务参数缺 mode=live: %s", deref(task.Params))
	}
}

// TestMigrateRejectsPaused 暂停态两种方式都不适用：热迁移要机器在跑，
// 停机迁移要机器已停。
func TestMigrateRejectsPaused(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusPaused})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m3p")
	row.Status = model.VMStatusPaused
	db.Save(row)
	toNode := seedTargetNode(t, db, false)

	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// TestMigrateRejectsStaticIPConflict 覆盖迁移**独有**的一类前置条件。
//
// 静态地址在**节点内唯一**（`uniq_static_ip_node_ip`）。换一台宿主机就可能
// 撞上别人已经占用的地址——迁过去之后两台机器抢同一个 IP，而控制面认为
// 一切正常。
//
// 拒绝时必须**列出是哪一项冲突**：只说「有冲突」等于让用户在两个节点之间
// 逐项对照。
func TestMigrateRejectsStaticIPConflict(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m4")
	toNode := seedTargetNode(t, db, false)

	// 源侧：这台虚拟机持有 192.168.1.50。
	ip := "192.168.1.50"
	db.Create(&model.StaticIP{
		NodeID: 1, VMID: &row.ID, IP: ip, AddressFamily: model.AddressFamilyIPv4,
	})
	// 目标节点上：同一个地址已经被别人占了。
	other := int64(99)
	db.Create(&model.StaticIP{
		NodeID: toNode, VMID: &other, IP: ip, AddressFamily: model.AddressFamilyIPv4,
	})

	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), ip) {
		t.Errorf("冲突说明里应指出是哪个地址: %v", err)
	}
}

// TestMigrateRejectsPortForwardConflict 覆盖端口转发的冲突。
func TestMigrateRejectsPortForwardConflict(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m5")
	toNode := seedTargetNode(t, db, false)

	db.Create(&model.PortForward{
		NodeID: 1, VMID: &row.ID, Protocol: "tcp", HostPort: 8080, TargetPort: 80,
	})
	other := int64(99)
	db.Create(&model.PortForward{
		NodeID: toNode, VMID: &other, Protocol: "tcp", HostPort: 8080, TargetPort: 8080,
	})

	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "8080") {
		t.Errorf("冲突说明里应指出是哪个端口: %v", err)
	}
}

// TestMigrateMovesVMAndNodeScopedResources 覆盖成功路径。
//
// **节点内资源必须跟着搬**：不搬的话静态地址仍然指向旧节点，而它在目标
// 节点上没有被登记——于是「地址在节点内唯一」这条约束对新位置失效，
// 另一台机器可以分配到同一个 IP。
func TestMigrateMovesVMAndNodeScopedResources(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := seedMigratableVM(t, db, "vm-m6")
	toNode := seedTargetNode(t, db, false)

	ip := "192.168.1.60"
	db.Create(&model.StaticIP{
		NodeID: 1, VMID: &row.ID, IP: ip, AddressFamily: model.AddressFamilyIPv4,
	})
	db.Create(&model.VMInterface{
		VMID: row.ID, NodeID: 1, Order: 0, Model: model.NICModelVirtio, IsPrimary: true,
	})
	db.Create(&model.PortForward{
		NodeID: 1, VMID: &row.ID, Protocol: "tcp", HostPort: 9090, TargetPort: 22,
	})

	if _, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitForVM(t, svc, row.ID, viewer, func(v vm.View) bool { return v.NodeID == toNode })

	var moved model.VM
	db.First(&moved, row.ID)
	if moved.NodeID != toNode {
		t.Fatalf("虚拟机仍在源节点: node=%d", moved.NodeID)
	}

	// 三处节点内资源都要跟着走。
	var iface model.VMInterface
	db.Where("vm_id = ?", row.ID).First(&iface)
	if iface.NodeID != toNode {
		t.Errorf("网卡未跟随迁移: node=%d，期望 %d", iface.NodeID, toNode)
	}

	var staticIP model.StaticIP
	db.Where("ip = ?", ip).First(&staticIP)
	if staticIP.NodeID != toNode {
		t.Errorf("静态地址未跟随迁移: node=%d，期望 %d", staticIP.NodeID, toNode)
	}

	var pf model.PortForward
	db.Where("host_port = ?", 9090).First(&pf)
	if pf.NodeID != toNode {
		t.Errorf("端口转发未跟随迁移: node=%d，期望 %d", pf.NodeID, toNode)
	}

	// 迁移记录要能说清「从哪来到哪去」——vm.node_id 只说「现在在哪」。
	items, err := svc.ListMigrations(ctx, row.ID, viewer)
	if err != nil {
		t.Fatalf("查询迁移记录失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("迁移记录数 = %d, 期望 1", len(items))
	}
	if items[0].FromNodeID != 1 || items[0].ToNodeID != toNode {
		t.Errorf("迁移记录方向不对: %d → %d", items[0].FromNodeID, items[0].ToNodeID)
	}
	if items[0].Status != model.MigrationSuccess {
		t.Errorf("状态 = %q, 期望 success", items[0].Status)
	}
	// 结果要说明**跟着搬了些什么**：只给一个「成功」会让用户不确定
	// 「我原来接的网络、配的转发还在不在」。
	if items[0].Result == "" {
		t.Error("未说明跟着搬了些什么")
	}
}

// TestMigrateRejectsOthersVM 覆盖越权。
func TestMigrateRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m7")
	row.OwnerID = ptr(int64(20))
	db.Save(row)
	toNode := seedTargetNode(t, db, false)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: toNode},
		authz.Viewer{UserID: 10}, "bob", "")
	assertAPIError(t, err, 404)
}

func TestMigrateRejectsMissingTargetNode(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := seedMigratableVM(t, db, "vm-m8")

	_, err := svc.Migrate(ctx, row.ID, vm.MigrateRequest{ToNodeID: 9999},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 404)
}
