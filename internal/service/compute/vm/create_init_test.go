package vm_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// createInitClient 在通用桩之上补一个"节点回传主网卡 MAC"的行为。
type createInitClient struct {
	*stubClient
	ops []agent.Operation
}

func (c *createInitClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	res, err := c.stubClient.Execute(ctx, op)
	c.ops = append(c.ops, op)
	if op.Kind == agent.OpVMCreate && res != nil {
		res.Data[agent.CreatePrimaryMACKey] = "52:54:00:11:22:33"
	}
	return res, err
}

func (c *createInitClient) opOf(kind agent.OpKind) (agent.Operation, bool) {
	for _, op := range c.ops {
		if op.Kind == kind {
			return op, true
		}
	}
	return agent.Operation{}, false
}

// TestCreateAppliesInitializationAndStaticIP 覆盖 F-2-17 与创建期静态地址：
// 初始化参数必须下发到节点，MAC 与静态租约必须真正落库。
func TestCreateAppliesInitializationAndStaticIP(t *testing.T) {
	client := &createInitClient{stubClient: newStubClient()}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "init-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		InitMode: "nocloud", Hostname: "init-host",
		InitialPassword: "s3cret1", StaticIP: "192.168.122.50",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	waitFor(t, "创建任务完成", func() bool {
		var count int64
		db.Model(&model.VM{}).Where("name = ?", "init-vm").Count(&count)
		return count == 1
	})

	var got model.VM
	if err := db.Where("name = ?", "init-vm").First(&got).Error; err != nil {
		t.Fatalf("查询虚拟机失败: %v", err)
	}
	if got.InitMode != "nocloud" {
		t.Errorf("InitMode = %q, 期望 nocloud", got.InitMode)
	}

	createOp, ok := client.opOf(agent.OpVMCreate)
	if !ok {
		t.Fatal("没有下发创建指令")
	}
	for key, want := range map[string]string{
		"init_mode":        "nocloud",
		"hostname":         "init-host",
		"initial_password": "s3cret1",
		"initial_username": "root",
		"static_ip":        "192.168.122.50",
	} {
		if v, _ := createOp.Params[key].(string); v != want {
			t.Errorf("创建参数 %s = %q, 期望 %q", key, v, want)
		}
	}

	// MAC 从节点回传后应写进主网卡记录。
	var nic model.VMInterface
	if err := db.Where("vm_id = ? AND \"order\" = ?", got.ID, 0).First(&nic).Error; err != nil {
		t.Fatalf("查询主网卡失败: %v", err)
	}
	if nic.MAC == nil || *nic.MAC != "52:54:00:11:22:33" {
		t.Errorf("主网卡 MAC 未回写: %v", nic.MAC)
	}

	// 静态地址落成 DHCP 静态租约，且节点确认下发后才记 applied_at。
	var sip model.StaticIP
	if err := db.Where("vm_id = ?", got.ID).First(&sip).Error; err != nil {
		t.Fatalf("创建期静态地址未落库: %v", err)
	}
	if sip.IP != "192.168.122.50" || !sip.IsDHCPReservation {
		t.Errorf("静态地址记录不对: %+v", sip)
	}
	if sip.AppliedAt == nil {
		t.Error("节点确认后未记录生效时间")
	}

	bind, ok := client.opOf(agent.OpVMStaticIPChange)
	if !ok {
		t.Fatal("没有下发 DHCP 静态租约")
	}
	if v, _ := bind.Params["mac"].(string); v != "52:54:00:11:22:33" {
		t.Errorf("租约 MAC = %q", v)
	}
	if v, _ := bind.Params["bridge_name"].(string); v == "" {
		t.Error("租约缺少网桥名")
	}
}
