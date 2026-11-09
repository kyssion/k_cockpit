package vm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

func TestBindDHCPReservationCarriesMACAndBridge(t *testing.T) {
	captured := make(chan agent.Operation, 1)
	client := &stubClient{exec: func(_ context.Context, op agent.Operation) (*agent.Result, error) {
		if op.Kind == agent.OpVMStaticIPChange {
			captured <- op
			return &agent.Result{Success: true}, nil
		}
		return &agent.Result{Success: true}, nil
	}}
	svc, _, db := newTestEnvWithClient(t, client)

	owner := int64(7)
	v := model.VM{Name: "dhcp-vm", NodeID: 1, Status: model.VMStatusStopped, OwnerID: &owner}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	sw := model.VpcSwitch{
		NodeID: 1, Name: "vpc", BridgeName: "br-vpc", Status: "active",
		DHCPStart: strPtr("10.0.0.100"), DHCPEnd: strPtr("10.0.0.200"),
	}
	if err := db.Create(&sw).Error; err != nil {
		t.Fatal(err)
	}
	mac := "52:54:00:aa:bb:01"
	order := 0
	if err := db.Create(&model.VMInterface{
		VMID: v.ID, NodeID: 1, Order: order, MAC: &mac, SwitchID: &sw.ID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	_, err := svc.BindStaticIP(context.Background(), v.ID, vm.BindStaticIPRequest{
		IP: "10.0.0.50", InterfaceOrder: &order, IsDHCPReservation: true,
	}, authz.Viewer{UserID: 7, IsAdmin: true}, "tester", "127.0.0.1")
	if err != nil {
		t.Fatalf("BindStaticIP: %v", err)
	}

	select {
	case op := <-captured:
		if op.Params["mac"] != mac || op.Params["bridge_name"] != "br-vpc" {
			t.Fatalf("下发参数缺少 MAC/网桥: %#v", op.Params)
		}
		if op.Params["dhcp_start"] != "10.0.0.100" || op.Params["dhcp_end"] != "10.0.0.200" {
			t.Fatalf("下发参数缺少 DHCP 范围: %#v", op.Params)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：未下发 vm.staticip.change")
	}
}

func TestAddPortForwardRejectsUnimplementedRegions(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &stubClient{
		exec: func(_ context.Context, _ agent.Operation) (*agent.Result, error) {
			return &agent.Result{Success: true}, nil
		},
	})
	_, err := svc.AddPortForward(context.Background(), 1, vm.AddPortForwardRequest{
		Protocol: "tcp", HostPort: 8080, TargetPort: 80, AllowedRegions: "JP",
	}, authz.Viewer{IsAdmin: true}, "tester", "127.0.0.1")
	if err == nil || !strings.Contains(err.Error(), "尚未实现") {
		t.Fatalf("未实现的地区白名单应明确拒绝，实际 err=%v", err)
	}
}

func strPtr(s string) *string { return &s }

// TestInterfaceChangeCarriesVMIdentity 覆盖「网络变更下发时域名为空」。
//
// 实机现象：新增/修改/删除网卡都失败在
// `virsh: error: Failed to get option 'domain': Option argument is empty`，
// 报给用户的文案是「无法确认虚拟机状态」——与真实原因（任务参数里没有
// 域名）毫无关系。
//
// 根因在受理侧：enqueueNetChange 只把资源 spec 当任务参数，而执行器拿
// vm_name 当发给节点的 Target。因此这里同时断言两件事：
//  1. 入队任务的 Params 里既带着原有的 spec 字段，也带着 vm_id / vm_name；
//  2. 执行器真正下发的指令里 Target 是虚拟机名（端到端串起来看才可信）。
func TestInterfaceChangeCarriesVMIdentity(t *testing.T) {
	captured := make(chan agent.Operation, 1)
	client := &stubClient{exec: func(_ context.Context, op agent.Operation) (*agent.Result, error) {
		if op.Kind == agent.OpVMInterfaceChange {
			select {
			case captured <- op:
			default: // 只留第一份，避免阻塞执行器
			}
		}
		return &agent.Result{Success: true}, nil
	}}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	v := model.VM{Name: "vm-nic-ident", NodeID: 1, Status: model.VMStatusStopped, OwnerID: &owner}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.VMInterface{
		VMID: v.ID, NodeID: 1, Order: 0, IsPrimary: true, Model: model.NICModelVirtio,
	}).Error; err != nil {
		t.Fatal(err)
	}

	tk, err := svc.AddInterface(ctx, v.ID, vm.InterfaceRequest{Model: model.NICModelE1000},
		authz.Viewer{UserID: 7, IsAdmin: true}, "tester", "127.0.0.1")
	if err != nil {
		t.Fatalf("新增网卡失败: %v", err)
	}
	if tk.Params == nil {
		t.Fatal("任务参数为空")
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(*tk.Params), &params); err != nil {
		t.Fatalf("解析任务参数失败: %v", err)
	}

	// 身份字段：缺了它执行器拿到的是空域名。
	if got, _ := params["vm_id"].(float64); int64(got) != v.ID {
		t.Errorf("任务参数 vm_id = %v, 期望 %d", params["vm_id"], v.ID)
	}
	if got, _ := params["vm_name"].(string); got != v.Name {
		t.Errorf("任务参数 vm_name = %v, 期望 %q", params["vm_name"], v.Name)
	}

	// spec 原有字段必须原样保留：补身份不能顺手把下发描述冲掉。
	if params["action"] != vm.NetActionUpdate {
		t.Errorf("下发描述 action = %v, 期望 %q", params["action"], vm.NetActionUpdate)
	}
	if params["model"] != model.NICModelE1000 {
		t.Errorf("下发描述 model = %v, 期望 %q", params["model"], model.NICModelE1000)
	}
	if _, ok := params["interface_id"]; !ok {
		t.Errorf("下发描述缺少 interface_id: %#v", params)
	}

	// 端到端：节点侧收到的 Target 必须是域名，而不是空串。
	select {
	case op := <-captured:
		if op.Target != v.Name {
			t.Errorf("下发指令的 Target = %q, 期望 %q（空串即实机上的 'domain: Option argument is empty'）",
				op.Target, v.Name)
		}
		if op.Params["action"] != vm.NetActionUpdate {
			t.Errorf("下发给节点的 action = %v, 期望 %q", op.Params["action"], vm.NetActionUpdate)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时：未下发 vm.interface.change")
	}
}
