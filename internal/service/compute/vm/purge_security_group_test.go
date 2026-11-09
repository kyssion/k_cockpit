package vm_test

import (
	"context"
	"encoding/json"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/compute/vm"
)

// TestPurgeClearsInterfaceSecurityGroups 覆盖「VM 彻底删除后安全组关联清不掉」。
//
// 实机日志：`SQL logic error: no such column: vm_id` +
// `DELETE FROM interface_security_group WHERE vm_id = 35`。
// 这张表没有 vm_id 列——它挂在网口上（interface_id），因此按 vm_id 删永远
// 失败，关联行变成孤儿；安全组的「生效规则 / 挂载网卡数」会把指向已删网口
// 的关联一起算进去。
func TestPurgeClearsInterfaceSecurityGroups(t *testing.T) {
	_, _, db := newTestEnvWithClient(t, newStubClient())
	ctx := context.Background()

	owner := int64(7)
	vmRow := model.VM{NodeID: 1, Name: "purge-vm", Status: model.VMStatusStopped,
		OwnerID: &owner, DiskGB: 20, Present: true}
	if err := db.Create(&vmRow).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	// 两个安全组 + 两块网卡各挂一个。
	var groupIDs []int64
	for _, name := range []string{"sg-a", "sg-b"} {
		g := model.SecurityGroup{NodeID: 1, Name: name}
		if err := db.Create(&g).Error; err != nil {
			t.Fatalf("创建安全组失败: %v", err)
		}
		groupIDs = append(groupIDs, g.ID)
	}
	for i, gid := range groupIDs {
		iface := model.VMInterface{VMID: vmRow.ID, NodeID: 1, Order: i, Model: model.NICModelVirtio}
		if err := db.Create(&iface).Error; err != nil {
			t.Fatalf("创建网口失败: %v", err)
		}
		if err := db.Create(&model.InterfaceSecurityGroup{
			InterfaceID: iface.ID, GroupID: gid,
		}).Error; err != nil {
			t.Fatalf("创建关联失败: %v", err)
		}
	}

	params, _ := json.Marshal(map[string]any{
		"vm_id": vmRow.ID, "vm_name": vmRow.Name,
		"disk_action": "keep", "purge": true,
	})
	raw := string(params)
	nodeID := int64(1)
	tk := model.Task{ID: 1, Type: model.TaskVMDelete, NodeID: &nodeID, Params: &raw}

	if err := vm.NewDeleteExecutor(db, newStubClient()).Run(ctx, &tk); err != nil {
		t.Fatalf("彻底删除失败: %v", err)
	}

	var n int64
	db.Model(&model.InterfaceSecurityGroup{}).Count(&n)
	if n != 0 {
		t.Errorf("彻底删除后仍有 %d 条网口安全组关联（应当按 interface_id 清理）", n)
	}
	// 网口与 VM 记录都该没了。
	var ifaces int64
	db.Model(&model.VMInterface{}).Where("vm_id = ?", vmRow.ID).Count(&ifaces)
	if ifaces != 0 {
		t.Errorf("网口记录残留 %d 条", ifaces)
	}
}

// TestPurgeWithoutInterfacesIsFine 覆盖没有网口的机器。
//
// `IN ()` 在部分方言里是语法错误——不能为了「总执行一条 SQL」而拼一个空
// 列表，直接跳过。
func TestPurgeWithoutInterfacesIsFine(t *testing.T) {
	_, _, db := newTestEnvWithClient(t, newStubClient())
	ctx := context.Background()

	owner := int64(7)
	vmRow := model.VM{NodeID: 1, Name: "no-nic-vm", Status: model.VMStatusStopped,
		OwnerID: &owner, DiskGB: 20, Present: true}
	if err := db.Create(&vmRow).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	params, _ := json.Marshal(map[string]any{
		"vm_id": vmRow.ID, "vm_name": vmRow.Name,
		"disk_action": "keep", "purge": true,
	})
	raw := string(params)
	nodeID := int64(1)
	tk := model.Task{ID: 2, Type: model.TaskVMDelete, NodeID: &nodeID, Params: &raw}

	if err := vm.NewDeleteExecutor(db, newStubClient()).Run(ctx, &tk); err != nil {
		t.Fatalf("无网口的机器也应能彻底删除: %v", err)
	}
	var n int64
	db.Model(&model.VM{}).Where("id = ?", vmRow.ID).Count(&n)
	if n != 0 {
		t.Error("彻底删除后 VM 记录应消失")
	}
}
