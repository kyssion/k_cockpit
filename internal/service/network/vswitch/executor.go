package vswitch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/service/ops/task"
)

// switchParams 是 vpc.switch.change 任务的参数。
type switchParams struct {
	// Action 取值 create / update / delete。
	Action     string `json:"action"`
	NodeID     int64  `json:"node_id"`
	SwitchID   int64  `json:"switch_id"`
	BridgeName string `json:"bridge_name"`
	// Spec 是**期望状态**的完整描述，而不是「改了哪几个字段」。
	//
	// 下发完整状态而不是差量：节点侧只需要实现「让这个网桥变成这样」一件事，
	// 而不必维护「收到差量后当前状态应该是什么」的推理——后者要求节点自己
	// 保存一份状态，一旦节点重启或与控制面失联，那份状态就再也对不上。
	Spec map[string]any `json:"spec"`
}

// SwitchChangeExecutor 执行 vpc.switch.change 任务。
type SwitchChangeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewSwitchChangeExecutor 构造交换机变更执行器。
func NewSwitchChangeExecutor(db *gorm.DB, client agent.Client) *SwitchChangeExecutor {
	return &SwitchChangeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *SwitchChangeExecutor) Type() string { return model.TaskVPCSwitchChange }

// Run 下发变更并按动作维护控制面记录。
//
// 顺序与存储池一致：**先让宿主机上的操作成功，再写控制面记录**。反过来的话，
// 建网桥失败时会留下一条「存在但不可用」的交换机记录，用户拿它去建虚拟机，
// 然后在某个说不清的时刻发现没有网络。
func (e *SwitchChangeExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p switchParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[network] 解析交换机变更参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	// 迁移、重配置与端口释放各有自己的操作：它们与"增删改"不是同一种动作
	// （一个搬动现有端口，一个按记录重新下发，一个回收资源），塞进
	// OpVPCSwitchChange 会让节点那边用一个 action 字段去区分四种语义完全不同的
	// 事情——那种 switch 迟早会漏掉一种。
	kind := agent.OpVPCSwitchChange
	switch p.Action {
	case "migrate":
		kind = agent.OpVpcSwitchMigrate
	case "reconfigure":
		kind = agent.OpVpcSwitchReconfigure
	case "release_port":
		kind = agent.OpVpcPortRelease
	}

	op := agent.Operation{
		Kind:   kind,
		NodeID: *t.NodeID,
		Target: p.BridgeName,
		Params: map[string]any{"action": p.Action},
	}
	if p.Spec != nil {
		op.Params["spec"] = p.Spec
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, op)
	if err != nil {
		return api.Unavailable("节点不可达，网络变更未送达")
	}
	if !result.Success {
		// 节点侧的校验比控制面更准（它看得到宿主机上真实存在的网桥与
		// 邻居），因此把它的说明原样带出去。
		return api.ValidationFailed(result.Message)
	}

	switch p.Action {
	case "create":
		return e.createRecord(ctx, &p, result)
	case "update":
		return e.updateRecord(ctx, &p, result)
	case "delete":
		return e.deleteRecord(ctx, &p)
	case "migrate":
		// 例外：迁移要写回新的上行与 VLAN，否则面板会一直显示旧网卡。
		return e.migrateRecord(ctx, &p)
	case "reconfigure", "release_port":
		// 这两个不产生新的控制面记录：重配置按现有记录重新下发，释放只是
		// 回收节点资源。为它们各写一份记录等于把"记录"变成"日志"。
		return nil
	default:
		// 受理时已经校验过动作，走到这里说明任务参数被改过或来自更早的
		// 版本——当作失败，而不是静默成功留下一条语义不明的记录。
		log.Printf("[network] 未知的交换机动作 %q task=%d", p.Action, t.ID)
		return api.Internal()
	}
}

