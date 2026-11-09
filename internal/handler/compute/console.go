package compute

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/hertz-contrib/websocket"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/platform/auth"
	"k_cockpit/internal/service/platform/risk"
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
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	cfg, err := h.svc.Console(ctx, id, c.Query("protocol"), authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, cfg)
}

type updateConsoleRequest struct {
	// Protocol 选择要配置哪一种控制台（vnc / spice），留空按 VNC。
	Protocol string  `json:"protocol"`
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
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
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
		// 协议只决定落库的字段名。**二次验证在下面那一段里对两种协议是
		// 同一处代码**（risk.ActionConsoleExpose）——分成两个接口的话，
		// 某天给 VNC 加了更严的限制，而 SPICE 那条还开着。
		Protocol: req.Protocol,
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

// ConnectionFile 下载 SPICE 连接文件（.vv）。
//
// 只有 SPICE 有标准的连接文件；VNC 在浏览器里由 noVNC 承载，而它没有
// 跨客户端通用的文件格式。
//
// 密码是**可选**的：不请求时生成的文件不含密码（用户手输），请求时必须
// 已经完成二次验证——控制台密码平时只写不读（R-005），让它以明文离开
// 服务端需要一次明确的动作。
func (h *Console) ConnectionFile(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	withPassword := c.Query("with_password") == "true"
	if withPassword {
		if !h.risk.Require(c, risk.ActionConsoleConnectionFile) {
			return
		}
	}

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	file, err := h.svc.ConsoleConnectionFile(ctx, id, c.Query("protocol"), withPassword,
		authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}

	c.Response.Header.Set("Content-Type", "application/x-virt-viewer; charset=utf-8")
	c.Response.Header.Set("Content-Disposition", `attachment; filename="`+file.Filename+`"`)
	// delete-this-file=1 已经写进文件内容，这里再标一次只是为了让中间的
	// 下载管理器也不要把它留在"下载"目录里——那是一个含凭据的文件。
	c.Response.Header.Set("Cache-Control", "no-store")
	c.String(200, file.Content)
}

// Screenshot 返回控制台截帧（API-032）。
//
// 与 API-069（预览帧）是同一个节点操作的两档：预览卡轮询缩略图，这里按需
// 返回全幅画面。响应语义与预览帧一致——直接回 image/png 字节，no-store
// （画面是「此刻的样子」，缓存住就成了旧照片）。
func (h *Console) Screenshot(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}

	frame, err := h.svc.ConsoleScreenshot(ctx, id, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}

	c.Header("Cache-Control", "no-store")
	c.SetContentType(frame.MIME)
	c.Response.SetBody(frame.Data)
}

// consoleStartGrace 是「升级成功后等待 hijack 回调被调度」的宽限期。
//
// hertz 在 handler 返回、响应刷出**之后**才调用 hijack 回调（http1/server.go
// 的 `HijackConnHandle`）。正常路径下这段间隔是微秒级；但如果响应写入失败
// （客户端在收到 101 之前就断开），hertz 会在 Flush 出错处直接 return，
// **回调永远不会被调用**，会话就永久占着一个名额（R-007 / R-014）。用一个
// 到期回收兜住这条窄路径，比让会话静默泄漏更划算。
const consoleStartGrace = 15 * time.Second

// consoleLease 保证一次控制台会话的释放**恰好发生一次**（R-007 / R-014）。
//
// 释放有三条来路：升级失败、连接结束、以及「升级成功但 hijack 回调从未被
// 调度」的超时兜底。三者可能互相竞争，必须幂等——重复释放虽然不会让计数
// 错乱（sessionRegistry.close 本身就是幂等的），但会让"会话到底何时结束"
// 这个排查第一线索失真。
type consoleLease struct {
	vmID      int64
	sessionID string
	once      sync.Once
	releaseFn func()
}

