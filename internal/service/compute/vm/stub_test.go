package vm_test

import (
	"context"
	"time"

	"k_cockpit/internal/agent"
)

// stubClient 是本包测试用的最小 agent 替身。
//
// 只模拟控制面逻辑真正依赖的返回值（状态探测、创建结果、磁盘列表、
// 控制台截帧、域扫描）；其余操作一律成功，具体数据由用例通过 exec 覆盖。
type stubClient struct {
	exec func(context.Context, agent.Operation) (*agent.Result, error)
}

func newStubClient() *stubClient { return &stubClient{} }

func (s *stubClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if s.exec != nil {
		return s.exec(ctx, op)
	}
	switch op.Kind {
	case agent.OpVMStatus:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.StatusDataKey: "running",
		}}, nil
	case agent.OpVMCreate:
		return &agent.Result{Success: true, Data: map[string]any{
			"uuid": "11111111-2222-3333-4444-555555555555", agent.StatusDataKey: "stopped",
		}}, nil
	case agent.OpVMClone:
		info := agent.CloneInfo{DiskPath: "/var/lib/k_cockpit/images/stub.qcow2"}
		if mode, _ := op.Params["clone_mode"].(string); mode == "linked" {
			info.BackingPath, _ = op.Params["template_disk_path"].(string)
		}
		return &agent.Result{Success: true, Data: map[string]any{
			"uuid": "11111111-2222-3333-4444-555555555555", agent.StatusDataKey: "stopped",
			agent.CloneDataKey: info,
		}}, nil
	case agent.OpVMAdopt:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.VMAdoptDataKey: agent.VMAdoptInfo{UUID: "11111111-2222-3333-4444-555555555555"},
		}}, nil
	case agent.OpVMExport:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.ExportDataKey: agent.ExportInfo{
				FilePath: "/var/lib/k_cockpit/exports/stub.qcow2",
				FileName: "stub.qcow2", SizeBytes: 1 << 20,
			},
		}}, nil
	case agent.OpVMExportFetch:
		data := []byte("stub-export")
		return &agent.Result{Success: true, Data: map[string]any{
			agent.ExportContentKey: agent.ExportContent{Data: data, MIME: "application/octet-stream"},
			"total_size":           float64(len(data)),
		}}, nil
	case agent.OpVMInitReady:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.InitReadyDataKey: agent.InitReadyInfo{Ready: true},
		}}, nil
	case agent.OpMigrateAssess:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.MigrateAssessDataKey: agent.MigrateAssessInfo{
				BandwidthMbps: 937, Source: agent.AssessSourceSpeedtest, DirtyRateMBps: 40,
			},
		}}, nil
	case agent.OpVMReinstall:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.ReinstallDataKey: agent.ReinstallInfo{
				BackupPath: "/var/lib/k_cockpit/images/vm-backup.qcow2",
				DiskPath:   "/var/lib/k_cockpit/images/vm.qcow2",
			},
		}}, nil
	case agent.OpVMDiskList:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.VMDiskListDataKey: []agent.VMDisk{
				{Dev: "vda", CapacityGB: 20, Format: "qcow2", Bus: "virtio", IsSystem: true},
				{Dev: "vdb", CapacityGB: 10, Format: "qcow2", Bus: "virtio", Hotpluggable: true},
			},
		}}, nil
	case agent.OpVMConsoleFrame:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.FrameDataKey: agent.ConsoleFrame{
				MIME: "image/png", Data: []byte("stub-frame"), Width: 640, Height: 400,
			},
		}}, nil
	case agent.OpNodeDomains:
		return &agent.Result{Success: true, Data: map[string]any{
			agent.NodeDomainsDataKey: []agent.DomainInfo{
				{Name: "legacy-ubuntu-1804", State: "running", VCPU: 2, MemoryMB: 4096, DiskGB: 60},
				{Name: "bare-win2022", State: "stopped", VCPU: 4, MemoryMB: 8192, DiskGB: 120},
			},
		}}, nil
	default:
		return &agent.Result{Success: true, Message: "stub"}, nil
	}
}

func (s *stubClient) Snapshot(context.Context, int64) (*agent.Snapshot, error) {
	return &agent.Snapshot{Status: agent.StatusOnline, LastHeartbeat: time.Now()}, nil
}

func (s *stubClient) OpenStream(context.Context, agent.StreamKind, int64, string) (agent.Stream, error) {
	return nil, agent.ErrStreamUnsupported
}