func (e *SwitchChangeExecutor) createRecord(
	ctx context.Context, p *switchParams, result *agent.Result,
) error {
	sw := model.VpcSwitch{
		NodeID:     p.NodeID,
		Name:       specStr(p.Spec, "name"),
		Mode:       specStr(p.Spec, "mode"),
		BridgeName: p.BridgeName,
		IsSystem:   false,
		Status:     model.NetworkActive,
		VlanID:     specInt(p.Spec, "vlan_id"),
		CIDR:       specOptStr(p.Spec, "cidr"),
		GatewayIP:  specOptStr(p.Spec, "gateway_ip"),
		DHCPStart:  specOptStr(p.Spec, "dhcp_start"),
		DHCPEnd:    specOptStr(p.Spec, "dhcp_end"),
		UplinkIf:   specOptStr(p.Spec, "uplink_if"),

		// 带宽上限（G-38）：spec 里是数字，缺省 0 = 不限。
		BandwidthInMbps:  specIntDef(p.Spec, "bandwidth_in_mbps"),
		BandwidthOutMbps: specIntDef(p.Spec, "bandwidth_out_mbps"),
	}

	if err := e.db.WithContext(ctx).Create(&sw).Error; err != nil {
		if isDuplicateKey(err) {
			return e.conflictWithNodeRollback(ctx, p, result)
		}
		log.Printf("[network] 写入交换机记录失败: %v", err)
		return api.Internal()
	}

	log.Printf("[network] 已创建交换机 id=%d node=%d name=%s bridge=%s",
		sw.ID, sw.NodeID, sw.Name, sw.BridgeName)
	return nil
}

// conflictWithNodeRollback 处理「节点已建好、数据库写不进去」这种情况。
//
// 第 3 轮真机缺陷 DEF-R3-12 的形状是：软删记录占着唯一索引 → 创建任务失败 →
// 节点上那座桥留了下来，谁也不知道它的存在。因此这里在返回 409 之前先把
// 节点侧刚建的东西**逆操作**回去，让「失败」在两侧都是干净的。
//
// 关键前提：只能删**本任务创建的**桥。`ovs-vsctl --may-exist add-br` 对
// 「本来就存在」也返回成功，所以是否本任务创建由节点在结果里回报
// （agent.SwitchBridgeCreatedKey）。回报为否或缺键时一律不动——删掉一座
// 正在承载业务的桥，比留一座空桥严重得多。
func (e *SwitchChangeExecutor) conflictWithNodeRollback(
	ctx context.Context, p *switchParams, result *agent.Result,
) error {
	created := false
	if result != nil && result.Data != nil {
		created, _ = result.Data[agent.SwitchBridgeCreatedKey].(bool)
	}
	if !created {
		log.Printf("[network] 交换机记录冲突，网桥 %s 非本任务创建，不做回滚", p.BridgeName)
		return api.Conflict("同名或同 VLAN 的交换机已在节点上存在，请刷新后确认")
	}

	if err := e.rollbackBridge(ctx, p); err != nil {
		// 逆操作也失败：**不要静默吞掉**。告诉用户桥名与手工清理命令，
		// 否则残留只有知道桥名的人才找得到。
		log.Printf("[network] 回滚网桥失败 bridge=%s: %v", p.BridgeName, err)
		return api.Conflict(fmt.Sprintf(
			"同名或同 VLAN 的交换机已存在，请刷新后确认；"+
				"本次在节点上创建的网桥 %s 回滚失败（%v），"+
				"请在该节点执行 `ovs-vsctl --if-exists del-br %s` 清理",
			p.BridgeName, err, p.BridgeName))
	}
	log.Printf("[network] 交换机记录冲突，已回滚网桥 %s", p.BridgeName)
	return api.Conflict("同名或同 VLAN 的交换机已存在，请刷新后确认（本次创建的网桥已回滚）")
}

// rollbackBridge 让节点把这台交换机拆掉（与 delete 动作同一套逆操作）。
//
// 复用 OpVPCSwitchChange 的 delete 分支而不是在这里另写一串 ovs-vsctl：
// 拆桥的顺序（dnsmasq → NAT → 上行口 → 桥）只有一处维护，否则两处迟早分叉，
// 而分叉的表现是"删除留下了半座桥"。
func (e *SwitchChangeExecutor) rollbackBridge(ctx context.Context, p *switchParams) error {
	op := agent.Operation{
		Kind:   agent.OpVPCSwitchChange,
		NodeID: p.NodeID,
		Target: p.BridgeName,
		Params: map[string]any{"action": "delete"},
	}
	if p.Spec != nil {
		op.Params["spec"] = p.Spec
	}
	res, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, op)
	if err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("%s", res.Message)
	}
	return nil
}

