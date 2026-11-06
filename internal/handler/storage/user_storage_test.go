package storage_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/handler/storage"
	"k_cockpit/internal/platform/api"
)

// newUserStorageTestEngine 只挂被测的 my-storage 路由，中间件与生产一致。
//
// svc 传 nil 是刻意的：本用例覆盖「缺 node_id」的前置校验，它在调用服务层
// 之前就返回，因此不需要真实服务；若校验失效而继续往下走，会因空指针直接
// 失败，正好构成一个强断言而非弱断言。
func newUserStorageTestEngine() *server.Hertz {
	h := server.Default()
	h.Use(api.RequestID(), api.Recover())
	us := storage.NewUserStorage(nil)
	h.GET("/my-storage", us.Get)
	h.POST("/my-storage", us.Ensure)
	h.GET("/my-storage/files", us.ListFiles)
	h.GET("/my-storage/files/:id/download", us.Download)
	h.DELETE("/my-storage/files/:id", us.DeleteFile)
	return h
}

// TestMyStorageRejectsMissingNodeID 覆盖 DEF-003：node_id 只从 query 读，
// 缺失（或只放在 JSON body 里）时必须返回 400 INVALID_PARAMETER 并给出可执行
// 提示，而不是让 nodeID=0 流到服务层变成误导性的 404「节点不存在」。
func TestMyStorageRejectsMissingNodeID(t *testing.T) {
	h := newUserStorageTestEngine()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"GET 缺 node_id", "GET", "/my-storage", ""},
		// QA 原始复现：node_id 放在 body，query 里没有。
		{"POST body 带 node_id 但 query 缺", "POST", "/my-storage", `{"node_id":1}`},
		{"POST 完全缺参数", "POST", "/my-storage", `{}`},
		{"GET files 缺 node_id", "GET", "/my-storage/files", ""},
		{"GET download 缺 node_id", "GET", "/my-storage/files/1/download", ""},
		{"DELETE files 缺 node_id", "DELETE", "/my-storage/files/1", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *ut.Body
			var headers []ut.Header
			if tc.body != "" {
				body = &ut.Body{Body: strings.NewReader(tc.body), Len: len(tc.body)}
				headers = []ut.Header{{Key: "Content-Type", Value: "application/json"}}
			}
			w := ut.PerformRequest(h.Engine, tc.method, tc.path, body, headers...)
			if w.Code != consts.StatusBadRequest {
				t.Fatalf("HTTP = %d，期望 400（body=%s）", w.Code, w.Body.Bytes())
			}
			var resp api.ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("解析响应失败: %v (body=%s)", err, w.Body.Bytes())
			}
			if resp.Error.Code != api.CodeInvalidParameter {
				t.Errorf("error.code = %q，期望 %q", resp.Error.Code, api.CodeInvalidParameter)
			}
			if !strings.Contains(resp.Error.Message, "node_id") {
				t.Errorf("错误信息应可执行地指出 node_id，实际 %q", resp.Error.Message)
			}
			if resp.RequestID == "" {
				t.Error("响应缺少 request_id")
			}
		})
	}
}
