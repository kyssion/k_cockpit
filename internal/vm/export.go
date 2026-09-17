package vm

import (
	"context"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// ExportView 是一次导出的对外视图。
type ExportView struct {
	ID     int64  `json:"id"`
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	Format string `json:"format"`
	Status string `json:"status"`
	// IncludeDataDisks 表示是否连同数据盘一起导出。
	IncludeDataDisks bool `json:"include_data_disks"`
	// FileName 只在导出完成后有值——**进行中时不给文件名**：给了会让界面
	// 显示一个可以点击、点了却拿不到东西的链接。
	FileName string `json:"file_name,omitempty"`
	// SizeBytes 计入用户的存储配额（f-2-14）。
	SizeBytes int64  `json:"size_bytes"`
	Error     string `json:"error,omitempty"`

	CreatedAt  string `json:"created_at"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// ExportRequest 是一次导出请求。
type ExportRequest struct {
	// Format 取值 qcow2 / ova。
	Format string
	// IncludeDataDisks 是否连同数据盘一起导出；默认不包含。
	IncludeDataDisks bool
}

// Export 受理一次导出（F-2-14）。
//
// **要求关机**，理由比其它操作更强：导出的产物会被搬到别的地方使用
// （导入到另一套环境、当作模板、交给别人），因此它必须是一个干净的、自洽的
// 镜像。运行中导出得到的是崩溃一致性快照——而导入方往往不在你手边，
// 出了问题很难回头找原因。
func (s *Service) Export(
	ctx context.Context, id int64, req ExportRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}

	switch req.Format {
	case model.ExportQCOW2, model.ExportOVA:
	default:
		return nil, api.InvalidParameter("导出格式非法，可选 qcow2 或 ova")
	}

	// 实时探测，不用投影（f-2-01 R-002）。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		return nil, err
	}
	if current != model.VMStatusStopped {
		return nil, api.ValidationFailed(
			"导出需要先关机（当前：" + DescribeStatus(current) + "）。" +
				"导出产物会被搬到别处使用，运行中导出得到的是不一致的镜像")
	}

	active, err := s.hasActiveTask(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	// 配额校验用**弱口径**（只判当前是否已超），因为产物大小在受理时
	// 根本无法知道——它取决于盘里真正写了多少数据。假装能算准只会给出
	// 一个看起来精确、实际误导的拒绝理由。
	if s.quota != nil {
		if err := s.quota.CheckOverQuota(ctx, ownerOf(target, v), target.NodeID); err != nil {
			return nil, err
		}
	}

	// 同一台虚拟机同时只允许一个导出：多个导出会同时读同一块盘，既拖慢
	// 彼此，也会让产物的完成时间集中在同一段时间——而它们占用的是同一份
	// 用户配额。
	var running int64
	if err := s.db.WithContext(ctx).Model(&model.VMExport{}).
		Where("vm_id = ? AND status IN ?", target.ID,
			[]string{model.ExportPending, model.ExportRunning}).
		Count(&running).Error; err != nil {
		log.Printf("[vm] 统计进行中的导出失败: %v", err)
		return nil, api.Internal()
	}
	if running > 0 {
		return nil, api.Conflict("该虚拟机已有正在进行中的导出，请等待它完成")
	}

	// 导出记录在**受理时**就创建（状态 pending），而不是等执行完再建。
	//
	// 与模板/存储池的「先让宿主机成功、再写记录」相反，这里刻意提前：
	// 导出是**长时间运行**的任务（可能几十分钟），用户需要在那段时间里看到
	// 「有一个导出在进行」；否则界面上什么都没有，他会以为刚才那一下没点上，
	// 转头再点一次。
	row := model.VMExport{
		VMID:             target.ID,
		NodeID:           target.NodeID,
		VMName:           target.Name,
		Format:           req.Format,
		IncludeDataDisks: req.IncludeDataDisks,
		Status:           model.ExportPending,
		CreatedBy:        &v.UserID,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[vm] 创建导出记录失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMExport,
		NodeID:       target.NodeID,
		ResourceType: "vm",
		ResourceID:   target.ID,
		ResourceName: target.Name,
		OwnerID:      ownerOf(target, v),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"export_id":          row.ID,
			"vm_id":              target.ID,
			"vm_name":            target.Name,
			"format":             req.Format,
			"include_data_disks": req.IncludeDataDisks,
			"observed_status":    current,
		},
	})
	if err != nil {
		// 入队失败要把刚建的记录清掉，否则会留下一条永远停在 pending 的
		// 导出——用户既等不到它完成，也无法解释它是怎么来的。
		if err := s.db.WithContext(ctx).Delete(&model.VMExport{}, row.ID).Error; err != nil {
			log.Printf("[vm] 回滚导出记录失败 id=%d: %v", row.ID, err)
		}
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: target.NodeID, ResourceType: "vm", ResourceID: target.ID,
		ResourceName: target.Name, Action: "vm.export",
		Params: map[string]any{
			"task_id": t.ID, "export_id": row.ID, "format": req.Format,
			"include_data_disks": req.IncludeDataDisks,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// ListExports 返回一台虚拟机的导出记录。
func (s *Service) ListExports(
	ctx context.Context, id int64, v authz.Viewer,
) ([]ExportView, error) {
	// 先做归属检查：直接查导出表会绕过「这台虚拟机是不是你的」。
	if _, err := s.load(ctx, id, v); err != nil {
		return nil, err
	}

	var rows []model.VMExport
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", id).
		Order("id DESC").
		Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询导出记录失败: %v", err)
		return nil, api.Internal()
	}

	views := make([]ExportView, 0, len(rows))
	for i := range rows {
		views = append(views, toExportView(&rows[i]))
	}
	return views, nil
}

// DeleteExport 删除一个导出产物。
//
// **不需要二次验证**：产物是导出的副本，删掉它不影响虚拟机本身的任何东西。
// 给它加验证只会稀释真正危险操作的份量。
func (s *Service) DeleteExport(
	ctx context.Context, vmID, exportID int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	if _, err := s.load(ctx, vmID, v); err != nil {
		return nil, err
	}

	row, err := s.loadExport(ctx, vmID, exportID)
	if err != nil {
		return nil, err
	}
	if row.IsRunning() {
		return nil, api.Conflict("该导出仍在进行中，请等待完成后再删除")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMExportDelete,
		NodeID:       row.NodeID,
		ResourceType: "vm",
		ResourceID:   row.VMID,
		ResourceName: row.VMName,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"export_id": row.ID,
			"file_path": derefStr(row.FilePath),
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "vm", ResourceID: row.VMID,
		ResourceName: row.VMName, Action: "vm.export.delete",
		Params:  map[string]any{"task_id": t.ID, "export_id": row.ID},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// ExportFile 读取导出产物的内容，供接口层转发给用户下载。
//
// 返回文件名与原始字节：**由控制面转发而不是给用户一个节点上的直链**。
// 直链意味着要把节点的访问凭据或一个匿名可访问的地址暴露出去，而产物里是
// 整台机器的数据。转发多花一次带宽，但权限判断留在控制面这一处。
func (s *Service) ExportFile(
	ctx context.Context, vmID, exportID int64, v authz.Viewer,
) (fileName string, data []byte, mime string, err error) {
	if _, err := s.load(ctx, vmID, v); err != nil {
		return "", nil, "", err
	}

	row, err := s.loadExport(ctx, vmID, exportID)
	if err != nil {
		return "", nil, "", err
	}
	if row.Status != model.ExportSuccess {
		return "", nil, "", api.ValidationFailed("该导出尚未完成，暂时无法下载")
	}
	if row.FilePath == nil || *row.FilePath == "" {
		return "", nil, "", api.NotFound("该导出的产物已不在节点上")
	}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMExportFetch,
		NodeID: row.NodeID,
		Target: row.VMName,
		Params: map[string]any{"file_path": derefStr(row.FilePath)},
	})
	if err != nil {
		return "", nil, "", api.Unavailable("节点不可达，无法获取导出产物")
	}
	if !result.Success {
		return "", nil, "", api.ValidationFailed(result.Message)
	}

	content, ok := result.Data[agent.ExportContentKey].(agent.ExportContent)
	if !ok || len(content.Data) == 0 {
		return "", nil, "", api.Unavailable("节点未返回导出产物")
	}

	name := derefStr(row.FileName)
	if name == "" {
		name = fmt.Sprintf("%s.%s", row.VMName, row.Format)
	}
	mime = content.MIME
	if mime == "" {
		// 未声明类型时给一个通用二进制流：**不要**猜成 text/plain——
		// 那会让浏览器试图把镜像当文本渲染。
		mime = "application/octet-stream"
	}
	return name, content.Data, mime, nil
}

// loadExport 读取导出记录并确认它属于指定虚拟机。
func (s *Service) loadExport(
	ctx context.Context, vmID, exportID int64,
) (*model.VMExport, error) {
	var row model.VMExport
	err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", exportID, vmID).
		First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("导出记录不存在")
	case err != nil:
		log.Printf("[vm] 查询导出记录失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func toExportView(row *model.VMExport) ExportView {
	view := ExportView{
		ID: row.ID, VMID: row.VMID, VMName: row.VMName,
		Format: row.Format, Status: row.Status,
		IncludeDataDisks: row.IncludeDataDisks, SizeBytes: row.SizeBytes,
		CreatedAt: row.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if row.FileName != nil {
		view.FileName = *row.FileName
	}
	if row.Error != nil {
		view.Error = *row.Error
	}
	if row.FinishedAt != nil {
		view.FinishedAt = row.FinishedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

// ExportFormatLabel 把格式翻译成用户能读懂的说法。
func ExportFormatLabel(format string) string {
	switch format {
	case model.ExportOVA:
		return "OVA 包"
	case model.ExportQCOW2:
		return "QCOW2 系统盘"
	default:
		return format
	}
}
