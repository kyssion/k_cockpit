package network

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"net"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// SwitchRequest 是一次交换机的新建或修改。
type SwitchRequest struct {
	Name string
	// Mode 取值 nat / empty / physical（model.NetworkMode*）。
	Mode string
	// VlanID 可为空，表示不划 VLAN。
	VlanID    *int
	CIDR      string
	GatewayIP string
	DHCPStart string
	DHCPEnd   string
	UplinkIf  string
	// BandwidthInMbps / BandwidthOutMbps 是该交换机的总带宽上限（G-38，
	// F-4-10 的速率型维度）。0 表示不限。它约束的是**整个交换机**的合计
	// 吞吐，与网卡级的 rate_limit_mbps（单网卡）是两层不同的限制。
	BandwidthInMbps  int
	BandwidthOutMbps int
}

// maxBridgeNameLen 是宿主机上网桥名的长度上限。
//
// Linux 的 `IFNAMSIZ` 是 16 字节**含结尾 NUL**，因此接口名最多 15 个字符。
// 超出的部分会被内核**静默截断**：控制面记录 `kbr-production-network`，
// 宿主机上实际叫 `kbr-production-`，两边对不上——而且不报错，只在后续
// 按名字查找网桥时表现为「找不到」，排查要翻到宿主机上 `ip link` 才看得见。
//
// 因此这里不采用「用户输入什么就用什么」，而是由控制面生成短名。
const maxBridgeNameLen = 15

// bridgeNameFor 由交换机名与节点推导一个合法的网桥名。
//
// 用哈希后缀而不是截断名字：截断会让 `prod-a` 与 `prod-b` 在名字前缀
// 相同时**撞到同一个网桥**（截断后完全相同），而两台交换机共用一个网桥
// 是两个网络的广播域被悄悄合并——这类问题在现象上完全看不出来。
func bridgeNameFor(nodeID int64, name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(fmt.Sprintf("%d/%s", nodeID, name)))
	return fmt.Sprintf("kbr%08x", h.Sum32())
}

