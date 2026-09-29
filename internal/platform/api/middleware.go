package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"runtime/debug"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
)

// RequestIDHeader 是链路追踪请求头（API.md §2）。
const RequestIDHeader = "X-Request-Id"

// RequestID 为每个请求确定 request_id：客户端已传则透传，否则生成。
// 该值会写入响应头与响应体，是排查问题的关联键。
func RequestID() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		id := string(c.GetHeader(RequestIDHeader))
		if id == "" {
			id = newRequestID()
		}
		c.Set(requestIDKey, id)
		c.Response.Header.Set(RequestIDHeader, id)
		c.Next(withRequestID(ctx, id))
	}
}

// Recover 捕获 panic，记录堆栈并返回统一的内部错误。
//
// 注册顺序必须在业务处理器之前；堆栈只进日志，不返回给客户端。
func Recover() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[panic] request_id=%s path=%s err=%v\n%s",
					RequestIDFrom(c), c.FullPath(), r, debug.Stack())
				Fail(c, Internal())
			}
		}()
		c.Next(ctx)
	}
}

// AccessLog 输出访问日志。
//
// 只记录路由模板（FullPath）而非原始路径，避免把路径中的标识与查询参数
// 原样写入日志。
func AccessLog() app.HandlerFunc {
	return func(ctx context.Context, c *app.RequestContext) {
		start := time.Now()
		c.Next(ctx)
		log.Printf("[http] %s %s status=%d cost=%s ip=%s request_id=%s",
			c.Request.Method(), c.FullPath(), c.Response.StatusCode(),
			time.Since(start).Round(time.Millisecond), c.ClientIP(), RequestIDFrom(c))
	}
}

// newRequestID 生成 16 位十六进制随机串。
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时退化为时间戳，保证请求不中断。
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405")))[:16]
	}
	return hex.EncodeToString(b[:])
}
