package network_test

import (
	"context"
	"errors"

	"k_cockpit/internal/agent"
	"testing"
	"time"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/network"
)

func validSwitch() network.SwitchRequest {
	return network.SwitchRequest{
		Name:      "prod-net",
		Mode:      model.NetworkModeNAT,
		CIDR:      "192.168.10.0/24",
		GatewayIP: "192.168.10.1",
		DHCPStart: "192.168.10.100",
		DHCPEnd:   "192.168.10.200",
	}
}

func TestCreateSwitchWritesRecordAfterNodeSucceeds(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	tk, err := svc.CreateSwitch(ctx, 1, validSwitch(), 7, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if tk.ID == 0 {
		t.Fatal("未返回任务标识")
	}

	// 记录由执行器在**节点成功之后**写入，因此要等任务落地。
	waitForSwitch(t, func() bool {
		var n int64
		db.Model(&model.VpcSwitch{}).Where("name = ?", "prod-net").Count(&n)
		return n == 1
	})

	var sw model.VpcSwitch
	if err := db.Where("name = ?", "prod-net").First(&sw).Error; err != nil {
		t.Fatalf("查询交换机失败: %v", err)
	}
	if sw.IsSystem {
		t.Error("新建的交换机被标记为系统网络")
	}
	if sw.CIDR == nil || *sw.CIDR != "192.168.10.0/24" {
		t.Errorf("网段未写入: %v", sw.CIDR)
	}

	// 网桥名有长度上限：Linux 的 IFNAMSIZ 是 16 字节（含结尾 NUL），
	// 因此接口名最多 15 个字符。超出的部分会被内核**静默截断**——
	// 控制面记着长名字、宿主机上是短名字，两边对不上且不报错。
	if len(sw.BridgeName) > 15 {
		t.Errorf("网桥名 %q 长度 %d 超过 15，会在宿主机上被静默截断",
			sw.BridgeName, len(sw.BridgeName))
	}
}

// TestBridgeNameDoesNotCollide 覆盖「前缀相同但名字不同」的情形。
//
// 用截断名字的写法（`kbr` + 前 12 个字符）会让 `production-a` 与
// `production-b` 得到同一个网桥——两个网络的广播域被悄悄合并，而现象上
// 完全看不出来。
func TestBridgeNameDoesNotCollide(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{
		"production-network-a", "production-network-b", "prod", "prod-2",
	} {
		svc, db := newTestEnv(t, agent.NewMockClient())
		req := validSwitch()
		req.Name = name
		if _, err := svc.CreateSwitch(context.Background(), 1, req, 7, "admin", ""); err != nil {
			t.Fatalf("受理 %q 失败: %v", name, err)
		}
		waitForSwitch(t, func() bool {
			var n int64
			db.Model(&model.VpcSwitch{}).Where("name = ?", name).Count(&n)
			return n == 1
		})

		var sw model.VpcSwitch
		db.Where("name = ?", name).First(&sw)
		if prev, ok := seen[sw.BridgeName]; ok {
			t.Errorf("%q 与 %q 得到同一个网桥名 %q", name, prev, sw.BridgeName)
		}
		seen[sw.BridgeName] = name
	}
}

func TestSwitchRejectsInvalidNetwork(t *testing.T) {
	svc, _ := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*network.SwitchRequest)
	}{
		{"网段格式错误", func(r *network.SwitchRequest) { r.CIDR = "192.168.10.0" }},
		{"网关不在网段内", func(r *network.SwitchRequest) { r.GatewayIP = "10.0.0.1" }},
		{"DHCP 起始不在网段内", func(r *network.SwitchRequest) { r.DHCPStart = "10.0.0.5" }},
		{"DHCP 范围颠倒", func(r *network.SwitchRequest) {
			r.DHCPStart, r.DHCPEnd = "192.168.10.200", "192.168.10.100"
		}},
		{"网段过小", func(r *network.SwitchRequest) {
			// /31 没有可用主机地址，网关与虚拟机都放不下。
			r.CIDR, r.GatewayIP = "192.168.10.0/31", "192.168.10.0"
			r.DHCPStart, r.DHCPEnd = "", ""
		}},
		{"VLAN 超出范围", func(r *network.SwitchRequest) {
			v := 4095 // 保留值
			r.VlanID = &v
		}},
		{"名称为空", func(r *network.SwitchRequest) { r.Name = "  " }},
		{"模式非法", func(r *network.SwitchRequest) { r.Mode = "magic" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validSwitch()
			tc.mutate(&req)
			_, err := svc.CreateSwitch(ctx, 1, req, 7, "admin", "")
			if err == nil {
				t.Fatal("应被拒绝")
			}
			var apiErr *api.Error
			if !asAPIError(err, &apiErr) {
				t.Fatalf("期望业务错误, 实际 %v", err)
			}
			// 400（参数不合法）或 409（重名/冲突）都是受理阶段的拒绝，
			// 关键是不能变成 500——那意味着校验漏到了数据库层。
			if apiErr.Status != 400 && apiErr.Status != 409 {
				t.Errorf("状态码 = %d, 期望 400 或 409（%s）", apiErr.Status, apiErr.Message)
			}
		})
	}
}