func (l *consoleLease) release(reason string) bool {
	ran := false
	l.once.Do(func() {
		ran = true
		log.Printf("[console] 释放会话 vm=%d session=%s 原因=%s", l.vmID, l.sessionID, reason)
		l.releaseFn()
	})
	return ran
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
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
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

	// 会话与流必须**恰好**释放一次，而且释放时机要绑定到**连接**的生命周期。
	//
	// 这里原先是一个 `upgraded bool`：在 Upgrade 返回后 `if !upgraded` 就释放。
	// 那是错的——Upgrade 只把回调登记进 ctx.Hijack 就返回（不阻塞），真正的
	// 转发要等 handler 返回、101 刷出之后才开始。于是那个标志永远是 false，
	// **每一次成功升级都会立刻把刚建立的会话关掉**：active_sessions 恒为 0、
	// 会话上限（R-014）形同虚设、可以无限并发开控制台。
	lease := &consoleLease{
		vmID:      id,
		sessionID: session.ID,
		releaseFn: func() {
			h.svc.CloseConsole(session.ID)
			_ = stream.Close()
		},
	}

	// 回调是否已被调度。见 consoleStartGrace 的说明：那条路径上回调永不执行，
	// 只能靠超时把会话收回来。
	started := make(chan struct{})
	var startedOnce sync.Once
	go func() {
		select {
		case <-started:
		case <-time.After(consoleStartGrace):
			lease.release("升级后 hijack 回调未被调度")
		}
	}()

	err = h.upgrader.Upgrade(c, func(conn *websocket.Conn) {
		startedOnce.Do(func() { close(started) })

		// **连接结束即会话结束**：正常路径下释放只发生在这里。
		defer func() {
			lease.release("连接结束")
			log.Printf("[console] 会话结束 vm=%d session=%s", id, session.ID)
		}()
		defer conn.Close()

		// 控制台流量**不记内容**（R-013）：只记会话的建立与结束。
		// 控制台里可能有用户输入的凭据，写进日志等于把它们扩散到
		// 日志系统，而日志的访问面比业务数据宽得多。
		log.Printf("[console] 升级成功，开始转发 vm=%d user=%d session=%s", id, user.ID, session.ID)
		pipeConsole(conn, stream)
	})
	if err != nil {
		// 握手失败：回调不会被调度，会话必须在这里释放，否则一次失败的
		// 连接会永久占用一个会话名额，用户反复重试直到撞上上限，而界面上
		// 看不到任何正在使用的会话。
		log.Printf("[console] WebSocket 升级失败 vm=%d: %v", id, err)
		lease.release("升级失败")
	}
}

// consoleConn 是 pipeConsole 对 WebSocket 连接提出的全部需求。
//
// 抽成接口而不是直接收 *websocket.Conn，是为了让转发逻辑能在**没有真实
// 网络连接**的条件下被测试：下面两个转发协程跑在 HTTP handler 的 goroutine
// 之外，api.Recover() 覆盖不到它们，一旦 panic 就是整进程退出。这条恢复
// 路径必须被单测覆盖，而用真实连接无法稳定地制造 panic。
type consoleConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// pipeConsole 在 WebSocket 与 agent 流之间双向转发。
//
// 任一侧结束即返回：VNC 是有状态的交互协议，半开连接没有意义——
// 留着它只会占用资源，而用户看到的是一幅不再更新的画面。
//
// 两个转发协程各自带 panic 兜底。原因是这里的 panic 一旦逃逸就会终结整个
// 控制面进程：api.Recover() 只包住 HTTP handler 所在的 goroutine，子协程
// 里的 panic 不在它的覆盖范围内。而浏览器关标签页／刷新会让 hijack 连接在
// 对端直接关闭，hertz-contrib/websocket 随后会在 Read 里对已失效的底层
// 连接解引用（conn.go:376 的 c.br.Peek），于是**用户关个页面就能打挂整个
// 控制面**。连接已经断了，正确反应是让这一侧安静地结束转发。
func pipeConsole(conn consoleConn, stream agent.Stream) {
	done := make(chan struct{}, 2)

	// 浏览器 → 节点
	go func() {
		// 两个 defer 的声明顺序有意如此：defer 是后进先出，所以下面这个
		// recover 会先执行、把 panic 兜住，再轮到上面那个把 done 发出去。
		// 这样无论正常返回还是 panic 返回，done 都**恰好**被发一次——
		// 发两次会写满缓冲通道并让主协程等不到第二个信号；发零次（让
		// panic 直接跳过发送）会让主协程永久阻塞在 <-done 上。
		defer func() { done <- struct{}{} }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[console] 浏览器→节点转发协程 panic（已兜住）: %v", r)
			}
		}()
		var upstream int // 已转发给节点的字节数（用于判断链路是否真的通了）
		for {
			messageType, data, err := conn.ReadMessage()
			if err != nil {
				log.Printf("[console] 浏览器→节点 结束: ReadMessage err=%v；已转发 %d 字节", err, upstream)
				return
			}
			// 只转发二进制帧：VNC 是二进制协议，文本帧不是它的一部分。
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, err := stream.Write(data); err != nil {
				log.Printf("[console] 浏览器→节点 结束: stream.Write err=%v；已转发 %d 字节", err, upstream)
				return
			}
			upstream += len(data)
		}
	}()

	// 节点 → 浏览器
	go func() {
		// 同上的两段 defer：先兜 panic，再发 done。
		defer func() { done <- struct{}{} }()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[console] 节点→浏览器转发协程 panic（已兜住）: %v", r)
			}
		}()
		buf := make([]byte, consoleWriteBuffer)
		var (
			downstream int // 已写给浏览器的字节数
			frames     int // 已写出的帧数
		)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					log.Printf("[console] 节点→浏览器 结束: WriteMessage err=%v；已送出 %d 帧/%d 字节",
						werr, frames, downstream)
					return
				}
				frames++
				downstream += n
			}
			if err != nil {
				// 这一行是「浏览器黑屏」的第一现场：它说明节点侧一个字节都
				// 没送来（downstream==0 且 n==0），问题在 agent 流而不在 WS。
				log.Printf("[console] 节点→浏览器 结束: stream.Read err=%v (n=%d)；本次已送出 %d 帧/%d 字节",
					err, n, frames, downstream)
				return
			}
		}
	}()

	<-done
	_ = conn.Close()
	_ = stream.Close()
}

