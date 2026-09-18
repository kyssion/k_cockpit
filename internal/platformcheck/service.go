// Package platformcheck 实现平台自检与修复（F-4-13）。
//
// 自检与探测的根本区别：
//
//	探测回答「这台机器有没有 OVS」——它看的是**环境**。
//	自检回答「我们配的那些东西现在还在不在」——它看的是**偏差**。
//
// 后者才是用户真正需要的东西。面板上显示「端口安全已启用」，而节点上的流表
// 早被一次重启清掉了——这个状态**不会以任何形式报警**，也不会有任何界面
// 提示。自检正是去找它。
//
// 分工是刻意的：**控制面说「应该有什么」，节点说「实际有什么」**。让节点
// 自己判断应该有什么是不可能的——期望状态在控制面的库里，而节点与控制面的
// 数据库之间不该有依赖。
package platformcheck

import (
	"context"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// Service 提供自检能力。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder, now: time.Now}
}

// ClientIP 返回当前请求方的地址。
//
// 用途很具体：**宿主机防火墙的白名单**。管理员要把自己加进白名单，而
// 「我的 IP 是多少」在浏览器里看不到——让他自己去查，多半会查到一个出口
// 地址而填错。这里直接告诉他面板看到的那个地址，与白名单的实际比对口径一致。
func ClientIP(ip, forwardedFor string) map[string]any {
	out := map[string]any{"client_ip": ip}
	if forwardedFor != "" {
		out["forwarded_for"] = forwardedFor
		out["note"] = "面板看到的直接地址是 " + ip + "；X-Forwarded-For 是 " + forwardedFor +
			"。如果有反向代理，白名单应当填真实客户端地址（后者）——" +
			"填前者等于放行所有经代理过来的请求。"
	}
	return out
}

// OVSStatus 读取 OVS 运行状态。
func (s *Service) OVSStatus(ctx context.Context, nodeID int64) (*agent.OVSStatus, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpOVSStatus, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取 OVS 状态")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	if st, ok := result.Data[agent.OVSStatusKey].(agent.OVSStatus); ok {
		return &st, nil
	}
	return &agent.OVSStatus{Reason: "节点未报告 OVS 状态"}, nil
}

// OVSPorts 列出 OVS 端口。
func (s *Service) OVSPorts(ctx context.Context, nodeID int64) ([]agent.OVSPort, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpOVSPorts, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取端口列表")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	ports, _ := result.Data[agent.OVSPortsKey].([]agent.OVSPort)
	return ports, nil
}

// Leases 读取 DHCP 租约。
func (s *Service) Leases(ctx context.Context, nodeID int64) ([]agent.DHCPLease, error) {
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpDHCPLeases, NodeID: nodeID,
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取租约")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	leases, _ := result.Data[agent.DHCPLeasesKey].([]agent.DHCPLease)
	return leases, nil
}

// Check 执行一次自检，返回**偏差清单**。
func (s *Service) Check(ctx context.Context, nodeID int64) (*agent.PlatformCheck, error) {
	expectations, err := s.expectations(ctx, nodeID)
	if err != nil {
		return nil, err
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpPlatformCheck,
		NodeID: nodeID,
		Params: map[string]any{"expectations": toAnySlice(expectations)},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法自检")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	actual, _ := result.Data[agent.PlatformCheckKey].([]agent.PlatformCheckResult)
	byID := map[int64]agent.PlatformCheckResult{}
	for _, a := range actual {
		byID[a.ID] = a
	}

	out := &agent.PlatformCheck{Items: []agent.CheckItem{}}
	for _, e := range expectations {
		got := byID[e.ID]
		item := agent.CheckItem{
			Category: categoryOf(e.Kind), Target: e.Target,
			Expected: "存在", Actual: got.Actual,
			OK: got.Present, Severity: severityOf(e.Kind, got.Present),
			Fix: fixOf(e.Kind),
			// 这些项**可以**通过重新下发修复：期望状态就在我们库里。
			Repairable: true,
		}
		if item.OK {
			item.Actual = "存在"
			item.Severity = "info"
		} else {
			out.Drifts++
		}
		out.Items = append(out.Items, item)
	}

	// 网络底座单独查：它不属于「我们配的东西」，而是「环境本身」。
	if st, err := s.OVSStatus(ctx, nodeID); err == nil {
		base := ovsItems(st)
		out.Items = append(out.Items, base...)
		out.Drifts += countDrifts(base)
	}
	return out, nil
}

// expectations 从控制面记录生成期望状态。
func (s *Service) expectations(
	ctx context.Context, nodeID int64,
) ([]agent.PlatformExpectation, error) {
	out := []agent.PlatformExpectation{}

	// 端口安全：**只有标着已生效的**才期望节点上有。
	//
	// 把全部策略都拿去比对是错的：那些 pending 的本来就还没下发，报成偏差
	// 会让自检结果里充满噪声，而噪声会让人把这个功能关掉。
	var policies []model.PortSecurityPolicy
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND status = ?", nodeID, model.PortSecurityActive).
		Find(&policies).Error; err != nil {
		log.Printf("[platformcheck] 查询端口安全失败: %v", err)
		return nil, api.Internal()
	}
	for i := range policies {
		out = append(out, agent.PlatformExpectation{
			ID: policies[i].ID, Kind: "port_security", Target: policies[i].PortRef,
		})
	}

	// 公网 IP：只有**绑定中**的（released_at 为空）才期望节点上有规则。
	var bindings []model.PublicIPBinding
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND released_at IS NULL", nodeID).
		Find(&bindings).Error; err != nil {
		log.Printf("[platformcheck] 查询公网绑定失败: %v", err)
		return nil, api.Internal()
	}
	for i := range bindings {
		out = append(out, agent.PlatformExpectation{
			ID: bindings[i].ID, Kind: "public_ip",
			Target: fmt.Sprintf("binding:%d", bindings[i].ID),
		})
	}

	// 端口镜像：只有启用中的才期望存在。
	var mirrors []model.PortMirror
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND enabled = ?", nodeID, true).Find(&mirrors).Error; err != nil {
		log.Printf("[platformcheck] 查询端口镜像失败: %v", err)
		return nil, api.Internal()
	}
	for i := range mirrors {
		name := ""
		if mirrors[i].Name != nil {
			name = *mirrors[i].Name
		}
		if name == "" {
			// 没名字的镜像在自检结果里没法指向具体对象，跳过而不是显示
			// 一行空 target——那种结果用户无法据此做任何事。
			continue
		}
		out = append(out, agent.PlatformExpectation{
			ID: mirrors[i].ID, Kind: "port_mirror", Target: name,
		})
	}
	return out, nil
}

