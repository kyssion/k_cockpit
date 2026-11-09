package vm_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// streamingClient 在通用桩之上补一条"能打开控制台流"的能力——绝大多数
// 用例里的桩故意不支持流（测的是「不支持时要说清楚」），而会话上限这条
// 规则必须先能开出会话。
type streamingClient struct {
	*stubClient
	openErr error
}

func (c *streamingClient) OpenStream(
	context.Context, agent.StreamKind, int64, string,
) (agent.Stream, error) {
	if c.openErr != nil {
		return nil, c.openErr
	}
	return &nopStream{}, nil
}

// nopStream 是一条立即 EOF 的空流：用例只关心会话的登记与释放。
type nopStream struct{ closed bool }

func (s *nopStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (s *nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *nopStream) Close() error                { s.closed = true; return nil }

// TestConsoleSessionLimitEnforced 覆盖控制台会话上限（R-014）。
//
// 这条规则此前**形同虚设**：WS handler 里的 `upgraded bool` 永远来不及置位，
// 每次成功升级都立刻把刚建立会话释放掉，于是 active_sessions 恒为 0，
// 同一个用户可以无限并发打开控制台，把节点上的 VNC 连接堆满。
//
// 用「第 N+1 个必须 409」来断言，而不是断言 active_sessions 的数字：后者在
// 会话被提前释放时同样是 0，会**假通过**。
func TestConsoleSessionLimitEnforced(t *testing.T) {
	client := &streamingClient{stubClient: newStubClient()}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "console-vm", Status: model.VMStatusRunning,
		OwnerID: &owner, DiskGB: 20, Present: true,
		VNCEnabled: true, VideoModel: "virtio"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	viewer := authz.Viewer{UserID: 7}
	var sessions []string
	for i := 0; i < vm.ConsoleSessionLimit; i++ {
		_, session, err := svc.OpenConsole(ctx, row.ID, 7, viewer)
		if err != nil {
			t.Fatalf("第 %d 个会话应能打开: %v", i+1, err)
		}
		sessions = append(sessions, session.ID)
	}

	// 第 N+1 个必须被拒：这是「上限真的生效」的唯一可判据。
	_, _, err := svc.OpenConsole(ctx, row.ID, 7, viewer)
	if err == nil {
		t.Fatalf("超过上限（%d）后仍能打开控制台，会话上限未生效", vm.ConsoleSessionLimit)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Errorf("超过上限应返回 409，实际 err=%v", err)
	}

	// 关掉一个之后必须能再开——否则「上限」其实变成了「终身配额」。
	svc.CloseConsole(sessions[0])
	if _, _, err := svc.OpenConsole(ctx, row.ID, 7, viewer); err != nil {
		t.Errorf("释放一个会话后应能重新打开: %v", err)
	}
}

// TestConsoleSessionNotLeakedOnStreamFailure 覆盖「打开流失败不留会话」。
//
// 会话先登记、再打开流；流打开失败时必须回滚登记，否则每次失败尝试都会
// 永久吃掉一个名额，用户重试几次之后就再也打不开控制台了。
func TestConsoleSessionNotLeakedOnStreamFailure(t *testing.T) {
	client := &streamingClient{stubClient: newStubClient(),
		openErr: errors.New("stream broken")}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "console-vm-2", Status: model.VMStatusRunning,
		OwnerID: &owner, DiskGB: 20, Present: true,
		VNCEnabled: true, VideoModel: "virtio"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	viewer := authz.Viewer{UserID: 7}

	// 失败次数远超上限：只要每次都回滚了登记，就不会撞上 409。
	for i := 0; i < vm.ConsoleSessionLimit*3; i++ {
		if _, _, err := svc.OpenConsole(ctx, row.ID, 7, viewer); err == nil {
			t.Fatal("打开流失败时应返回错误")
		}
	}
	// 流恢复后仍能正常打开，说明前面那些失败没有吃掉名额。
	client.openErr = nil
	if _, _, err := svc.OpenConsole(ctx, row.ID, 7, viewer); err != nil {
		t.Fatalf("失败尝试不应泄漏会话名额: %v", err)
	}
}
