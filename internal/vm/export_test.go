package vm_test

import (
	"context"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestExportRequiresStopped 覆盖 F-2-14 的关机要求，理由比其它操作更强。
//
// 导出产物会被**搬到别的地方使用**（导入到另一套环境、当模板、交给别人），
// 因此它必须是一个干净的、自洽的镜像。运行中导出得到的是崩溃一致性快照，
// 而导入方往往不在你手边，出了问题很难回头找原因。
func TestExportRequiresStopped(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ex1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Export(ctx, row.ID, vm.ExportRequest{Format: model.ExportQCOW2},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestExportRejectsBadFormat(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-ex2", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.Export(ctx, row.ID, vm.ExportRequest{Format: "vmdk"},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 400)
}

// TestExportRecordExistsWhileRunning 覆盖一条刻意的顺序选择。
//
// 导出记录**在受理时就创建**（pending），而不是等执行完再建。导出可能跑
// 几十分钟，用户需要在那段时间里看到「有一个导出在进行」；否则界面上什么
// 都没有，他会以为刚才那一下没点上，转头再点一次。
func TestExportRecordExistsWhileRunning(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-ex3", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	if _, err := svc.Export(ctx, row.ID, vm.ExportRequest{Format: model.ExportOVA},
		viewer, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	// 受理后立刻就能查到记录——不必等任务跑完。
	items, err := svc.ListExports(ctx, row.ID, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("导出记录数 = %d, 期望 1（受理时即创建）", len(items))
	}
	if items[0].Format != model.ExportOVA {
		t.Errorf("格式 = %q, 期望 ova", items[0].Format)
	}
}

// TestExportCompletesAndReportsSize 覆盖产物的落地与记账。
//
// SizeBytes 计入用户的存储配额（f-2-14），因此必须在导出完成后**如实记录**
// ——记 0 等于这份占用永远不算数。
func TestExportCompletesAndReportsSize(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-ex4", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	if _, err := svc.Export(ctx, row.ID, vm.ExportRequest{Format: model.ExportQCOW2},
		viewer, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitForExport(t, svc, row.ID, viewer, func(e vm.ExportView) bool {
		return e.Status == model.ExportSuccess
	})

	items, _ := svc.ListExports(ctx, row.ID, viewer)
	got := items[0]
	if got.SizeBytes <= 0 {
		t.Error("未记录产物大小——配额记账会漏掉这份占用")
	}
	if got.FileName == "" {
		t.Error("未给出下载文件名")
	}
	if got.FinishedAt == "" {
		t.Error("未记录完成时间")
	}

	// 产物内容可读（接口层据此转发给用户）。
	name, data, mime, err := svc.ExportFile(ctx, row.ID, got.ID, viewer)
	if err != nil {
		t.Fatalf("读取产物失败: %v", err)
	}
	if name == "" || len(data) == 0 {
		t.Error("产物为空")
	}
	if mime == "" {
		t.Error("未给出媒体类型")
	}
}

// TestExportDownloadRejectedBeforeCompletion 覆盖「没跑完就别给下载」。
func TestExportDownloadRejectedBeforeCompletion(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-ex5", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	// 直接造一条 pending 记录：受理后任务可能已经跑完，不便于稳定复现。
	pending := model.VMExport{
		VMID: row.ID, NodeID: 1, VMName: row.Name,
		Format: model.ExportQCOW2, Status: model.ExportPending,
	}
	db.Create(&pending)

	_, _, _, err := svc.ExportFile(ctx, row.ID, pending.ID, viewer)
	assertAPIError(t, err, 422)
}

func TestExportRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	theirs := model.VM{NodeID: 1, Name: "vm-ex6", Status: model.VMStatusStopped, OwnerID: ptr(int64(20))}
	db.Create(&theirs)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.Export(ctx, theirs.ID, vm.ExportRequest{Format: model.ExportQCOW2},
		authz.Viewer{UserID: 10}, "bob", "")
	assertAPIError(t, err, 404)
}

// TestExportRecordIsScopedToVM 覆盖一条越权路径。
//
// 导出编号是全表递增的，若不校验它属于哪台虚拟机，用户就能拿自己虚拟机的
// ID 加上别人的导出编号去下载——而那不是他的数据。
func TestExportRecordIsScopedToVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	mine := model.VM{NodeID: 1, Name: "vm-ex7a", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	other := model.VM{NodeID: 1, Name: "vm-ex7b", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&mine)
	db.Create(&other)

	foreign := model.VMExport{
		VMID: other.ID, NodeID: 1, VMName: other.Name,
		Format: model.ExportQCOW2, Status: model.ExportSuccess,
	}
	db.Create(&foreign)

	// 用「我的虚拟机 + 别人的导出编号」去下载。
	_, _, _, err := svc.ExportFile(ctx, mine.ID, foreign.ID, viewer)
	assertAPIError(t, err, 404)
}

func TestDeleteExportRejectedWhileRunning(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-ex8", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	running := model.VMExport{
		VMID: row.ID, NodeID: 1, VMName: row.Name,
		Format: model.ExportQCOW2, Status: model.ExportRunning,
	}
	db.Create(&running)

	_, err := svc.DeleteExport(ctx, row.ID, running.ID, viewer, "alice", "")
	assertAPIError(t, err, 409)
}

// --- 辅助 ---

func waitForExport(
	t *testing.T, svc *vm.Service, vmID int64, viewer authz.Viewer,
	cond func(vm.ExportView) bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := svc.ListExports(context.Background(), vmID, viewer)
		if err == nil && len(items) > 0 && cond(items[0]) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待导出完成超时")
}
