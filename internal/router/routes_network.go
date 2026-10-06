// routes_network 登记网络域的路由：VPC 交换机、网桥与上行口、VPC ACL、
// 安全组、双层防火墙（虚拟机级 + 宿主机级）、公网 IP、端口安全、端口
// 镜像与抓包。
//
// 挂在 /vms/:id 下的三个网络子资源（VM 级防火墙策略 / 安全组挂载 /
// 公网 IP 来宾状态）也在这里登记——handler 属于网络域，按 handler 归属
// 而不是路径前缀分文件。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	"k_cockpit/internal/handler/network"
)

func registerNetworkRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	networkHandler := network.NewNetwork(deps.Network)
	vpcACLHandler := network.NewVpcACL(deps.VpcACL)
	publicIPHandler := network.NewPublicIP(deps.PublicIP)
	sgHandler := network.NewSecurityGroup(deps.SecurityGroup)
	firewallHandler := network.NewFirewall(deps.Firewall)
	mirrorHandler := network.NewPortMirror(deps.PortMirror)
	portSecurityHandler := network.NewPortSecurity(deps.PortSecurity)
	captureHandler := network.NewCapture(deps.Capture)
	hostFirewallHandler := network.NewHostFirewall(deps.HostFirewall)
	netHandler := network.NewNetworkBridge(deps.NetworkBridge)

	{
		// 网络（F-4-01）：M2 只有只读接口——能力探测与降级说明。
		// 网络变更（建网桥、物理口入桥）属 M3 范围。
		// VPC ACL（F-4-05）：挂在**网段**上的访问控制，与安全组（挂在虚拟机
		// 网口上）并列但不同。必须先预览后应用，且应用要带回预览版本号。
		v1.GET("/vpc-acl", g.requireAuth, g.adminOnly, vpcACLHandler.List)
		v1.POST("/vpc-acl", g.requireAuth, g.adminOnly, vpcACLHandler.Create)
		v1.PUT("/vpc-acl/:id", g.requireAuth, g.adminOnly, vpcACLHandler.Update)
		v1.DELETE("/vpc-acl/:id", g.requireAuth, g.adminOnly, vpcACLHandler.Delete)
		v1.GET("/vpc-acl/preview", g.requireAuth, g.adminOnly, vpcACLHandler.Preview)
		v1.POST("/vpc-acl/apply", g.requireAuth, g.adminOnly, vpcACLHandler.Apply)

		v1.GET("/nodes/:id/network", g.requireAuth, g.adminOnly, networkHandler.Status)
		v1.GET("/nodes/:id/networks", g.requireAuth, g.adminOnly, networkHandler.Networks)

		// 虚拟交换机（F-4-02）。写操作走任务队列——建网桥是宿主机上的实际
		// 操作，接口不同步等待；记录由执行器在节点成功后写入。
		//
		// 不需要二次验证：交换机变更**可逆**（改回去即可），而它影响的是
		// 网络连通性而非数据。给可逆操作加验证只会稀释验证本身的分量。
		v1.POST("/nodes/:id/vpc-switches", g.requireAuth, g.adminOnly, networkHandler.CreateSwitch)
		v1.PATCH("/vpc-switches/:id", g.requireAuth, g.adminOnly, networkHandler.UpdateSwitch)
		// 迁移（换物理网卡 / VLAN）与重配置（按记录重新下发）。两者都走任务队列。
		v1.POST("/vpc-switches/:id/migrate", g.requireAuth, g.adminOnly, networkHandler.MigrateSwitch)
		v1.POST("/vpc-switches/:id/reconfigure", g.requireAuth, g.adminOnly, networkHandler.ReconfigureSwitch)
		// 端口释放（回收节点上的端口资源）、计数重置、IPv6 策略。
		v1.POST("/networks/ports/release", g.requireAuth, g.adminOnly, networkHandler.ReleasePort)
		v1.POST("/networks/counters/reset", g.requireAuth, g.adminOnly, networkHandler.ResetCounters)
		v1.POST("/networks/ipv6/policy", g.requireAuth, g.adminOnly, networkHandler.ApplyIPv6Policy)
		// 全局带宽总限（G-44）：把设置值下发到节点上行。
		v1.POST("/networks/global-bandwidth/apply", g.requireAuth, g.adminOnly, networkHandler.ApplyGlobalBandwidth)
		v1.DELETE("/vpc-switches/:id", g.requireAuth, g.adminOnly, networkHandler.DeleteSwitch)

		// 网络底座与自愈（F-4-01 / F-4-13）。
		//
		// Overview 与 Repair **几乎不会失败**：探测不到节点、桥列表读不出来，
		// 都以字段形式返回，整体仍是 200。这是规格里「网络配置失败不得阻断
		// 主流程」的具体实现——用户点进这个页面本来就是为了看网络出了什么
		// 问题，给他白屏等于把唯一的诊断入口也关掉了。
		v1.GET("/networks", g.requireAuth, g.adminOnly, netHandler.Overview)
		v1.GET("/networks/bridges", g.requireAuth, g.adminOnly, netHandler.List)
		v1.POST("/networks/bridges", g.requireAuth, g.adminOnly, netHandler.CreateBridge)
		v1.DELETE("/networks/bridges/:id", g.requireAuth, g.adminOnly, netHandler.DeleteBridge)
		v1.POST("/networks/bridges/:id/uplink", g.requireAuth, g.adminOnly, netHandler.AttachUplink)
		v1.POST("/networks/bridges/:id/uplink/confirm", g.requireAuth, g.adminOnly, netHandler.ConfirmUplink)
		// 摘出入口不设门槛：一个已经切断管理通道的桥要能一键摘掉。
		v1.DELETE("/networks/bridges/:id/uplink", g.requireAuth, g.adminOnly, netHandler.DetachUplink)
		v1.POST("/networks/repair", g.requireAuth, g.adminOnly, netHandler.Repair)

		// 端口镜像（F-4-09）。
		//
		// **看门狗与镜像在同一次下发里建立**，且由**节点**计时——控制面是
		// 通过网络下发指令的，而端口镜像配错恰恰会打垮网络，那时控制面自己
		// 也发不出回滚指令。一个依赖网络的保险，在它最需要起作用的时候
		// 一定不在。
		v1.GET("/port-mirrors", g.requireAuth, g.adminOnly, mirrorHandler.List)
		v1.POST("/port-mirrors", g.requireAuth, g.adminOnly, mirrorHandler.Create)
		v1.PATCH("/port-mirrors/:id", g.requireAuth, g.adminOnly, mirrorHandler.Update)
		v1.DELETE("/port-mirrors/:id", g.requireAuth, g.adminOnly, mirrorHandler.Delete)
		v1.GET("/port-mirrors/:id/precheck", g.requireAuth, g.adminOnly, mirrorHandler.Precheck)
		v1.POST("/port-mirrors/:id/enable", g.requireAuth, g.adminOnly, mirrorHandler.Enable)
		v1.POST("/port-mirrors/:id/confirm", g.requireAuth, g.adminOnly, mirrorHandler.Confirm)
		// 关闭不设门槛。见 handler 上的说明。
		v1.POST("/port-mirrors/:id/disable", g.requireAuth, g.adminOnly, mirrorHandler.Disable)

		// 防火墙（F-4-11）。双层：节点级基线 + 虚拟机级覆盖。
		//
		// 整体归 admin：它作用于宿主机，一次配错会影响该节点上所有虚拟机，
		// 而且**可能把管理员自己关在门外**——那不是租户该有的能力。
		//
		// 应用与回滚都是**同步调用节点、不经队列**（见 firewall.Service.Apply）：
		// 管理员点下按钮后立刻需要一个答复，而回滚更不能排在几十个
		// 虚拟机创建之后——那时他已经连不上面板了。
		v1.GET("/firewall/policy", g.requireAuth, g.adminOnly, firewallHandler.GetPolicy)
		v1.PATCH("/firewall/policy", g.requireAuth, g.adminOnly, firewallHandler.UpdatePolicy)
		v1.GET("/firewall/rules", g.requireAuth, g.adminOnly, firewallHandler.ListRules)
		v1.POST("/firewall/rules", g.requireAuth, g.adminOnly, firewallHandler.CreateRule)
		v1.DELETE("/firewall/rules/:ruleID", g.requireAuth, g.adminOnly, firewallHandler.DeleteRule)
		v1.GET("/firewall/precheck", g.requireAuth, g.adminOnly, firewallHandler.Precheck)
		v1.POST("/firewall/apply", g.requireAuth, g.adminOnly, firewallHandler.Apply)
		// 紧急回滚不设任何门槛。见 handler 上的说明。
		v1.POST("/firewall/rollback", g.requireAuth, g.adminOnly, firewallHandler.Rollback)

		v1.GET("/vms/:id/firewall", g.requireAuth, g.adminOnly, firewallHandler.GetVMPolicy)
		v1.PUT("/vms/:id/firewall", g.requireAuth, g.adminOnly, firewallHandler.SetVMPolicy)
		v1.DELETE("/vms/:id/firewall", g.requireAuth, g.adminOnly, firewallHandler.ClearVMPolicy)

		// 宿主机防火墙（F-4-11 第一层）。
		//
		// **与 /firewall/* 是两套**：这里保护的是**宿主机自己与面板**（SSH、
		// 面板端口），而 /firewall/* 管的是虚拟机的入站流量。它们的配置项
		// 长得几乎一样，而攻击面完全不同——混在一处会让用户以为改了 KVM
		// 规则就关掉了面板的暴露面。
		//
		// 回滚**不需要二次验证**：它要在"已经出事了"的那一刻还能用。应用
		// 之后连不上面板或 SSH 断了，而用户又进不去宿主机时，这是唯一的
		// 自救入口。
		v1.GET("/host-firewall", g.requireAuth, g.adminOnly, hostFirewallHandler.Get)
		v1.PATCH("/host-firewall/policy", g.requireAuth, g.adminOnly, hostFirewallHandler.UpdatePolicy)
		v1.POST("/host-firewall/rules", g.requireAuth, g.adminOnly, hostFirewallHandler.CreateRule)
		v1.DELETE("/host-firewall/rules/:id", g.requireAuth, g.adminOnly, hostFirewallHandler.DeleteRule)
		v1.GET("/host-firewall/precheck", g.requireAuth, g.adminOnly, hostFirewallHandler.Precheck)
		v1.POST("/host-firewall/apply", g.requireAuth, g.adminOnly, hostFirewallHandler.Apply)
		v1.POST("/host-firewall/rollback", g.requireAuth, g.adminOnly, hostFirewallHandler.Rollback)
		v1.GET("/host-firewall/connections", g.requireAuth, g.adminOnly, hostFirewallHandler.Connections)
		v1.POST("/host-firewall/connections/close", g.requireAuth, g.adminOnly, hostFirewallHandler.CloseConnection)

		// 端口安全（F-4-08）。
		//
		// 归管理员：它会改虚拟机所在**二层网段**的连通性——一个租户给自己
		// 开端口隔离，影响的是同网段所有机器（包括别人的），而"同网段"
		// 这件事在界面上看不出来，租户无法判断自己会波及谁。
		//
		// 启用走**预检 → 确认**：同网段失联是不可从界面直接看出来的后果，
		// 而能力是否具备更不能等到点下"启用"才告诉用户。
		v1.GET("/port-security", g.requireAuth, g.adminOnly, portSecurityHandler.List)
		v1.POST("/port-security/preview", g.requireAuth, g.adminOnly, portSecurityHandler.Preview)
		v1.POST("/port-security", g.requireAuth, g.adminOnly, portSecurityHandler.Apply)
		v1.DELETE("/port-security/:id", g.requireAuth, g.adminOnly, portSecurityHandler.Disable)

		// 抓包与网络诊断（F-4-12）。
		//
		// 抓包会读到一个网口上的**全部流量**（包括明文密码与会话令牌），
		// 因此：租户只能看与删自己发起的（列表在服务层按 created_by 过滤），
		// 而每一次抓包都进审计。
		v1.GET("/captures", g.requireAuth, captureHandler.List)
		v1.POST("/captures", g.requireAuth, captureHandler.Start)
		v1.GET("/captures/:id/file", g.requireAuth, captureHandler.Download)
		v1.DELETE("/captures/:id", g.requireAuth, captureHandler.Delete)

		// 安全组（F-4-03）与 ACL 汇总应用（F-4-04）。
		//
		// **组内只有允许规则**：叠加生效意味着生效规则是各组的并集，而在
		// 并集模型里「拒绝」没法定义——A 组拒绝 22、B 组允许 22，合并后通不通
		// 取决于谁先算，而那是用户看不见的实现细节。默认拒绝由组的整体语义
		// 给出：不在任何允许规则里的流量一律不通。
		v1.GET("/security-groups", g.requireAuth, sgHandler.ListGroups)
		v1.POST("/security-groups", g.requireAuth, sgHandler.CreateGroup)
		v1.DELETE("/security-groups/:id", g.requireAuth, sgHandler.DeleteGroup)
		v1.GET("/security-groups/:id/rules", g.requireAuth, sgHandler.ListRules)
		v1.POST("/security-groups/:id/rules", g.requireAuth, sgHandler.CreateRule)
		v1.PATCH("/security-groups/:id/rules/:ruleID", g.requireAuth, sgHandler.UpdateRule)
		v1.DELETE("/security-groups/:id/rules/:ruleID", g.requireAuth, sgHandler.DeleteRule)
		v1.POST("/security-groups/:id/interfaces/:interfaceID", g.requireAuth, sgHandler.Attach)
		v1.DELETE("/security-groups/:id/interfaces/:interfaceID", g.requireAuth, sgHandler.Detach)

		// 生效规则的汇总与下发（F-4-04）。应用必须带回预览版本号：
		// 用户确认的必须正是他看到的那一套规则。
		v1.GET("/vms/:id/security-groups/effective", g.requireAuth, sgHandler.Effective)
		v1.POST("/vms/:id/security-groups/apply", g.requireAuth, sgHandler.Apply)

		// 公网 IP（F-4-06）。
		//
		// 地址池是**节点级资源**，整体归 admin（与节点、存储池同一档）。
		// 「谁能绑哪个地址」属于配额范畴（f-1-08 把公网 IP 列为配额维度），
		// 那是后续的事——先让地址能录进来、能绑上去。
		//
		// 绑定、解绑、迁移都不需要二次验证：它们都不会造成不可逆的结果，
		// 而连通性中断是**立刻可见**的——用户马上就知道发生了什么。
		v1.GET("/public-ips", g.requireAuth, g.adminOnly, publicIPHandler.List)
		v1.POST("/public-ips", g.requireAuth, g.adminOnly, publicIPHandler.Create)
		// 批量操作。**必须注册在带 :id 的路由之前**，否则 `batch` 会被
		// 当成一个 id 去匹配 `/public-ips/:id/bind`。
		//
		// 它们逐条调用单条方法，复用全部校验——在批量里重写一遍的话两处
		// 迟早分叉，而分叉的表现是「单个能解绑、批量里解不了」。
		v1.POST("/public-ips/batch/bind", g.requireAuth, g.adminOnly, publicIPHandler.BatchBind)
		v1.POST("/public-ips/batch/unbind", g.requireAuth, g.adminOnly, publicIPHandler.BatchUnbind)
		v1.DELETE("/public-ips/:id", g.requireAuth, g.adminOnly, publicIPHandler.Delete)
		v1.GET("/public-ips/:id/preview", g.requireAuth, g.adminOnly, publicIPHandler.Preview)
		v1.POST("/public-ips/:id/bind", g.requireAuth, g.adminOnly, publicIPHandler.Bind)
		v1.POST("/public-ips/:id/migrate", g.requireAuth, g.adminOnly, publicIPHandler.Migrate)
		v1.DELETE("/public-ips/:id/bind", g.requireAuth, g.adminOnly, publicIPHandler.Unbind)
		// 公网 IP 的三项运维能力（F-4-06）：前缀检测、规则重载、来宾地址状态。
		//
		// 前两项挂在 /public-ips 下（按节点），第三项按虚拟机查询——它回答的是
		// "绑定成功之后为什么还是不通"。
		v1.GET("/public-ips/ipv6-prefixes", g.requireAuth, g.adminOnly, publicIPHandler.DetectIPv6Prefixes)
		v1.POST("/public-ips/reload", g.requireAuth, g.adminOnly, publicIPHandler.ReloadRules)
		v1.GET("/vms/:id/public-ips/guest-status", g.requireAuth, publicIPHandler.GuestStatus)
	}
}
