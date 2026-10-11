// 结果数据解码：agent 回传的 Result.Data 是 JSON 对象（map[string]any 的
// 线上形状），而服务层用**类型断言**消费（如 result.Data[key].(agent.HostStats)）。
//
// 因此 hub 需要一张「操作 → 键 → 类型」的解码表。未登记的键走通用 JSON
// 解码（string/float64/bool/嵌套 any）——那是标量键的正确形状，服务层对
// 它们的断言本来就是基础类型；数字经 JSON 往返是 float64，与 mock 的
// 行为一致（mock 的 size_bytes 就是 float64）。
package hub

import (
	"encoding/json"
	"fmt"
	"reflect"

	"k_cockpit/internal/agent"
)

// decodeFunc 把一个键的原始 JSON 解码为服务层期望的具体类型。
type decodeFunc func(raw json.RawMessage) (any, error)

// typedDecoders 登记**结构体/切片**类结果的解码器，按 OpKind 分组。
//
// 键名与 agent 包各契约文件的 XxxDataKey 常量一一对应；新增操作的解码器
// 在这里登记，登记遗漏的表现是服务层断言失败——那会在对账测试里暴露，
// 而不是静默丢数据。
var typedDecoders = map[agent.OpKind]map[string]decodeFunc{
	// node 域（P1）
	agent.OpNodeStats:   {agent.NodeStatsDataKey: into(agent.NodeStats{})},
	agent.OpNodeDisks:   {agent.DiskListKey: into([]agent.Disk{})},
	agent.OpNodeNetwork: {agent.NetworkKey: into(agent.NetworkBackend{})},
	agent.OpNodeDomains: {agent.NodeDomainsDataKey: into([]agent.DomainInfo{})},
	// host 域（P1）
	agent.OpHostStats:       {agent.HostStatsDataKey: into(agent.HostStats{})},
	agent.OpHostHardware:    {agent.HostHardwareDataKey: into(agent.HostHardware{})},
	agent.OpHostNetStats:    {agent.HostNetStatsDataKey: into(agent.HostNetStats{})},
	agent.OpHostTuning:      {agent.TuningStateKey: into(agent.TuningState{})},
	agent.OpHostIOMMU:       {agent.IOMMUStatusKey: into(agent.IOMMUStatus{})},
	agent.OpHostPCIDevices:  {agent.PCIDevicesKey: into([]agent.PCIDevice{})},
	agent.OpHostConnections: {agent.HostConnectionsKey: into([]agent.HostConnection{})},
	// vm 域只读（P1）
	agent.OpVMStats:     {agent.StatsDataKey: into(agent.VMStats{})},
	agent.OpVMDiskList:  {agent.VMDiskListDataKey: into([]agent.VMDisk{})},
	agent.OpVMXML:       {agent.VMXMLKey: into(agent.VMXMLInfo{})},
	agent.OpVMPcieInfo:  {agent.PCIeDataKey: into(agent.PCIeInfo{})},
	agent.OpVMNeighbors: {agent.NeighborDataKey: into([]agent.NeighborEntry{})},
	// 网络探测（P1）
	agent.OpNetworkProbe:  {agent.NetworkCapabilityKey: into(agent.NetworkCapability{})},
	agent.OpOVSStatus:     {agent.OVSStatusKey: into(agent.OVSStatus{})},
	agent.OpOVSPorts:      {agent.OVSPortsKey: into([]agent.OVSPort{})},
	agent.OpDHCPLeases:    {agent.DHCPLeasesKey: into([]agent.DHCPLease{})},
	agent.OpPlatformCheck: {agent.PlatformCheckKey: into([]agent.PlatformCheckResult{})},
	agent.OpMigrateAssess: {agent.MigrateAssessDataKey: into(agent.MigrateAssessInfo{})},
}

// into 返回「解码进 sample 同型结构」的解码器（sample 只用于取类型）。
func into(sample any) decodeFunc {
	return func(raw json.RawMessage) (any, error) {
		v := reflect.New(reflect.TypeOf(sample)).Interface()
		if err := json.Unmarshal(raw, v); err != nil {
			return nil, err
		}
		// *T → T（切片/结构体都解引用为服务层断言期望的形状）。
		return reflect.ValueOf(v).Elem().Interface(), nil
	}
}

// decodeResultData 把线上的 JSON 对象还原为服务层可断言的 map[string]any。
func decodeResultData(kind agent.OpKind, raw string) (map[string]any, error) {
	if raw == "" || raw == "{}" {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("结果 Data 不是 JSON 对象: %w", err)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if dec := decodeFor(kind, k); dec != nil {
			val, err := dec(v)
			if err != nil {
				return nil, fmt.Errorf("键 %s 解码失败: %w", k, err)
			}
			out[k] = val
			continue
		}
		var a any
		if err := json.Unmarshal(v, &a); err != nil {
			return nil, fmt.Errorf("键 %s 不是合法 JSON: %w", k, err)
		}
		out[k] = a
	}
	return out, nil
}

// decodeFor 返回某操作某键的解码器；未登记返回 nil（调用方走通用解码）。
func decodeFor(kind agent.OpKind, key string) decodeFunc {
	if m, ok := typedDecoders[kind]; ok {
		return m[key]
	}
	return nil
}
