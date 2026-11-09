package node

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/service/node"
	"k_cockpit/internal/service/platform/auth"
	"k_cockpit/internal/service/platform/risk"
	"k_cockpit/internal/service/platform/settings"
)

// Options 是节点接口的可选装配项。
type Options struct {
	// AgentAddr 是控制面 agent 网关的监听地址（AGENT_LISTEN_ADDR）。
	//
	// 注册命令里的端口从这里取，而不是写死 8443：AGENT_LISTEN_ADDR
	// 本来就是可配置项，写死会让「改成 18443 部署」的环境拿到一条
	// 连不上的安装命令——而这条命令是用来复制粘贴的，错了只能靠人猜。
	AgentAddr string
	// Settings 用于读取「站点对外地址」：从反向代理或本机访问面板时，
	// 请求的 Host 未必是 agent 能连到的地址。
	Settings *settings.Service
}

// Node 提供节点管理接口（F-6-01 / F-6-02）。
//
// 全部接口都要求管理员角色——该要求由**路由注册时声明**，而不是在
// 本文件内部判断（f-1-06 R-003，见 internal/router）。
type Node struct {
	svc  *node.Service
	risk *risk.Guard
	opts Options
}

// NewNode 构造节点接口。
func NewNode(svc *node.Service, guard *risk.Guard, opts Options) *Node {
	return &Node{svc: svc, risk: guard, opts: opts}
}

// List 返回节点列表（含运行态）。
func (h *Node) List(ctx context.Context, c *app.RequestContext) {
	views, err := h.svc.List(ctx)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, views)
}

// Get 返回节点详情。
func (h *Node) Get(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	view, err := h.svc.Get(ctx, id)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type maintenanceRequest struct {
	// Enabled 用指针区分「没传」与「传了 false」：后者是明确的**退出**
	// 维护模式，当成参数缺失忽略掉会给出一个「设置成功但什么都没变」的反馈。
	Enabled *bool  `json:"enabled"`
	Reason  string `json:"reason"`
}

// Stats 返回宿主机的实时指标（API-083 / F-6-03）。
//
// 响应里带 `at`（采集时刻）：指标是瞬时值，轮询失败时界面会继续显示上一组
// 数字，没有采集时刻就分不清「当前」与「几分钟前」。
func (h *Node) Stats(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	stats, err := h.svc.Stats(ctx, id)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, stats)
}

// SetMaintenance 进入或退出维护模式（API-042 / F-6-05）。
//
// **不需要二次验证**，与删除、移除节点这类不可逆操作不同：进入维护模式
// 是收紧（拒绝新变更），退出是恢复，两者都不会造成不可逆的结果，而且
// 随时可以反向操作。给可逆操作加验证只会稀释验证本身的分量。
func (h *Node) SetMaintenance(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req maintenanceRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if req.Enabled == nil {
		api.Fail(c, api.InvalidParameter("缺少 enabled 字段"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.SetMaintenance(ctx, id, *req.Enabled, req.Reason,
		user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

type consoleHostRequest struct {
	Host string `json:"host"`
}

// SetConsoleHost 设置节点控制台的对外地址。
//
// 它决定的只有一件事：**能不能下载 SPICE 连接文件**。控制台本身的开关与
// 监听地址不受它影响——"网页里能看"与"本地客户端能连"是两条独立的路。
func (h *Node) SetConsoleHost(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req consoleHostRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.SetConsoleHost(ctx, id, req.Host, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// Remove 移除节点。
func (h *Node) Remove(ctx context.Context, c *app.RequestContext) {
	// 高风险操作：移除节点后其上的虚拟机将失去管控（f-10-02）。
	if !h.risk.Require(c, risk.ActionNodeRemove) {
		return
	}

	id, err := api.NamedPathID(c, "id", "节点 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)
	if err := h.svc.Remove(ctx, id, user.ID, user.Username, info.IP); err != nil {
		api.Fail(c, err)
		return
	}
	api.NoContent(c)
}

type createEnrollTokenRequest struct {
	Name string `json:"name"`
	// TTLHours 为 0 时使用默认有效期（24 小时）。
	TTLHours int `json:"ttl_hours"`
}

type createEnrollTokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Node      node.View `json:"node"`

	// InstallCommand 是在目标机器上执行的 agent 注册命令。
	InstallCommand string `json:"install_command"`
}

// CreateEnrollToken 生成一次性注册令牌，并创建一条待接入的节点。
//
// 明文令牌只在本次响应中出现一次——数据库存的是它的哈希，之后无法取出。
func (h *Node) CreateEnrollToken(ctx context.Context, c *app.RequestContext) {
	var req createEnrollTokenRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	var ttl time.Duration
	if req.TTLHours > 0 {
		ttl = time.Duration(req.TTLHours) * time.Hour
	}

	result, err := h.svc.CreateEnrollToken(ctx, req.Name, ttl, user.ID, user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	resp := createEnrollTokenResponse{
		Token:     result.Token,
		ExpiresAt: result.ExpiresAt,
		Node:      result.Node,
	}

	resp.InstallCommand = fmt.Sprintf(
		"k_cockpit-agent enroll -server %s -token %s",
		h.enrollAddr(string(c.Request.Host())), result.Token,
	)

	api.Created(c, resp)
}

// enrollAddr 拼出 agent 实际可连的网关地址 host:port。
//
// 取址优先级：
//  1. 系统设置里的「站点对外地址」（basic.site_url）——反向代理或本机
//     访问时请求 Host 不等于 agent 能连到的地址，显式配置最可靠；
//  2. 请求的 Host（去掉端口）——浏览器从内网地址打开面板的常见情形；
//  3. 都拿不到时退回 127.0.0.1（仅本机 agent 可用，至少不是一条语法错误的命令）。
//
// 端口一律取 AGENT_LISTEN_ADDR 的实际端口，不写死。
func (h *Node) enrollAddr(requestHost string) string {
	port := "8443"
	if _, p, err := net.SplitHostPort(h.opts.AgentAddr); err == nil && p != "" {
		port = p
	}

	host := hostOnly(requestHost)
	if h.opts.Settings != nil {
		if site := strings.TrimSpace(h.opts.Settings.String(settings.KeySiteURL, "")); site != "" {
			if u, err := url.Parse(site); err == nil && u.Hostname() != "" {
				// 只取主机名：站点地址是**面板**地址，它的端口属于 HTTP 面，
				// 与 agent 网关端口是两个东西，混用会拼出一条连不上的命令。
				host = u.Hostname()
			}
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// hostOnly 去掉 Host 头里的端口，用于拼接 agent 网关地址。
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}
