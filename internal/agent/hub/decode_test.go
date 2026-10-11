package hub

import (
	"encoding/json"
	"testing"

	"k_cockpit/internal/agent"
)

// TestDecodeResultDataScalars 标量键走通用解码：string 保持 string、数字
// 是 float64（与 mock 行为一致，服务层对它们的断言依赖这一点）。
func TestDecodeResultDataScalars(t *testing.T) {
	out, err := decodeResultData(agent.OpKind("vm.status"), `{"status":"running","size_bytes":1024,"ok":true}`)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if s, _ := out["status"].(string); s != "running" {
		t.Errorf("status 应为 string，实际 %T", out["status"])
	}
	if f, _ := out["size_bytes"].(float64); f != 1024 {
		t.Errorf("数字应为 float64（JSON 语义），实际 %T", out["size_bytes"])
	}
	if b, _ := out["ok"].(bool); !b {
		t.Errorf("ok 应为 true")
	}
}

// TestDecodeResultDataEmpty 空 Data 回 nil：服务层对无数据结果的判定不受影响。
func TestDecodeResultDataEmpty(t *testing.T) {
	for _, raw := range []string{"", "{}"} {
		out, err := decodeResultData(agent.OpKind("vm.delete"), raw)
		if err != nil || out != nil {
			t.Errorf("raw=%q 应返回 nil,nil，实际 %v,%v", raw, out, err)
		}
	}
}

// TestDecodeResultDataTyped 类型化解码表按（操作，键）命中：登记的键还原为
// 具体类型，未登记的键继续走通用解码——两轨并存是设计而非疏漏。
func TestDecodeResultDataTyped(t *testing.T) {
	type demo struct {
		Names []string `json:"names"`
	}
	typedDecoders["test.kind"] = map[string]decodeFunc{
		"payload": func(raw json.RawMessage) (any, error) {
			var d demo
			if err := json.Unmarshal(raw, &d); err != nil {
				return nil, err
			}
			return d, nil
		},
	}
	t.Cleanup(func() { delete(typedDecoders, "test.kind") })

	out, err := decodeResultData("test.kind", `{"payload":{"names":["a"]},"extra":1}`)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	d, ok := out["payload"].(demo)
	if !ok || len(d.Names) != 1 || d.Names[0] != "a" {
		t.Errorf("payload 应还原为 demo 结构，实际 %T", out["payload"])
	}
	if f, _ := out["extra"].(float64); f != 1 {
		t.Errorf("未登记键应走通用解码，实际 %T", out["extra"])
	}
}

// TestDecodeConsoleFrame 控制台帧的往返：[]byte 经 JSON base64 往返后必须
// 还原为 []byte（服务层取 frame.Data 直接回给浏览器）。
func TestDecodeConsoleFrame(t *testing.T) {
	f := agent.ConsoleFrame{MIME: "image/png", Data: []byte("hello-png"), Width: 640, Height: 480}
	b, err := json.Marshal(map[string]any{agent.FrameDataKey: f})
	if err != nil {
		t.Fatalf("编码: %v", err)
	}
	out, err := decodeResultData(agent.OpVMConsoleFrame, string(b))
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	got, ok := out[agent.FrameDataKey].(agent.ConsoleFrame)
	if !ok {
		t.Fatalf("应为 ConsoleFrame，实际 %T", out[agent.FrameDataKey])
	}
	if string(got.Data) != "hello-png" || got.Width != 640 || got.MIME != "image/png" {
		t.Fatalf("字段不符: %+v", got)
	}
}

// TestDecodeScalarKeys 标量键解码后必须是 string / float64（服务层的类型
// 断言依赖）。
func TestDecodeScalarKeys(t *testing.T) {
	out, err := decodeResultData(agent.OpVMSnapshotCreate,
		`{"domain_name":"p2v9-snap3","size_bytes":2048}`)
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	if s, _ := out["domain_name"].(string); s != "p2v9-snap3" {
		t.Errorf("domain_name 应为 string，实际 %T", out["domain_name"])
	}
	if f, _ := out["size_bytes"].(float64); f != 2048 {
		t.Errorf("size_bytes 应为 float64，实际 %T", out["size_bytes"])
	}
}
