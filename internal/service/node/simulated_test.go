package node_test

import (
	"context"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/service/node"
)

// 预置模拟节点（开发环境启动期调用）的契约：走真实注册链路、缺谁补谁、
// 已存在的一律不动——重复调用必须幂等。
func TestEnsureSimulated(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()
	names := []string{"dev-node-1", "dev-node-2", "dev-node-3"}

	if err := svc.EnsureSimulated(ctx, names); err != nil {
		t.Fatalf("首次预置失败: %v", err)
	}
	// 再跑一遍（重启服务、幂等路径）。
	if err := svc.EnsureSimulated(ctx, names); err != nil {
		t.Fatalf("重复预置失败: %v", err)
	}

	var count int64
	if err := db.Model(&model.Node{}).Count(&count).Error; err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != int64(len(names)) {
		t.Fatalf("节点数 = %d, 期望 %d（重复调用不应新增）", count, len(names))
	}

	views, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	byName := map[string]node.View{}
	for _, v := range views {
		byName[v.Name] = v
	}
	for _, name := range names {
		v, ok := byName[name]
		if !ok {
			t.Fatalf("节点 %s 不存在", name)
		}
		if v.EnrollState != "enrolled" {
			t.Errorf("%s enroll_state = %q, 期望 enrolled（应走完整注册）", name, v.EnrollState)
		}
		if v.Status != "online" {
			t.Errorf("%s status = %q, 期望 online（mock 对任意节点恒在线）", name, v.Status)
		}
		if v.AgentVersion != "mock-0.1.0" {
			t.Errorf("%s agent_version = %q, 期望 mock-0.1.0（前端以 mock- 前缀识别模拟节点）", name, v.AgentVersion)
		}
	}
}