func TestSystemSwitchIsProtected(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	// 系统基础网络在首次列出网络时**幂等**建立，因此用读接口把它触发出来，
	// 而不是去调一个私有方法——测的是用户实际会走的路径。
	if _, err := svc.Networks(ctx, 1); err != nil {
		t.Fatalf("建立系统网络失败: %v", err)
	}
	var sys model.VpcSwitch
	if err := db.Where("node_id = ? AND is_system = ?", 1, true).First(&sys).Error; err != nil {
		t.Fatalf("未找到系统网络: %v", err)
	}

	// 改它的网段会让该节点上所有已接入的虚拟机立刻失去网络，而这件事
	// 在界面上没有任何提示——用户只是改了一个看起来普通的字段。
	req := validSwitch()
	req.Name = "renamed"
	_, err := svc.UpdateSwitch(ctx, sys.ID, req, 7, "admin", "")
	assertStatus(t, err, 422)

	_, err = svc.DeleteSwitch(ctx, sys.ID, 7, "admin", "")
	assertStatus(t, err, 422)
}

func TestDeleteSwitchRejectsAttachedInterfaces(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	if _, err := svc.CreateSwitch(ctx, 1, validSwitch(), 7, "admin", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitForSwitch(t, func() bool {
		var n int64
		db.Model(&model.VpcSwitch{}).Where("name = ?", "prod-net").Count(&n)
		return n == 1
	})
	var sw model.VpcSwitch
	db.Where("name = ?", "prod-net").First(&sw)

	if err := db.Create(&model.VMInterface{
		VMID: 1, NodeID: 1, Order: 0, Model: "virtio", SwitchID: &sw.ID,
	}).Error; err != nil {
		t.Fatalf("创建网卡失败: %v", err)
	}

	// 占用检查必须是**同步**的：排进队列等执行到它时才失败，用户会在
	// 几分钟后收到一条失败通知，而那时他多半已经去做别的事了。
	_, err := svc.DeleteSwitch(ctx, sw.ID, 7, "admin", "")
	assertStatus(t, err, 409)
}

func TestSwitchUniquePerNode(t *testing.T) {
	svc, db := newTestEnv(t, agent.NewMockClient())
	ctx := context.Background()

	if _, err := svc.CreateSwitch(ctx, 1, validSwitch(), 7, "admin", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitForSwitch(t, func() bool {
		var n int64
		db.Model(&model.VpcSwitch{}).Where("name = ?", "prod-net").Count(&n)
		return n == 1
	})

	// 同名：给出「已有同名交换机」而不是让数据库抛一句唯一约束冲突。
	_, err := svc.CreateSwitch(ctx, 1, validSwitch(), 7, "admin", "")
	assertStatus(t, err, 409)

	// 同 VLAN、不同名：同样当场拒绝。
	v := 100
	other := validSwitch()
	other.Name = "prod-net-2"
	other.VlanID = &v
	if _, err := svc.CreateSwitch(ctx, 1, other, 7, "admin", ""); err != nil {
		t.Fatalf("首次使用该 VLAN 应被接受: %v", err)
	}
	waitForSwitch(t, func() bool {
		var n int64
		db.Model(&model.VpcSwitch{}).Where("name = ?", "prod-net-2").Count(&n)
		return n == 1
	})

	third := validSwitch()
	third.Name = "prod-net-3"
	third.VlanID = &v
	_, err = svc.CreateSwitch(ctx, 1, third, 7, "admin", "")
	assertStatus(t, err, 409)
}

// --- 辅助 ---

func asAPIError(err error, target **api.Error) bool {
	return errors.As(err, target)
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

func waitForSwitch(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待交换机记录建立超时")
}
