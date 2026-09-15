package api

import (
	"context"
	"errors"
	"log"
	"math"

	"github.com/cloudwego/hertz/pkg/app"
)

// Response 是成功响应体（API.md §3.1）。
type Response struct {
	Data       any    `json:"data"`
	Pagination *Page  `json:"pagination,omitempty"`
	RequestID  string `json:"request_id"`
}

// Page 是分页元信息（API.md §2 统一命名：page / page_size）。
type Page struct {
	Page       int   `json:"page"`
	PageSize   int   `json:"page_size"`
	Total      int64 `json:"total"`
	TotalPages int   `json:"total_pages"`
}

// NewPage 由页码、每页条数与总数计算分页元信息。
func NewPage(page, pageSize int, total int64) *Page {
	pages := 0
	if pageSize > 0 {
		pages = int(math.Ceil(float64(total) / float64(pageSize)))
	}
	return &Page{Page: page, PageSize: pageSize, Total: total, TotalPages: pages}
}

// ErrorResponse 是错误响应体（API.md §3.2）。
type ErrorResponse struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"request_id"`
}

// ErrorBody 是错误响应中的 error 字段。
type ErrorBody struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Details []Detail `json:"details,omitempty"`
}

// OK 返回 200 与数据。
func OK(c *app.RequestContext, data any) {
	c.JSON(200, Response{Data: data, RequestID: RequestIDFrom(c)})
}

// OKPage 返回 200 与分页数据。
func OKPage(c *app.RequestContext, data any, page *Page) {
	c.JSON(200, Response{Data: data, Pagination: page, RequestID: RequestIDFrom(c)})
}

// Created 返回 201 与新建资源。
func Created(c *app.RequestContext, data any) {
	c.JSON(201, Response{Data: data, RequestID: RequestIDFrom(c)})
}

// NoContent 返回 204，无响应体（用于删除成功）。
func NoContent(c *app.RequestContext) {
	c.SetStatusCode(204)
}

// Fail 将错误转为统一错误响应。
//
// 非 *Error 的错误一律按内部错误处理：**原始错误只写入日志**，
// 响应中只给出通用提示与 request_id，避免泄漏内部细节。
func Fail(c *app.RequestContext, err error) {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		log.Printf("[error] request_id=%s path=%s err=%v", RequestIDFrom(c), c.FullPath(), err)
		apiErr = Internal()
	}

	c.JSON(apiErr.Status, ErrorResponse{
		Error: ErrorBody{
			Code:    apiErr.Code,
			Message: apiErr.Message,
			Details: apiErr.Details,
		},
		RequestID: RequestIDFrom(c),
	})
}

// FailWith 在 Fail 基础上附加字段级明细，用于表单校验反馈。
func FailWith(c *app.RequestContext, err *Error, details ...Detail) {
	clone := *err
	clone.Details = append(clone.Details, details...)
	Fail(c, &clone)
}

// requestIDKey 是 request_id 在 RequestContext 中的键。
const requestIDKey = "request_id"

// RequestIDFrom 取出当前请求的 request_id；不存在时返回空串。
func RequestIDFrom(c *app.RequestContext) string {
	if v, ok := c.Get(requestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// withRequestID 把 request_id 写入底层 context，供下游（日志、审计）使用。
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKeyRequestID{}, id)
}

type contextKeyRequestID struct{}

// RequestIDFromContext 从 context.Context 取出 request_id。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKeyRequestID{}).(string); ok {
		return v
	}
	return ""
}