func (e *SwitchChangeExecutor) updateRecord(
	ctx context.Context, p *switchParams, result *agent.Result,
) error {
	// 状态以 agent 返回为准；它没返回时用 active（下发已成功）。
	status := model.NetworkActive
	if s, ok := result.Data[agent.StatusDataKey].(string); ok && s != "" {
		status = s
	}

	err := e.db.WithContext(ctx).Model(&model.VpcSwitch{}).
		Where("id = ?", p.SwitchID).
		Updates(map[string]any{
			"name":               specStr(p.Spec, "name"),
			"mode":               specStr(p.Spec, "mode"),
			"vlan_id":            specInt(p.Spec, "vlan_id"),
			"cidr":               specOptStr(p.Spec, "cidr"),
			"gateway_ip":         specOptStr(p.Spec, "gateway_ip"),
			"dhcp_start":         specOptStr(p.Spec, "dhcp_start"),
			"dhcp_end":           specOptStr(p.Spec, "dhcp_end"),
			"uplink_if":          specOptStr(p.Spec, "uplink_if"),
			"bandwidth_in_mbps":  specIntDef(p.Spec, "bandwidth_in_mbps"),
			"bandwidth_out_mbps": specIntDef(p.Spec, "bandwidth_out_mbps"),
			"status":             status,
			"updated_at":         time.Now(),
		}).Error
	if err != nil {
		// 记录不在了（用户刚把它删了）不算失败：宿主机上已经改好了，
		// 报失败只会让人去重试一个已经生效的操作。
		log.Printf("[network] 更新交换机记录失败 id=%d: %v", p.SwitchID, err)
	}
	log.Printf("[network] 交换机变更完成 id=%d action=update", p.SwitchID)
	return nil
}

// migrateRecord 在迁移成功后把新的上行与 VLAN 写回记录。
//
// 必须写回：迁移改变的是"这台交换机现在挂在哪块网卡上"，而那正是控制面
// 记录里的字段。不写回的话，面板会一直显示旧的网卡，用户下次按面板上的
// 信息去排查，方向就是错的。
func (e *SwitchChangeExecutor) migrateRecord(ctx context.Context, p *switchParams) error {
	updates := map[string]any{
		"uplink_if":  specOptStr(p.Spec, "uplink_if"),
		"updated_at": time.Now(),
	}
	if v, ok := p.Spec["vlan_id"]; ok {
		updates["vlan_id"] = v
	}
	if err := e.db.WithContext(ctx).Model(&model.VpcSwitch{}).
		Where("id = ?", p.SwitchID).Updates(updates).Error; err != nil {
		log.Printf("[network] 更新交换机迁移结果失败 id=%d: %v", p.SwitchID, err)
	}
	log.Printf("[network] 交换机迁移完成 id=%d", p.SwitchID)
	return nil
}

func (e *SwitchChangeExecutor) deleteRecord(ctx context.Context, p *switchParams) error {
	// 软删除：审计与历史任务会引用它，物理删除会让这些引用悬空，
	// 事后追查「这条网络是什么时候没的」就无从谈起。
	err := e.db.WithContext(ctx).Model(&model.VpcSwitch{}).
		Where("id = ?", p.SwitchID).
		Updates(map[string]any{"deleted_at": time.Now()}).Error
	if err != nil {
		log.Printf("[network] 标记交换机已删除失败 id=%d: %v", p.SwitchID, err)
	}
	log.Printf("[network] 交换机变更完成 id=%d action=delete", p.SwitchID)
	return nil
}

// --- 参数取值辅助 ---

func specStr(spec map[string]any, key string) string {
	if v, ok := spec[key].(string); ok {
		return v
	}
	return ""
}

// specOptStr 空串返回 nil：数据库里存 NULL 而不是空串。
//
// 空串与 NULL 在这些可选项上会被分别渲染成「空的配置项」与「未配置」，
// 而用户看到「网关：」后面什么都没有时会以为配置丢了。
func specOptStr(spec map[string]any, key string) *string {
	v := specStr(spec, key)
	if v == "" {
		return nil
	}
	return &v
}

// specInt 把 JSON 里的数字转成 *int。
//
// JSON 反序列化到 any 会得到 float64，直接断言 int 会**静默失败**并返回
// nil——那意味着用户设的 VLAN ID 被丢掉了，而界面上看起来设置成功了。
// specIntDef 读取数字键，缺省返回 def（0 = 不限）。
func specIntDef(spec map[string]any, key string) int {
	if p := specInt(spec, key); p != nil {
		return *p
	}
	return 0
}

func specInt(spec map[string]any, key string) *int {
	v, ok := spec[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case float64:
		i := int(n)
		return &i
	case int:
		return &n
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return nil
		}
		iv := int(i)
		return &iv
	}
	return nil
}

// isDuplicateKey 见 service.go —— 本包共用一个判定，不再重复实现。
