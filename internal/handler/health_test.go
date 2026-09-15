package handler_test

import (
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

func TestHealth(t *testing.T) {
	h, _ := newTestServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/health", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d, 期望 %d", w.Code, consts.StatusOK)
	}

	var body struct {
		Data struct {
			Status   string `json:"status"`
			Database string `json:"database"`
		} `json:"data"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body.Data.Status != "ok" || body.Data.Database != "up" {
		t.Errorf("健康检查数据 = %+v, 期望 status=ok database=up", body.Data)
	}
	if body.RequestID == "" {
		t.Error("响应缺少 request_id")
	}
}
