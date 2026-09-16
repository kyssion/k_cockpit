package handler

import (
	"context"
	"log"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/websocket"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/risk"
	"k_cockpit/internal/vm"
)

// consoleWriteBuffer 是单个 WebSocket 帧的最大字节数。
//
// 64 KiB 足够承载 VNC 的画面更新，又不会在一次读取里占用过多内存——
// 控制面会同时承载多个控制台流，缓冲开大了会成倍放大内存占用。
const consoleWriteBuffer = 64 * 1024

// Console 提供虚拟机控制台接口（F-2-08）。
//
// 权限：与虚拟机详情一致（f-1-06）——管理员可访问全部，tenant 仅自己名下，
// 越权返回 404。该判定在这里通过 svc 的归属过滤完成。
type Console struct {
	svc      *vm.Service
	risk     *risk.Guard
	upgrader *websocket.HertzUpgrader
}

// NewConsole 构造控制台接口。
func NewConsole(svc *vm.Service, guard *risk.Guard) *Console {
	return &Console{
		svc:  svc,
		risk: guard,
		upgrader: &websocket.HertzUpgrader{
			// 控制台会传输画面数据，消息可能较大；同时设上限避免
			// 单个连接无限占用内存（f-2-08 §5.2 的背压要求）。
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			CheckOrigin: func(_ *app.RequestContext) bool {
				// 同源部署，且连接已通过 Cookie 鉴权；跨源请求会因
				// SameSite=Strict 拿不到会话，因此这里放行即可。
				return true
			},
		},
	}
}

// GetConfig 返回控制台配置与状态（API-030）。
func (h *Console) GetConfig(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	cfg, err := h.svc.Console(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, cfg)
}

type updateConsoleRequest struct {
	Enabled  *bool   `json:"enabled"`
	Password *string `json:"password"`
	Exposed  *bool   `json:"exposed"`
}

// Update 更新控制台配置（API-031）。
//
// 其中「对外暴露」是**高危操作**（R-004）：它会把宿主机的 VNC 端口开放到
// 网络上，等于给这台虚拟机开了一扇绕过面板的后门。因此它单独走二次验证，
// 而开启/关闭与改密不需要——那两项不改变「谁能接触到这个端口」。
func (h *Console) Update(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	var req updateConsoleRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	// 只有在请求**确实要变更暴露状态**时才要求二次验证。若无论请求内容
	// 都要求验证，用户改个密码也会被弹一次验证框，验证疲劳随之而来——
	// 而验证疲劳正是这套机制失效的开端。
	if req.Exposed != nil {
		if !h.risk.Require(c, risk.ActionConsoleExpose) {
			return
		}
		ctx = vm.WithExposureVerified(ctx)
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	cfg, err := h.svc.UpdateConsole(ctx, id, vm.ConsoleUpdate{
		Enabled:  req.Enabled,
		Password: req.Password,
		Exposed:  req.Exposed,
	}, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, cfg)
}

// Screenshot 返回控制台截帧（API-032）。
//
// 当前**尚未实现**：截帧需要 agent 侧的图形导出能力，而这一能力还没有
// 契约。这里返回明确的 503 而不是一张占位图——占位图会让前端以为通路
// 已经打通，把一个「还没做」误认为「做完了但有 bug」。
func (h *Console) Screenshot(_ context.Context, c *app.RequestContext) {
	api.Fail(c, api.Unavailable("节点尚未提供控制台截帧能力"))
}

// WS 建立控制台 WebSocket 连接（API-033）。
//
// 三件事的顺序很关键：
//  1. **升级前**完成鉴权与授权（R-003）——升级之后就没有 HTTP 状态码
//     可用了，那时再发现「这台虚拟机不是你的」已经没有合适的表达方式；
//  2. 先向 agent 打开流，再开始转发——反过来的话，浏览器会先看到连接
//     建立、然后永远黑屏；
//  3. 无论哪一侧先结束，都要关闭另一侧并释放会话（R-007）。
func (h *Console) WS(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	user := auth.CurrentUser(c)
	if user == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return
	}

	stream, session, err := h.svc.OpenConsole(ctx, id, user.ID, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	var upgraded bool
	defer func() {
		// 会话必须在**升级失败**时也释放：否则一次失败的连接会永久占用
		// 一个会话名额，用户反复重试直到撞上上限，而界面上看不到任何
		// 正在使用的会话。
		if !upgraded {
			h.svc.CloseConsole(session.ID)
		}
	}()

	err = h.upgrader.Upgrade(c, func(conn *websocket.Conn) {
		upgraded = true
		defer conn.Close()
		// 会话与流都要释放（R-007）。放在这里而不是 Upgrade 返回之后：
		// Upgrade 会在连接存续期间阻塞，返回时连接已经结束。
		defer h.svc.CloseConsole(session.ID)
		defer stream.Close()

		// 控制台流量**不记内容**（R-013）：只记会话的建立与结束。
		// 控制台里可能有用户输入的凭据，写进日志等于把它们扩散到
		// 日志系统，而日志的访问面比业务数据宽得多。
		log.Printf("[console] 会话建立 vm=%d user=%d session=%s", id, user.ID, session.ID)
		pipeConsole(conn, stream)
		log.Printf("[console] 会话结束 vm=%d session=%s", id, session.ID)
	})
	if err != nil {
		log.Printf("[console] WebSocket 升级失败 vm=%d: %v", id, err)
	}
}

// pipeConsole 在 WebSocket 与 agent 流之间双向转发。
//
// 任一侧结束即返回：VNC 是有状态的交互协议，半开连接没有意义——
// 留着它只会占用资源，而用户看到的是一幅不再更新的画面。
func pipeConsole(conn *websocket.Conn, stream agent.Stream) {
	done := make(chan struct{}, 2)

	// 浏览器 → 节点
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			// 只转发二进制帧：VNC 是二进制协议，文本帧不是它的一部分。
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := stream.Write(data); err != nil {
				return
			}
		}
	}()

	// 节点 → 浏览器
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, consoleWriteBuffer)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	<-done
	_ = conn.Close()
	_ = stream.Close()
}