func ovsItems(st *agent.OVSStatus) []agent.CheckItem {
	if !st.Available {
		return []agent.CheckItem{{
			Category: "网络底座", Target: "Open vSwitch",
			Expected: "可用", Actual: st.Reason,
			OK: false, Severity: "warning", Fix: st.Fix,
			// **不可修复**：装 OVS 是宿主机上的操作，面板做不了。标成可修复
			// 的话，用户会点那个按钮、等一会、然后发现什么都没变。
			Repairable: false,
		}}
	}
	return []agent.CheckItem{
		{
			Category: "网络底座", Target: "Open vSwitch 服务",
			Expected: "运行中", Actual: "v" + st.Version,
			OK: st.ServiceActive, Severity: sev(st.ServiceActive, "critical"),
			// 装好了但服务挂了是最容易被忽略的一种状态：探测说「可用」，
			// 而所有依赖它的功能都不生效。
			Fix: "重启 ovs-vswitchd 服务", Repairable: true,
		},
		{
			Category: "网络底座", Target: "OpenFlow 1.3",
			Expected: "支持", Actual: yesNo(st.OpenFlow13),
			OK: st.OpenFlow13, Severity: sev(st.OpenFlow13, "critical"),
			Fix: "升级 OVS 或启用 OpenFlow 1.3", Repairable: false,
		},
		{
			Category: "网络底座", Target: "meter 表（包速率限制依赖）",
			Expected: "支持", Actual: yesNo(st.MeterAvailable),
			OK: st.MeterAvailable, Severity: sev(st.MeterAvailable, "warning"),
			Fix: "升级到支持 meter 的 OVS 版本", Repairable: false,
		},
	}
}

func countDrifts(items []agent.CheckItem) int {
	n := 0
	for _, it := range items {
		if !it.OK {
			n++
		}
	}
	return n
}

func categoryOf(kind string) string {
	switch kind {
	case "port_security":
		return "端口安全"
	case "public_ip":
		return "公网地址"
	case "port_mirror":
		return "端口镜像"
	}
	return kind
}

// severityOf 按**影响连通性与安全性的程度**分级。
//
// 一律标红的结果是用户对红色麻木——而「整个地址校验被放开」与「少了一条
// 计数规则」不该得到同样的注意力。
func severityOf(kind string, ok bool) string {
	if ok {
		return "info"
	}
	switch kind {
	case "port_security":
		// 端口安全失效会让**整台机器的地址校验放开**，那是安全边界。
		return "critical"
	case "public_ip":
		// 公网地址失效只是访问不通——而那是立刻可见的。
		return "warning"
	case "port_mirror":
		return "warning"
	}
	return "warning"
}

func fixOf(kind string) string {
	switch kind {
	case "port_security":
		return "重新下发端口安全策略"
	case "public_ip":
		return "重新下发公网地址绑定"
	case "port_mirror":
		return "重新启用端口镜像"
	}
	return "重新下发"
}

func sev(ok bool, whenBad string) string {
	if ok {
		return "info"
	}
	return whenBad
}

func yesNo(b bool) string {
	if b {
		return "支持"
	}
	return "不支持"
}

// Repair 按自检结果重新下发。
//
// 它**不是「一键变好」**：修复的能力受限于节点——缺 OVS 装不上、缺内核
// 模块也加载不了。因此审计里把这一点记下来，以免事后被读成"修复过就好"。
func (s *Service) Repair(
	ctx context.Context, nodeID int64, kinds []string,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	if len(kinds) == 0 {
		kinds = []string{"port_security", "public_ip", "port_mirror"}
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskPlatformRepair,
		NodeID:       nodeID,
		ResourceType: "platform",
		ResourceID:   nodeID,
		OwnerID:      v.UserID, CreatedBy: v.UserID,
		Params: map[string]any{"kinds": kinds},
	})
	if err != nil {
		return nil, api.Internal()
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "platform",
		Action: "platform.repair",
		Params: map[string]any{
			"kinds": kinds,
			"note":  "按控制面记录重新下发；节点环境缺失的部分（如未装 OVS）不会被修复",
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func toAnySlice(in []agent.PlatformExpectation) []any {
	out := make([]any, 0, len(in))
	for i := range in {
		out = append(out, in[i])
	}
	return out
}