// XML 返回虚拟机的 libvirt 定义（API-036）。
//
// **只读**。它是排查「面板显示的和实际跑的不是一回事」时唯一的真相——
// 而那个场景里用户最需要的恰恰是一份**没被缓存过**的读数，因此服务层每次都
// 向节点现读。
//
// 返回的内容**已脱敏**：libvirt 的定义里有控制台密码，原样送出等于把界面上
// 特意「只写不读」的那些凭据从后门送出去。被替换掉的字段名一并返回，好让
// 用户知道那里的 `[已脱敏]` 是本来的内容而不是面板读错了。
//
// `live` 参数区分运行中与持久定义：热插拔一块盘之后两份会不同，而用户需要
// 知道自己在看哪一份——拿运行时的定义去判断「重启后还在不在」会出错。
func (h *Console) XML(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	live := c.Query("live") == "true"
	view, err := h.svc.XML(ctx, id, c.Query("protocol"), live, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// XMLPrecheck 校验一份新的域定义并给出 diff（API-037）。**只读**，不应用。
//
// 两件事一起返回是有意的：用户提交新定义之后，他要同时知道「我改了什么」与
// 「这份能不能用」。分两次请求会让他在看到 diff 之后还得再点一次才知道行不行
// ——而那时他已经做好了决定。
//
// **不需要二次验证**：它不产生任何改动。而"看一眼会影响什么"这件事如果需要
// 先验证一次，用户就会在还不知道要改什么的时候被迫走一遍验证流程。
func (h *Console) XMLPrecheck(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		XML string `json:"xml"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	view, err := h.svc.PrecheckXML(ctx, id, req.XML, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}

// XMLUpdate 应用一份新的域定义（API-038）。
//
// **它绕过我们建立的其它全部校验**：同节点、配额、地址唯一性、端口安全的前置
// 条件——在 XML 里都可以被绕开。因此它比「对外暴露控制台」更需要二次验证，
// 而不是同样需要：暴露的后果是"多开了一个入口"，而这里是可以把一台机器的
// 电源、磁盘、网络改成任何样子。
func (h *Console) XMLUpdate(ctx context.Context, c *app.RequestContext) {
	id, err := api.NamedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req struct {
		XML string `json:"xml"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	if !h.risk.Require(c, risk.ActionVMXMLEdit) {
		return
	}
	ctx = vm.WithXMLVerified(ctx)

	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	view, err := h.svc.UpdateXML(ctx, id, req.XML, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, view)
}
