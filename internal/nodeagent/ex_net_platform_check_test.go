package nodeagent

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
)

func platformCheckResults(t *testing.T, expectations ...agent.PlatformExpectation) []agent.PlatformCheckResult {
	t.Helper()
	res, err := platformCheck(context.Background(), agent.Operation{
		Params: map[string]any{"expectations": expectations},
	}, func(Stage) {})
	if err != nil {
		t.Fatalf("platformCheck: %v", err)
	}
	if !res.Success {
		t.Fatalf("自检失败: %s", res.Message)
	}
	items, ok := res.Data[agent.PlatformCheckKey].([]agent.PlatformCheckResult)
	if !ok {
		t.Fatalf("结果形状不对: %#v", res.Data)
	}
	return items
}

// 端口镜像在节点侧有确定的对象名，必须按实态核对，而不是一律回「分期接入」。
func TestPlatformCheckMirrorUsesOVSRow(t *testing.T) {
	installFakeCommands(t, map[string]string{
		"ovs-vsctl": "#!/bin/sh\ncase \"$*\" in\n  *'get Mirror kc-mir-m1 name'*) echo kc-mir-m1 ;;\n  *) exit 1 ;;\nesac\n",
	})

	items := platformCheckResults(t,
		agent.PlatformExpectation{ID: 1, Kind: "port_mirror", Target: "m1"},
		agent.PlatformExpectation{ID: 2, Kind: "port_mirror", Target: "m2"},
	)
	if len(items) != 2 {
		t.Fatalf("期望两项结果，实际 %d", len(items))
	}
	if !items[0].Present || !strings.Contains(items[0].Actual, "kc-mir-m1") {
		t.Errorf("存在的镜像应报 Present=true 并给出对象名: %+v", items[0])
	}
	if items[1].Present || !strings.Contains(items[1].Actual, "kc-mir-m2") {
		t.Errorf("不存在的镜像应报 Present=false 并给出对象名: %+v", items[1])
	}
	if items[0].Unverifiable || items[1].Unverifiable {
		t.Error("能按名字定位的镜像不应标记为无法核对")
	}
}

// 查不了的对象必须与偏差区分开：Present=false 只代表「没查」，不是「配置丢了」。
func TestPlatformCheckUnverifiableKinds(t *testing.T) {
	items := platformCheckResults(t,
		agent.PlatformExpectation{ID: 1, Kind: "public_ip", Target: "binding:12"},
		agent.PlatformExpectation{ID: 2, Kind: "unknown", Target: "x"},
	)
	if len(items) != 2 {
		t.Fatalf("期望两项结果，实际 %d", len(items))
	}
	for _, it := range items {
		if !it.Unverifiable {
			t.Errorf("无法定位对象的类别应标记 Unverifiable: %+v", it)
		}
		if it.Present || it.Actual == "" {
			t.Errorf("未核对的项不能报存在，且必须说明原因: %+v", it)
		}
		if strings.Contains(it.Actual, "分期接入") {
			t.Errorf("P3 网络能力已交付，不应再使用该过期理由: %+v", it)
		}
	}
}
