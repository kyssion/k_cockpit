package handler

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/realtime"
)

// 心跳间隔。与日志流共用同一个常量：两条 SSE 面对的是同一批代理与浏览器，
// 各自定一个值只会让"为什么这条断得更快"变得无法解释。

// Stream 推送任务状态变化（SSE，F-7-02）。
//
// 它替代的是任务中心与底部任务栏的轮询。轮询的代价不在带宽，而在**时机**：
// 任务的变化是稀疏的，短间隔轮询几乎每次都空转，长间隔又让用户在任务结束
// 后还盯着一个转圈。
//
// 视角过滤在**服务端**做：租户只收到自己名下资源的事件。让前端收到全部再
// 自己筛，等于把"谁能看到什么"这件事搬到浏览器里——而那不是前端该负责的
// 判断。
//
// 事件只说明"什么变了"，不含变化后的完整内容：完整内容由前端收到后重新
// 查询。推一条可能已经过期的快照，比让客户端自己去取一次准确的更糟。
func (h *Task) Stream(ctx context.Context, c *app.RequestContext) {
	_ = ctx

	if h.bus == nil {
		// 未装配总线时不要用轮询假装成流：那会让调用方以为自己在收推送，
		// 而实际上什么都不会来。
		api.Fail(c, api.Unavailable("实时通道未启用"))
		return
	}

	viewer := authz.ViewerOf(c)
	sub := h.bus.Subscribe()
	// **不做 defer sub.Close()**：流式响应在 handler 返回之后才被读取，
	// 这里的 defer 会在订阅还没用过时就关掉它——表现是"连上了但一条都没有"。
	// 改由写协程在结束时关闭。

	c.SetContentType("text/event-stream; charset=utf-8")
	c.Response.Header.Set("Cache-Control", "no-cache")
	// 让反向代理不要缓冲：缓冲会把"实时"变成"攒够一批再给"，那样这一条
	// 长连接与轮询就没有区别了。
	c.Response.Header.Set("X-Accel-Buffering", "no")

	pr, pw := io.Pipe()
	// -1 表示读到 EOF 为止；服务端读完后关闭 pr，写端的下一次 Write 返回
	// ErrClosedPipe，那就是客户端断开的信号。
	c.Response.SetBodyStream(pr, -1)

	go func() {
		defer sub.Close()
		defer pw.Close()

		// 先发一条 connected：前端据此把界面上的"连接中"换成"已连接"。
		// 不发的话，用户在一项任务都没跑时会看到永远的"连接中"。
		if _, err := io.WriteString(pw, "event: connected\ndata: {}\n\n"); err != nil {
			return
		}

		ticker := time.NewTicker(streamHeartbeat)
		defer ticker.Stop()

		for {
			select {
			case e, ok := <-sub.C():
				if !ok {
					return
				}
				// 丢弃必须显式告知：不说的话前端会以为那段时间没有变化，
				// 而实际上有、只是没送达——它会继续显示旧状态。
				if n := sub.Dropped(); n > 0 {
					if !writeSSE(pw, "gap", map[string]any{"dropped": n}) {
						return
					}
				}
				if !visibleTo(viewer, e) {
					continue
				}
				if !writeSSE(pw, "task", e) {
					return
				}

			case <-ticker.C:
				if _, err := io.WriteString(pw, ": ping\n\n"); err != nil {
					return
				}

			case <-sub.Done():
				return
			}
		}
	}()
}

// visibleTo 判断一个事件对该视角是否可见。
func visibleTo(v authz.Viewer, e realtime.Event) bool {
	if v.IsAdmin {
		return true
	}
	// 无归属的事件（例如管理员操作的节点级任务）只对管理员可见：
	// 租户看它没有任何用处，而"知道存在这么一个任务"本身也是信息。
	return e.OwnerID != 0 && e.OwnerID == v.UserID
}

// writeSSE 写一个命名事件。返回 false 表示写端已关闭（客户端断开）。
func writeSSE(w io.Writer, name string, payload any) bool {
	blob, err := json.Marshal(payload)
	if err != nil {
		return true // 序列化失败不该断开连接，跳过这一条即可
	}
	_, err = io.WriteString(w, "event: "+name+"\ndata: "+string(blob)+"\n\n")
	return err == nil
}
