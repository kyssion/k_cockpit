package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/logging"
)

// Logging 提供日志管理（F-9-02）。归管理员。
//
// 归管理员的理由：日志里有**全部请求的上下文**——谁在什么时候访问了什么，
// 以及服务端内部的报错细节。它对运维排障有用，而对租户既没有意义也不该
// 被看到。
type Logging struct {
	log   *logging.Logger
	audit *audit.Recorder
}

// NewLogging 构造日志接口。
func NewLogging(log *logging.Logger, recorder *audit.Recorder) *Logging {
	return &Logging{log: log, audit: recorder}
}

// Status 返回日志运行状态（API-290）。
func (h *Logging) Status(_ context.Context, c *app.RequestContext) {
	api.OK(c, h.log.Status())
}

// Read 在线查看最近的日志（API-291）。
//
// **只读内存**，不扫磁盘：在线查看要即时响应，而一个几百 MB 的日志文件
// 扫一遍会让页面卡上几秒。需要更早的内容时用导出。
func (h *Logging) Read(_ context.Context, c *app.RequestContext) {
	items := h.log.Tail(logging.TailQuery{
		Limit:   queryInt(c, "limit"),
		Level:   c.Query("level"),
		Keyword: c.Query("keyword"),
	})
	api.OK(c, map[string]any{"items": items})
}

// SetLevel 调整日志级别（API-292）。
func (h *Logging) SetLevel(ctx context.Context, c *app.RequestContext) {
	var req struct {
		Level string `json:"level"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	level, ok := logging.ParseLevel(req.Level)
	if !ok {
		api.Fail(c, api.InvalidParameter("未知的日志级别，可选 debug / info / warn / error"))
		return
	}

	before := h.log.Level()
	h.log.SetLevel(level)

	// **调级别要记审计**：这句话的意思不是"记一笔流水"，而是把它变成
	// 一次可追溯的变更。把级别调到 DEBUG 会让日志量显著上升，进而加快
	// 磁盘消耗（以及可能的脱敏信息暴露面扩大）——事后要能回答"是谁在
	// 什么时候把它打开的"。
	user := auth.CurrentUser(c)
	if h.audit != nil && user != nil {
		h.audit.Record(ctx, audit.Entry{
			OperatorID: user.ID, OperatorName: user.Username,
			ResourceType: "logging", Action: "log.level.change",
			Params: map[string]any{
				"before": before.String(), "after": level.String(),
			},
			Success: true, ClientIP: auth.ClientInfoOf(c).IP,
		})
	}

	api.OK(c, map[string]any{"level": level.String()})
}

// Export 导出日志（API-293）。
//
// 导出的内容是**脱敏后**的：脱敏发生在写入时，因此磁盘上的那份本身就不含
// 明文（见 logging 包）。这意味着导出接口不需要再做一遍处理，也不可能
// 因为"忘了处理"而泄漏。
func (h *Logging) Export(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	text, err := h.log.ExportText()
	if err != nil {
		api.Fail(c, api.ValidationFailed(err.Error()))
		return
	}

	if h.audit != nil && user != nil {
		h.audit.Record(ctx, audit.Entry{
			OperatorID: user.ID, OperatorName: user.Username,
			ResourceType: "logging", Action: "log.export",
			Params:  map[string]any{"bytes": len(text)},
			Success: true, ClientIP: auth.ClientInfoOf(c).IP,
		})
	}

	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="kc-logs.txt"`)
	c.Response.SetBodyString(text)
}

// Delete 清理日志（API-294）。
func (h *Logging) Delete(ctx context.Context, c *app.RequestContext) {
	user := auth.CurrentUser(c)
	freed, err := h.log.Purge()
	if err != nil {
		api.Fail(c, api.ValidationFailed(err.Error()))
		return
	}

	if h.audit != nil && user != nil {
		h.audit.Record(ctx, audit.Entry{
			OperatorID: user.ID, OperatorName: user.Username,
			ResourceType: "logging", Action: "log.purge",
			Params: map[string]any{
				"freed_bytes": freed,
				// 清理之后就没法回溯了，因此把"清掉了多少"记下来——
				// 这是唯一能证明当时有多少日志的地方。
				"note": "轮转文件已删除，当前文件已截断（不删除文件本身）",
			},
			Success: true, ClientIP: auth.ClientInfoOf(c).IP,
		})
	}

	api.OK(c, map[string]any{"freed_bytes": freed})
}
