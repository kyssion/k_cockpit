package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/logging"
)

// Stream 把日志作为 SSE 流推给浏览器（API-295）。
//
// 这是 G-28 论证的落点：日志在线查看是该论证里**唯一合格**的场景——
// 它持续在变、不会自己停下来，而用户打开这一页的原因通常就是盯着
// 一件事的发生。其余几处轮询仍然是条件轮询，不必换（见 GAP_CLOSURE）。
//
// 仍然用 Cookie 鉴权，因此不需要把令牌放进查询串——`EventSource` 对同源
// 请求会自动带上 Cookie。这一点与参考项目的做法不同，他们必须在 URL 上
// 带令牌，而那会让凭据进入访问日志与浏览器历史。
func (h *Logging) Stream(ctx context.Context, c *app.RequestContext) {
	_ = ctx
	sub := h.log.Subscribe()

	// **不做 `defer sub.Close()`。** 流式响应在 handler 返回**之后**才由
	// 服务端读取，因此 handler 里的 defer 会在订阅还没被用过时就把它关掉
	// ——表现是「连上了但一条日志都没有」，而它看起来像日志器没在工作。
	// 改由写协程在结束时关闭。

	c.SetContentType("text/event-stream; charset=utf-8")
	c.Response.Header.Set("Cache-Control", "no-cache")
	// 让反向代理不要缓冲：缓冲会让「实时」变成「攒够一批再给」，而那样
	// 这一页与轮询就没有区别了，还多了一条长连接。
	c.Response.Header.Set("X-Accel-Buffering", "no")

	pr, pw := io.Pipe()
	// bodySize < 0 表示读到 EOF 为止；服务端读完后会调用 pr.Close()，
	// 于是写端的下一次 Write 返回 ErrClosedPipe —— 那就是客户端断开的信号。
	c.Response.SetBodyStream(pr, -1)

	go func() {
		defer sub.Close()
		defer pw.Close()

		// 先补最近几行：刚打开的页面立刻有内容，否则要等到下一次日志
		// 才看见东西，而用户会以为流没连上。
		for _, e := range h.log.Tail(logging.TailQuery{Limit: streamBacklog}) {
			if err := writeLogEvent(pw, e); err != nil {
				return
			}
		}

		// 心跳：注释帧不产生事件，但能让中间的代理与浏览器知道连接还活着。
		// 不发送的话，长时间无日志的连接会被某些代理按空闲超时掐掉，而
		// 表现是「看着看着就不更新了」。
		ticker := time.NewTicker(streamHeartbeat)
		defer ticker.Stop()

		for {
			select {
			case e, ok := <-sub.C():
				if !ok {
					return
				}
				// 有丢弃时先说一句。丢弃本身是必要的（见 logging.Subscribe），
				// 但**必须让用户知道这里有缺口**——不说的话他会以为那段时间
				// 没有日志，而实际上有、只是没送达。
				if n := sub.Dropped(); n > 0 {
					if err := writeGapEvent(pw, n); err != nil {
						return
					}
				}
				if err := writeLogEvent(pw, e); err != nil {
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

const (
	// streamBacklog 是连接建立时补推的历史行数。给少一点：这一页的用途是
	// 「看着它发生」，而不是回溯——回溯用在线查看的列表或导出。
	streamBacklog = 100
	// streamHeartbeat 是心跳间隔。
	streamHeartbeat = 20 * time.Second
)

func writeLogEvent(w io.Writer, e logging.Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: log\ndata: %s\n\n", b)
	return err
}

// writeGapEvent 告诉客户端「中间有若干行没送到」。
//
// 它是这一条流上唯一可能出现**静默错误**的地方：丢弃是服务端主动做的，
// 而客户端无从分辨「没有日志」与「日志没推过来」。因此必须显式说出来。
func writeGapEvent(w io.Writer, n int64) error {
	_, err := fmt.Fprintf(w,
		"event: gap\ndata: {\"dropped\":%d}\n\n", n)
	return err
}
