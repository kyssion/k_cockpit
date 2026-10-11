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
