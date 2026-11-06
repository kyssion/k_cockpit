package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// newUploadGuardServer 挂与生产一致的守卫中间件 + 一条分片上传路由。
func newUploadGuardServer() *server.Hertz {
	h := server.New()
	h.Use(SecurityHeaders(), InputFilter(nil))
	h.PUT("/api/v1/my-storage/uploads/:uid/chunks/:index",
		func(_ context.Context, c *app.RequestContext) { c.String(consts.StatusOK, "ok") })
	h.POST("/api/v1/my-storage/uploads/:uid/chunks/:index",
		func(_ context.Context, c *app.RequestContext) { c.String(consts.StatusOK, "ok") })
	return h
}

// 本文件覆盖第 3 轮 DEF-R3-04：上传守卫白名单缺 application/octet-stream。
//
// 分片上传的客户端（`curl --data-binary`、fetch 的 Blob）会显式声明
// `application/octet-stream`，而旧白名单里没有它 → 403。当时的"默认能过"
// 只是 `curl -d` 恰好发的是 x-www-form-urlencoded（在白名单内）造成的假象。

// TestGuardAllowsOctetStreamBody 覆盖声明了 octet-stream 的请求体不被拦。
func TestGuardAllowsOctetStreamBody(t *testing.T) {
	h := newUploadGuardServer()
	body := &ut.Body{Body: strings.NewReader("raw-binary-chunk"), Len: 17}
	w := ut.PerformRequest(h.Engine, "PUT", "/api/v1/my-storage/uploads/uid/chunks/0", body,
		ut.Header{Key: "Content-Type", Value: "application/octet-stream"})
	if w.Code == consts.StatusForbidden {
		t.Fatalf("分片上传的裸二进制体被守卫拦了（DEF-R3-04）: %s", w.Body.Bytes())
	}
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（测试路由直通）", w.Code)
	}
}

// TestGuardAllowsOctetStreamWithParams 覆盖带 charset 等参数的写法。
//
// 真实客户端会发 `application/octet-stream; charset=binary` 这类带参数的值；
// 中间件按 ';' 截断后再比对，这里把这条口径也钉住。
func TestGuardAllowsOctetStreamWithParams(t *testing.T) {
	h := newUploadGuardServer()
	body := &ut.Body{Body: strings.NewReader("chunk"), Len: 5}
	w := ut.PerformRequest(h.Engine, "PUT", "/api/v1/my-storage/uploads/uid/chunks/1", body,
		ut.Header{Key: "Content-Type", Value: "application/octet-stream; charset=binary"})
	if w.Code == consts.StatusForbidden {
		t.Fatalf("带参数的 octet-stream 被拦: %s", w.Body.Bytes())
	}
}

// TestGuardStillRejectsUnknownType 覆盖"补白名单不等于放开一切"。
func TestGuardStillRejectsUnknownType(t *testing.T) {
	h := newUploadGuardServer()
	body := &ut.Body{Body: strings.NewReader("<xml/>"), Len: 6}
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/my-storage/uploads/uid/chunks/0", body,
		ut.Header{Key: "Content-Type", Value: "application/x-shockwave-flash"})
	if w.Code != consts.StatusForbidden {
		t.Fatalf("未知内容类型应仍被拦，实际 %d", w.Code)
	}
}
