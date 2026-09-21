package handler

import (
	"context"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/reqlog"
)

// ReqLog 提供请求日志的查询与清理。
type ReqLog struct {
	svc *reqlog.Service
}

// NewReqLog 构造请求日志接口。
func NewReqLog(svc *reqlog.Service) *ReqLog { return &ReqLog{svc: svc} }

// List 列出最近的请求日志。
func (h *ReqLog) List(ctx context.Context, c *app.RequestContext) {
	items, err := h.svc.List(ctx, int64(queryInt(c, "user_id")), queryInt(c, "limit"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"items": items, "enabled": h.svc.Enabled(ctx)})
}

// Clear 清理请求日志。keep_days > 0 时只删这个天数之前的。
func (h *ReqLog) Clear(ctx context.Context, c *app.RequestContext) {
	n, err := h.svc.Clear(ctx, queryInt(c, "keep_days"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"deleted": n})
}

// RequestLogger 返回一个记录请求日志的中间件。
//
// 位置很关键：它**包在最外层**（在鉴权之外），这样才能记到 401 —— 而"为什么
// 一直 401"恰恰是最需要看请求日志的场景。用户信息在请求结束时才取：认证
// 中间件在它之后运行。
//
// 它只在开关打开时写库，且写入失败绝不影响响应——日志是旁路。
func RequestLogger(svc *reqlog.Service) app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		if svc == nil {
			c.Next(ctx)
			return
		}
		// 健康检查与静态资源不记：它们占了绝大部分调用量，记下来只会把
		// 真正有用的那几条挤掉。
		path := string(c.Path())
		if path == "/health" || path == "/api/public/version" || strings.HasPrefix(path, "/assets/") {
			c.Next(ctx)
			return
		}

		start := time.Now()
		c.Next(ctx)

		user := auth.CurrentUser(c)
		var userID *int64
		if user != nil {
			id := user.ID
			userID = &id
		}

		// User-Agent 截断：它是用户可控的，不加限制会变成一个往库里塞任意
		// 长字符串的入口。
		ua := string(c.UserAgent())
		if len(ua) > 255 {
			ua = ua[:255]
		}
		ip := c.ClientIP()
		svc.Record(ctx, reqlog.Entry{
			UserID:     userID,
			Method:     string(c.Method()),
			Path:       path,
			Status:     c.Response.StatusCode(),
			DurationMS: int(time.Since(start).Milliseconds()),
			ClientIP:   &ip,
			UserAgent:  &ua,
		})
	}
}
