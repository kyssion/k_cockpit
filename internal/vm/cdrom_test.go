package vm_test

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// cdromEnv 造一台虚拟机与一份 ISO。
func cdromEnv(t *testing.T) (*vm.Service, *gorm.DB, int64, int64) {
	t.Helper()
	svc, q, db := newTestEnv(t)
	q.Register(vm.NewCDROMExecutor(db, agent.NewMockClient()))

	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	v := model.VM{
		Name: "vm1", NodeID: node.ID, Status: model.VMStatusStopped,
		CloneMode: model.CloneFull, OwnerID: &owner, DiskGB: 20,
	}
	db.Create(&v)

	iso := model.StorageFile{
		NodeID: node.ID, UserID: &owner, RelPath: "iso/ubuntu.iso",
		Category: model.FileCategoryISO, Filename: "ubuntu.iso", SizeBytes: 1 << 30,
	}
	db.Create(&iso)
	return svc, db, v.ID, iso.ID
}

// TestEjectKeepsDeviceRemoveDoesNot 覆盖本项最要紧的一条区分。
//
// **弹出不等于移除**：弹出之后光驱仍在，来宾里看得到一个空的托盘
// （/dev/srN 还在）；移除才是设备消失。混为一谈的话，用户在来宾里找不到
// 设备而不知道是哪一种情况——而这两种情况的排查方向完全不同。
func TestEjectKeepsDeviceRemoveDoesNot(t *testing.T) {
	svc, db, vmID, isoID := cdromEnv(t)
	ctx := context.Background()

	if _, err := svc.AttachCDROM(ctx, vmID, isoID, "sata", viewer7(), "a", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	var row model.VMCDROM
	db.Where("vm_id = ?", vmID).First(&row)
	if !row.Loaded() {
		t.Fatal("挂载后应是有盘的")
	}

	// 弹出：介质没了，但**记录还在**。
	if _, err := svc.EjectCDROM(ctx, vmID, row.ID, viewer7(), "a", ""); err != nil {
		t.Fatalf("弹出失败: %v", err)
	}
	var afterEject model.VMCDROM
	if err := db.First(&afterEject, row.ID).Error; err != nil {
		t.Fatal("弹出后光驱记录必须还在——弹出的是介质，不是设备")
	}
	if afterEject.Loaded() {
		t.Error("弹出后不该还挂着盘")
	}

	// 移除：设备消失。
	if _, err := svc.RemoveCDROM(ctx, vmID, row.ID, viewer7(), "a", ""); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	var n int64
	db.Model(&model.VMCDROM{}).Where("id = ?", row.ID).Count(&n)
	if n != 0 {
		t.Error("移除后记录应被删掉")
	}
}

// TestEjectEmptyIsRefused 覆盖「本来就是空的」。
//
// 静默成功会让用户以为刚才那次弹出生效了，而它本来就是空的——那他上一次
// 操作的结果就成了无法解释的事。
func TestEjectEmptyIsRefused(t *testing.T) {
	svc, db, vmID, _ := cdromEnv(t)
	ctx := context.Background()

	// 加一个**不带盘**的光驱。
	if _, err := svc.AttachCDROM(ctx, vmID, 0, "sata", viewer7(), "a", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	var row model.VMCDROM
	db.Where("vm_id = ?", vmID).First(&row)

	_, err := svc.EjectCDROM(ctx, vmID, row.ID, viewer7(), "a", "")
	assertStatus(t, err, 422)
}

// TestOrdersAreNotRenumbered 覆盖删除后不重排。
//
// 重排会让剩余光驱的序号前移，而来宾里 /dev/sr0 与 /dev/sr1 就对调了——
// 用户按上次记的设备名去找会找错。宁可留一个空号。
func TestOrdersAreNotRenumbered(t *testing.T) {
	svc, db, vmID, isoID := cdromEnv(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := svc.AttachCDROM(ctx, vmID, isoID, "sata", viewer7(), "a", ""); err != nil {
			t.Fatalf("第 %d 次挂载失败: %v", i, err)
		}
	}
	var rows []model.VMCDROM
	db.Where("vm_id = ?", vmID).Order("order_no").Find(&rows)
	if len(rows) != 3 {
		t.Fatalf("应有 3 个光驱，实际 %d", len(rows))
	}
	if rows[0].OrderNo != 0 || rows[1].OrderNo != 1 || rows[2].OrderNo != 2 {
		t.Fatalf("序号应从 0 连续，实际 %d/%d/%d", rows[0].OrderNo, rows[1].OrderNo, rows[2].OrderNo)
	}

	// 删掉 0 号。
	if _, err := svc.RemoveCDROM(ctx, vmID, rows[0].ID, viewer7(), "a", ""); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	rows = nil
	db.Where("vm_id = ?", vmID).Order("order_no").Find(&rows)
	if len(rows) != 2 {
		t.Fatalf("应剩 2 个，实际 %d", len(rows))
	}
	// **关键**：剩下的序号仍是 1 与 2，没有变成 0 与 1。
	if rows[0].OrderNo != 1 || rows[1].OrderNo != 2 {
		t.Errorf("删除后不该重排：期望 1/2，实际 %d/%d", rows[0].OrderNo, rows[1].OrderNo)
	}

	// 新加的应当是 3（max+1），而不是与现有的撞上。
	if _, err := svc.AttachCDROM(ctx, vmID, isoID, "sata", viewer7(), "a", ""); err != nil {
		t.Fatalf("新增失败: %v", err)
	}
	var newest model.VMCDROM
	db.Where("vm_id = ?", vmID).Order("order_no DESC").First(&newest)
	if newest.OrderNo != 3 {
		t.Errorf("新增的序号应为 max+1=3，实际 %d（用 count 取序号会与现有的撞上）", newest.OrderNo)
	}
}

// TestOnlyISOAndSameNode 覆盖挂载前的校验。
//
// **类别校验**：把一份 qcow2 挂成光盘的话，来宾会把它当成一张坏盘，而排查
// 方向完全错。**同节点校验**：光驱是宿主机上的设备，而文件在节点的存储里；
// 跨节点的话那个路径在目标宿主机上根本不存在，而报错会是一句"文件未找到"。
func TestOnlyISOAndSameNode(t *testing.T) {
	svc, db, vmID, _ := cdromEnv(t)
	ctx := context.Background()

	owner := int64(7)
	// 一份**非 ISO** 的文件。
	disk := model.StorageFile{
		NodeID: 1, UserID: &owner, RelPath: "disk/data.qcow2",
		Category: model.FileCategoryDisk, Filename: "data.qcow2",
	}
	db.Create(&disk)
	_, err := svc.AttachCDROM(ctx, vmID, disk.ID, "sata", viewer7(), "a", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "ISO") {
		t.Errorf("报错应说明只有 ISO 可以挂载: %v", err)
	}

	// 一份**在别的节点上**的 ISO。
	otherNode := model.Node{Name: "n2", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&otherNode)
	remote := model.StorageFile{
		NodeID: otherNode.ID, UserID: &owner, RelPath: "iso/x.iso",
		Category: model.FileCategoryISO, Filename: "x.iso",
	}
	db.Create(&remote)
	_, err = svc.AttachCDROM(ctx, vmID, remote.ID, "sata", viewer7(), "a", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "节点") {
		t.Errorf("报错应说明镜像不在同一节点: %v", err)
	}
}

func TestCDROMCountIsCapped(t *testing.T) {
	svc, _, vmID, isoID := cdromEnv(t)
	ctx := context.Background()

	for i := 0; i < vm.MaxCDROMsPerVM; i++ {
		if _, err := svc.AttachCDROM(ctx, vmID, isoID, "sata", viewer7(), "a", ""); err != nil {
			t.Fatalf("第 %d 次应成功: %v", i+1, err)
		}
	}
	_, err := svc.AttachCDROM(ctx, vmID, isoID, "sata", viewer7(), "a", "")
	assertStatus(t, err, 409)
	// 理由要说出来：用户不知道上限是几个，也不知道为什么有这个上限。
	if err != nil && !strings.Contains(err.Error(), "总线") {
		t.Errorf("报错应说明与总线占用有关: %v", err)
	}
}

func TestSameBusIsRefused(t *testing.T) {
	svc, db, vmID, _ := cdromEnv(t)
	ctx := context.Background()

	if _, err := svc.AttachCDROM(ctx, vmID, 0, "sata", viewer7(), "a", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	var row model.VMCDROM
	db.Where("vm_id = ?", vmID).First(&row)

	_, err := svc.SetCDROMBus(ctx, vmID, row.ID, "sata", viewer7(), "a", "")
	assertStatus(t, err, 422)
}

func TestInvalidBusIsRefused(t *testing.T) {
	svc, _, vmID, _ := cdromEnv(t)

	_, err := svc.AttachCDROM(context.Background(), vmID, 0, "usb", viewer7(), "a", "")
	assertStatus(t, err, 400)
}
