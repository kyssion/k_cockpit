package network

import "testing"

// TestToPSRequest 覆盖「HTTP 请求 → 服务层请求」的字段映射。
//
// 端口安全会改变同网段所有机器的连通性，任何一个开关漏传都会让管理员
// 以为自己开了防护、实际下发的却是零值（全关）。
func TestToPSRequest(t *testing.T) {
	in := portSecurityRequest{
		PortRef: "vnet0", SwitchID: 3, VMID: 9,
		SpoofingGuard: true, Isolation: true, PPSLimit: 1200,
	}
	got := toPSRequest(7, in)
	if got.NodeID != 7 {
		t.Errorf("NodeID = %d", got.NodeID)
	}
	if got.PortRef != "vnet0" || got.SwitchID != 3 || got.VMID != 9 {
		t.Errorf("资源定位字段不对: %+v", got)
	}
	if !got.SpoofingGuard || !got.Isolation || got.PPSLimit != 1200 {
		t.Errorf("策略字段丢失: %+v", got)
	}
}