// CreateSwitch 新建一个虚拟交换机（F-4-02）。
//
// 走任务队列、由执行器写入记录：建网桥是宿主机上的实际操作，**先让宿主机
// 成功、再写控制面记录**。反过来的话，建网桥失败时会留下一条「存在但不可用」
// 的交换机记录，用户拿它去建虚拟机，然后在某个说不清的时刻发现没有网络。
//
// 成功时返回 `*SwitchView` 与 `*model.Task`：视图是**受理后的即时状态**
// （status 为 building），任务是后续跟踪的依据。两者都返回是因为用户在
// 界面上既需要看到「它出现了」，也需要看到「它还在建」。
func (s *Service) CreateSwitch(
	ctx context.Context, nodeID int64, req SwitchRequest,
	operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	if err := req.normalize(); err != nil {
		return nil, err
	}
	if err := req.validate(); err != nil {
		return nil, err
	}

	// 名字与 VLAN 的唯一性先查一次，给出一句能看懂的话。
	//
	// 唯一索引仍然会在写入时兜底，但让它来报错的话，用户看到的是一句
	// 「唯一约束冲突」——他得自己猜是名字重了还是 VLAN 重了。
	if err := s.ensureSwitchUnique(ctx, nodeID, 0, req); err != nil {
		return nil, err
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       nodeID,
		ResourceType: "node",
		ResourceID:   nodeID,
		ResourceName: req.Name,
		OwnerID:      operatorID,
		CreatedBy:    operatorID,
		Params: map[string]any{
			"action":  "create",
			"node_id": nodeID,
			// bridge_name 放在**顶层**而不是 spec 里：它是这次操作的目标标识
			// （与 vm 执行器里的 vm_name 同类），执行器据它设置
			// Operation.Target。放进 spec 会让同一份信息在两层里各存一份，
			// 而两处一旦不一致，节点收到的目标与记录里写的就不是同一个。
			"bridge_name": bridgeNameFor(nodeID, req.Name),
			"spec": map[string]any{
				"name":               req.Name,
				"mode":               req.Mode,
				"vlan_id":            req.VlanID,
				"cidr":               req.CIDR,
				"gateway_ip":         req.GatewayIP,
				"dhcp_start":         req.DHCPStart,
				"dhcp_end":           req.DHCPEnd,
				"uplink_if":          req.UplinkIf,
				"bandwidth_in_mbps":  req.BandwidthInMbps,
				"bandwidth_out_mbps": req.BandwidthOutMbps,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, operatorID, operatorName, clientIP, "network.switch.create",
		nodeID, req.Name, t.ID)
	return t, nil
}

// UpdateSwitch 修改一个虚拟交换机。
//
// **系统交换机不可修改**：它由 ensureSystemNetwork 幂等建立，是整个节点上
// 虚拟机连通的基础。改它的网段会让所有已接入的虚拟机立刻失去网络，而这件事
// 没有任何提示——用户只是改了一个看起来普通的字段。
func (s *Service) UpdateSwitch(
	ctx context.Context, id int64, req SwitchRequest,
	operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	sw, err := s.loadSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	if sw.IsSystem {
		return nil, api.ValidationFailed(
			"系统基础网络由系统维护，不能修改；如需隔离网络请新建一个交换机")
	}

	if err := req.normalize(); err != nil {
		return nil, err
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := s.ensureSwitchUnique(ctx, sw.NodeID, sw.ID, req); err != nil {
		return nil, err
	}

	// 网桥名**不可变**：改它等于让控制面指向一个宿主机上不存在的网桥，
	// 而原网桥会一直留在宿主机上（连同接在上面的虚拟机），成为孤儿。
	// 需要换网桥名就新建一个交换机并迁过去。
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       sw.NodeID,
		ResourceType: "node",
		ResourceID:   sw.NodeID,
		ResourceName: req.Name,
		OwnerID:      operatorID,
		CreatedBy:    operatorID,
		Params: map[string]any{
			"action":      "update",
			"switch_id":   sw.ID,
			"bridge_name": sw.BridgeName,
			"spec": map[string]any{
				"name":               req.Name,
				"mode":               req.Mode,
				"vlan_id":            req.VlanID,
				"cidr":               req.CIDR,
				"gateway_ip":         req.GatewayIP,
				"dhcp_start":         req.DHCPStart,
				"dhcp_end":           req.DHCPEnd,
				"uplink_if":          req.UplinkIf,
				"bandwidth_in_mbps":  req.BandwidthInMbps,
				"bandwidth_out_mbps": req.BandwidthOutMbps,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, operatorID, operatorName, clientIP, "network.switch.update",
		sw.NodeID, req.Name, t.ID)
	return t, nil
}

// DeleteSwitch 删除一个虚拟交换机。
//
// **占用检查是同步的**，而且必须在受理时就做：把一个还有虚拟机接入的交换机
// 排进队列，等执行到它时才失败，用户会在几分钟后收到一条失败通知，而那时
// 他多半已经去做别的事了——「几分钟前点的那一下失败了」是最难对上号的一类
// 反馈。占用是当下的确定事实，当场就能判断，没有必要推迟。
func (s *Service) DeleteSwitch(
	ctx context.Context, id int64,
	operatorID int64, operatorName, clientIP string,
) (*model.Task, error) {
	sw, err := s.loadSwitch(ctx, id)
	if err != nil {
		return nil, err
	}
	if sw.IsSystem {
		return nil, api.ValidationFailed(
			"系统基础网络由系统维护，不能删除；删除它会让该节点上所有虚拟机失去网络")
	}

	var attached int64
	err = s.db.WithContext(ctx).Model(&model.VMInterface{}).
		Where("switch_id = ?", sw.ID).Count(&attached).Error
	if err != nil {
		log.Printf("[network] 统计交换机占用失败: %v", err)
		return nil, api.Internal()
	}
	if attached > 0 {
		return nil, api.Conflict(fmt.Sprintf(
			"该交换机上仍接有 %d 块网卡，请先把它们改接到其它网络", attached))
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVPCSwitchChange,
		NodeID:       sw.NodeID,
		ResourceType: "node",
		ResourceID:   sw.NodeID,
		ResourceName: sw.Name,
		OwnerID:      operatorID,
		CreatedBy:    operatorID,
		Params: map[string]any{
			"action":      "delete",
			"switch_id":   sw.ID,
			"bridge_name": sw.BridgeName,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, operatorID, operatorName, clientIP, "network.switch.delete",
		sw.NodeID, sw.Name, t.ID)
	return t, nil
}

// normalize 去掉首尾空白，避免「名字末尾多一个空格」这类看不见的差异
// 被当成两个不同的交换机。
func (r *SwitchRequest) normalize() error {
	r.Name = strings.TrimSpace(r.Name)
	r.Mode = strings.TrimSpace(r.Mode)
	r.CIDR = strings.TrimSpace(r.CIDR)
	r.GatewayIP = strings.TrimSpace(r.GatewayIP)
	r.DHCPStart = strings.TrimSpace(r.DHCPStart)
	r.DHCPEnd = strings.TrimSpace(r.DHCPEnd)
	r.UplinkIf = strings.TrimSpace(r.UplinkIf)
	if r.Mode == "" {
		r.Mode = model.NetworkModeNAT
	}
	return nil
}

// validate 校验参数。
//
// 网段相关的校验做得比较细（网关与 DHCP 范围必须落在网段内），因为这类
// 错误在宿主机上**不会报错**：dnsmasq 照样起来，只是不发地址，而现象是
// 「虚拟机拿到了网卡却没有 IP」——从现象出发几乎不可能定位到这里。
func (r *SwitchRequest) validate() error {
	if r.Name == "" {
		return api.InvalidParameter("交换机名称不能为空")
	}
	if len([]rune(r.Name)) > 64 {
		return api.InvalidParameter("交换机名称不能超过 64 个字符")
	}

	switch r.Mode {
	case model.NetworkModeNAT, model.NetworkModeEmpty, model.NetworkModePhysical:
	default:
		return api.InvalidParameter("网络模式非法，可选 nat / empty / physical")
	}

	// 带宽上限（G-38）：0 表示不限，负数与超上限拒绝。上限取 40 Gbps
	// 的 Mbps 值——它高于当前主流单机网络的最大吞吐，只用来挡住明显的
	// 手滑输入，而不是一个业务约束。
	if r.BandwidthInMbps < 0 || r.BandwidthOutMbps < 0 {
		return api.InvalidParameter("带宽上限不能为负数（0 表示不限）")
	}
	if r.BandwidthInMbps > maxSwitchBandwidthMbps || r.BandwidthOutMbps > maxSwitchBandwidthMbps {
		return api.InvalidParameter(
			"带宽上限不能超过 " + strconv.Itoa(maxSwitchBandwidthMbps) + " Mbps")
	}

	if r.VlanID != nil && (*r.VlanID < 1 || *r.VlanID > 4094) {
		// 0 与 4095 是保留值，配上去不会报错但行为未定义。
		return api.InvalidParameter("VLAN ID 必须在 1-4094 之间")
	}

	_, ipnet, err := net.ParseCIDR(r.CIDR)
	if err != nil {
		return api.InvalidParameter("网段格式不正确，应形如 192.168.10.0/24")
	}
	if ones, _ := ipnet.Mask.Size(); ones > 30 {
		// /31 与 /32 没有可用主机地址，网关与虚拟机都放不下。
		return api.InvalidParameter("网段过小，至少需要 /30（可用地址不足以分配给虚拟机）")
	}

	if r.GatewayIP != "" && !ipnet.Contains(net.ParseIP(r.GatewayIP)) {
		return api.InvalidParameter("网关地址不在所配置的网段内")
	}
	if r.DHCPStart != "" && !ipnet.Contains(net.ParseIP(r.DHCPStart)) {
		return api.InvalidParameter("DHCP 起始地址不在所配置的网段内")
	}
	if r.DHCPEnd != "" && !ipnet.Contains(net.ParseIP(r.DHCPEnd)) {
		return api.InvalidParameter("DHCP 结束地址不在所配置的网段内")
	}
	if r.DHCPStart != "" && r.DHCPEnd != "" {
		start, end := net.ParseIP(r.DHCPStart), net.ParseIP(r.DHCPEnd)
		if start == nil || end == nil || !ipLess(start, end) {
			return api.InvalidParameter("DHCP 地址范围的结束地址必须不小于起始地址")
		}
	}
	return nil
}

// ipLess 比较两个同族 IP 的大小。
func ipLess(a, b net.IP) bool {
	if a4, b4 := a.To4(), b.To4(); a4 != nil && b4 != nil {
		return string(a4) < string(b4)
	}
	return string(a.To16()) < string(b.To16())
}

// ensureSwitchUnique 检查同节点内的名称与 VLAN 是否已被占用。
//
// excludeID 用于修改场景：改别的字段时不该被自己当前的名字挡住。
func (s *Service) ensureSwitchUnique(
	ctx context.Context, nodeID, excludeID int64, req SwitchRequest,
) error {
	// 每次都用**全新的查询**，而不是复用同一条再追加 Where。
	//
	// GORM 的 `Where` 会**累加**到同一条语句上：写成一个变量反复追加的话，
	// 第二次检查会带上第一次的条件（`name = ? AND vlan_id = ?`），于是永远
	// 匹配不到任何行——VLAN 唯一性检查会静默失效，而且它看起来是「通过」的。
	//
	// 这个坑正是被 TestSwitchUniquePerNode 抓出来的：同名那次拦住了，
	// 同 VLAN 那次放行了，而代码上两段几乎一样。
	base := func() *gorm.DB {
		q := s.db.WithContext(ctx).Model(&model.VpcSwitch{}).
			Where("node_id = ? AND deleted_at IS NULL", nodeID)
		if excludeID > 0 {
			q = q.Where("id <> ?", excludeID)
		}
		return q
	}

	var conflict int64
	if err := base().Where("name = ?", req.Name).Count(&conflict).Error; err != nil {
		log.Printf("[network] 检查交换机重名失败: %v", err)
		return api.Internal()
	}
	if conflict > 0 {
		return api.Conflict("该节点上已有同名交换机")
	}

	if req.VlanID != nil {
		if err := base().Where("vlan_id = ?", *req.VlanID).Count(&conflict).Error; err != nil {
			log.Printf("[network] 检查 VLAN 冲突失败: %v", err)
			return api.Internal()
		}
		if conflict > 0 {
			return api.Conflict(fmt.Sprintf("该节点上 VLAN %d 已被其它交换机占用", *req.VlanID))
		}
	}
	return nil
}

// loadSwitch 读取交换机，不存在时返回 404。
func (s *Service) loadSwitch(ctx context.Context, id int64) (*model.VpcSwitch, error) {
	var sw model.VpcSwitch
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&sw).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("交换机不存在")
	case err != nil:
		log.Printf("[network] 查询交换机失败: %v", err)
		return nil, api.Internal()
	}
	return &sw, nil
}

// record 写审计。
//
// 只记下「谁在什么时候对哪台节点的哪个交换机做了什么、产生了哪个任务」，
// 不复制具体变更参数——那些由执行器在任务里记录。两边各记一份只会产生
// 两份可能不一致的记录，而事后追查时无法判断该信哪一份。
func (s *Service) record(
	ctx context.Context, operatorID int64, operatorName, clientIP, action string,
	nodeID int64, name string, taskID int64,
) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		NodeID:       nodeID,
		ResourceType: "vpc_switch",
		ResourceName: name,
		Action:       action,
		Params:       map[string]any{"task_id": taskID},
		Success:      true,
		ClientIP:     clientIP,
	})
}

// maxSwitchBandwidthMbps 是交换机带宽上限的输入上限（40 Gbps）。
// 它挡的是手滑输入，不是业务约束——真实的瓶颈由链路决定。
const maxSwitchBandwidthMbps = 40000
