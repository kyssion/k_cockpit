package vm

import (
	"testing"

	"k_cockpit/internal/model"
)

// 本文件覆盖第 3 轮 DEF-R3-17：无 agent 时 GET /vms/:id 的 init 是空对象，
// 用户分不清"正在初始化"与"永远不会就绪"。

// TestInitViewCoversAllProjectionStates 覆盖投影的每一种状态都给得出结论。
func TestInitViewCoversAllProjectionStates(t *testing.T) {
	cases := []struct {
		name    string
		vm      model.VM
		ready   bool
		state   string
		wantWhy bool // 未就绪时必须给 reason
	}{
		{"运行中且 agent 就绪", model.VM{Status: model.VMStatusRunning, GuestAgent: true},
			true, InitStateReady, false},
		{"运行中但 agent 不可用", model.VM{Status: model.VMStatusRunning, GuestAgent: false},
			false, InitStateAgentUnavailable, true},
		{"已关机", model.VM{Status: model.VMStatusStopped}, false, InitStateStopped, true},
		{"状态未知", model.VM{Status: model.VMStatusUnknown}, false, InitStateUnknown, true},
		{"过渡状态（paused）", model.VM{Status: model.VMStatusPaused}, false, InitStateStarting, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := initView(&tc.vm)
			if got.Ready != tc.ready {
				t.Errorf("ready = %v，期望 %v", got.Ready, tc.ready)
			}
			if got.State != tc.state {
				t.Errorf("state = %q，期望 %q", got.State, tc.state)
			}
			if tc.wantWhy && got.Reason == "" {
				t.Error("未就绪时必须给出可执行的说明（否则用户不知道该等还是该动手）")
			}
			if !tc.wantWhy && got.Reason != "" {
				t.Errorf("就绪时不该带说明，实际 %q", got.Reason)
			}
			// 最常见的误判：关机被当成"还在初始化"→ 用户白等。
			if tc.state == InitStateStopped && got.Ready {
				t.Error("关机的机器不可能就绪")
			}
		})
	}
}

// TestInitViewNeverEmpty 覆盖"永远不会是空对象"。
//
// 这就是这个结构存在的理由：以前接口没有这一块，客户端拿到 `{}` 之后无从
// 判断。任何状态的投影都必须给出 ready 与 state 两个字段。
func TestInitViewNeverEmpty(t *testing.T) {
	for _, status := range []string{
		model.VMStatusRunning, model.VMStatusStopped, model.VMStatusUnknown,
		model.VMStatusPaused, "", "some-future-status",
	} {
		got := initView(&model.VM{Status: status})
		if got.State == "" {
			t.Errorf("状态 %q 的投影没有给出 state", status)
		}
	}
}
