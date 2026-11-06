package monitor_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// 本文件覆盖第 6 轮 DEF-R6-01：`init.ready` 不随 agent 可达更新。
//
// 真机实测（全程按 API 起机）：t=40s 起 `virsh qemu-agent-command … guest-ping`
// 已返回 `{"return":{}}`，而 `GET /vms/:id` 的 `init` 在 t=50…240s 的 20 次
// 采样里**始终**是 `agent_unavailable / false`。
//
// 根因不是判定文案，而是**这个信号没有任何写入方**：`model.VM.GuestAgent`
// 只有读取方（guest 视图与 init 投影），恒为 false，init 再正确也翻不过来。
//
// 为什么探测放在每轮的 `vm.stats` 而不是运行态扫描（`node.domains`）：
// 后者每 5 个采集节拍才跑一次（60s × 5 = **5 分钟**），而用户等的结论是
// "它到底起来没有"——5 分钟滞后在实测窗口里等于永远不更新。`vm.stats`
// 每轮对每台运行中的 VM 调用一次（60s），多一次本地 guest-ping 的边际成本
// 可以忽略（该操作本身已经要 1 秒做两次采样）。

// statsAgent 让每轮的 vm.stats 返回可控的 agent 可达性。
type statsAgent struct {
	*stubClient
	guestAgent bool
	calls      int
}

func (s *statsAgent) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind == agent.OpVMStats {
		s.calls++
		return &agent.Result{Success: true, Data: map[string]any{
			agent.StatsDataKey: agent.VMStats{GuestAgent: s.guestAgent},
		}}, nil
	}
	return s.stubClient.Execute(ctx, op)
}

// failingStatsAgent 模拟"这台机器当前采不到指标"。
type failingStatsAgent struct{ *stubClient }

func (f *failingStatsAgent) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind == agent.OpVMStats {
		return &agent.Result{Success: false, Message: "domstats 失败"}, nil
	}
	return f.stubClient.Execute(ctx, op)
}

// TestCollectVMStatsWritesGuestAgent 覆盖"每轮把探测结果写进投影"。
func TestCollectVMStatsWritesGuestAgent(t *testing.T) {
	client := &statsAgent{stubClient: newStubClient(), guestAgent: true}
	db, c := newTestEnv(t, client)
	row := seedVM(t, db, "vm-agent", model.VMStatusRunning)

	c.Tick(context.Background())

	var got model.VM
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !got.GuestAgent {
		t.Fatal("节点报 agent 可达，投影必须写 true（DEF-R6-01 就是这里永远 false）")
	}
	if client.calls == 0 {
		t.Fatal("本轮没有调用 vm.stats——探测根本没发生")
	}
}

// TestGuestAgentProjectionFlips 覆盖"翻得过来"这条最关键的行为。
//
// 第一轮 agent 还没起来 → false；第二轮它起来了（真机上 t=40s 那一刻）→ 必须变 true，
// 而且要在**下一次采样**就翻，不能等 5 分钟（这正是选 vm.stats 的原因）。
func TestGuestAgentProjectionFlips(t *testing.T) {
	client := &statsAgent{stubClient: newStubClient(), guestAgent: false}
	db, c := newTestEnv(t, client)
	row := seedVM(t, db, "vm-flip", model.VMStatusRunning)

	c.Tick(context.Background())
	var first model.VM
	if err := db.First(&first, row.ID).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if first.GuestAgent {
		t.Fatal("agent 还没起来时不该是 true")
	}

	// 来宾侧 agent 现在起来了。
	client.guestAgent = true
	c.Tick(context.Background())

	var second model.VM
	if err := db.First(&second, row.ID).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !second.GuestAgent {
		t.Fatal("agent 已可达，投影必须在下一次采样就翻成 true（不能等 5 分钟）")
	}
}

// TestGuestAgentStaysFalseWhenNotReachable 覆盖反方向：不可达就写 false。
func TestGuestAgentStaysFalseWhenNotReachable(t *testing.T) {
	client := &statsAgent{stubClient: newStubClient(), guestAgent: false}
	db, c := newTestEnv(t, client)
	row := seedVM(t, db, "vm-noagent", model.VMStatusRunning)
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Update("guest_agent", true).Error; err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	c.Tick(context.Background())

	var got model.VM
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.GuestAgent {
		t.Error("节点报不可达时必须写 false（否则 agent 挂掉后界面永远显示就绪）")
	}
}

// TestGuestAgentUntouchedWhenStatsUnavailable 覆盖"采不到指标时不写"。
//
// 采不到说明这台机器当前联系不上；那时把 guest_agent 写成 false 是拿
// "采不到"冒充"agent 不可用"——两者对用户的意义完全不同。
func TestGuestAgentUntouchedWhenStatsUnavailable(t *testing.T) {
	db, c := newTestEnv(t, &failingStatsAgent{stubClient: newStubClient()})
	row := seedVM(t, db, "vm-unreachable", model.VMStatusRunning)
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Update("guest_agent", true).Error; err != nil {
		t.Fatalf("预置失败: %v", err)
	}

	c.Tick(context.Background())

	var got model.VM
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !got.GuestAgent {
		t.Error("采不到指标时不该动 guest_agent（那是「没采到」不是「不可用」）")
	}
}
