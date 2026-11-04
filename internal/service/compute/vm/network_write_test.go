package vm_test

import (
	"context"
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

func strPtr(s string) *string { return &s }
