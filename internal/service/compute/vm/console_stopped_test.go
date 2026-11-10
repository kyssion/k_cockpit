package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
)

// 实机缺陷（r21 / U-12）：虚拟机停机时打开控制台返回 503「无法建立控制台
// 通道」。503 的语义是「服务端暂时不可用，稍后重试」，而重试一台关着的机器
// 永远不会成功；界面上它还会被显示成一次服务故障，而不是「这台机器没开机」。
//
// 正确结果是 409（状态冲突）+ 一句话说清原因。
func TestOpenConsoleStoppedVMReportsConflict(t *testing.T) {
	// 桩的 OpenStream 失败——节点侧对着一台关着的机器确实打不开 VNC 端口。
	client := &streamingClient{stubClient: newStubClient(),
		openErr: errors.New("dial vnc: connection refused")}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "console-stopped", Status: model.VMStatusStopped,
		OwnerID: &owner, DiskGB: 20, Present: true,
		VNCEnabled: true, VideoModel: "virtio"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	_, _, err := svc.OpenConsole(ctx, row.ID, 7, authz.Viewer{UserID: 7})
	if err == nil {
		t.Fatal("停机时打开控制台应失败")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误应带 HTTP 语义，实际 %T: %v", err, err)
	}
	if apiErr.Status != 409 {
		t.Errorf("停机时应返回 409（状态冲突），实际 %d: %v", apiErr.Status, err)
	}
	if !strings.Contains(apiErr.Message, "未运行") {
		t.Errorf("错误信息应说明虚拟机没在运行，实际 %q", apiErr.Message)
	}
}

// 同一处判断的反面：**不能**先按状态投影拒绝。
//
// vm.Status 是虚拟化层状态的缓存，权威在节点侧。如果先按它判决，一个恰好
// 陈旧的投影（刚在节点上开了机、控制面还没刷新）就会挡住一条本可成功的
// 控制台连接。因此这里的状态写成 stopped，而流能打开——结果必须成功。
func TestOpenConsoleDoesNotTrustStaleStatus(t *testing.T) {
	client := &streamingClient{stubClient: newStubClient()} // OpenStream 成功
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "console-stale", Status: model.VMStatusStopped,
		OwnerID: &owner, DiskGB: 20, Present: true,
		VNCEnabled: true, VideoModel: "virtio"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	stream, session, err := svc.OpenConsole(ctx, row.ID, 7, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("流能打开时不应因状态投影陈旧而拒绝: %v", err)
	}
	if stream == nil || session == nil {
		t.Fatal("成功路径必须同时返回流与会话")
	}
	svc.CloseConsole(session.ID)
	_ = stream.Close()
}

// 运行中但通道打不开：这**才**是 503。
//
// 与上面两条合起来构成本次修复的完整边界——409 只在「确实是停机」时给，
// 其余连不上仍然如实报告为服务不可用，不能被顺手改成 409 掩盖故障。
func TestOpenConsoleRunningButUnreachableIsUnavailable(t *testing.T) {
	client := &streamingClient{stubClient: newStubClient(),
		openErr: errors.New("节点不在线")}
	svc, _, db := newTestEnvWithClient(t, client)
	ctx := context.Background()

	owner := int64(7)
	row := model.VM{NodeID: 1, Name: "console-unreachable", Status: model.VMStatusRunning,
		OwnerID: &owner, DiskGB: 20, Present: true,
		VNCEnabled: true, VideoModel: "virtio"}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	_, _, err := svc.OpenConsole(ctx, row.ID, 7, authz.Viewer{UserID: 7})
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 503 {
		t.Fatalf("运行中但通道不可达应返回 503，实际 %v", err)
	}
}
