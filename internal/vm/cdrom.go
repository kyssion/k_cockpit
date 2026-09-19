package vm

import (
	"context"
	"errors"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// CDROMView 是一个光驱的对外视图。
type CDROMView struct {
	ID      int64  `json:"id"`
	VMID    int64  `json:"vm_id"`
	OrderNo int    `json:"order_no"`
	Bus     string `json:"bus"`
	// Loaded 为 false 表示**光驱在但没放盘**（弹出状态）。
	//
	// 与"没有光驱"是两件事，因此这个字段必须单独给出来——不区分的话，
	// 用户在来宾里找不到设备而不知道是哪一种情况。
	Loaded bool `json:"loaded"`
	// ISOFileID 为空即未放盘。
	ISOFileID *int64 `json:"iso_file_id,omitempty"`
	// ISOName 是挂载的镜像文件名，供界面直接显示。
	ISOName string `json:"iso_name,omitempty"`
	// Device 是预测的来宾内设备名（如 sr0）。
	//
	// **由服务端算好**：它由序号决定，而让界面自己拼（`sr` + order）在
	// 序号从 0 开始时是对的，某天变了就悄悄错掉。
	Device string `json:"device"`
	// MaxCDROMs 提示还能再加几个。
	MaxCDROMs int `json:"max_cdroms"`
}

// MaxCDROMsPerVM 是单台虚拟机的光驱数上限。
//
// 4 个：真实的"多个光驱"需求来自"同时挂安装盘与驱动盘"这类场景，两三个
// 足够；而每多一个光驱就多一条 ide/sata 占用，填满总线之后**加不上磁盘**，
// 而那时的报错来自 libvirt、与"你加了太多光驱"联系不起来。
const MaxCDROMsPerVM = 4

// ListCDROMs 返回虚拟机的光驱。
func (s *Service) ListCDROMs(ctx context.Context, vmID int64, v authz.Viewer) ([]CDROMView, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	var rows []model.VMCDROM
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vm.ID).Order("order_no ASC").Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询光驱失败: %v", err)
		return nil, api.Internal()
	}
	names := s.isoNames(ctx, rows)

	out := make([]CDROMView, 0, len(rows))
	for i := range rows {
		out = append(out, toCDROMView(&rows[i], names))
	}
	return out, nil
}

// AttachCDROM 加一个光驱（可选同时放盘）。
func (s *Service) AttachCDROM(
	ctx context.Context, vmID int64, isoFileID int64, bus string,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if bus == "" {
		bus = model.CDROMBusSATA
	}
	if !model.ValidCDROMBus(bus) {
		return nil, api.InvalidParameter("总线类型只能是 ide / sata / scsi")
	}
	if isoFileID > 0 {
		if err := s.checkISO(ctx, vm.NodeID, isoFileID); err != nil {
			return nil, err
		}
	}

	var count int64
	if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Where("vm_id = ?", vm.ID).Count(&count).Error; err != nil {
		return nil, api.Internal()
	}
	if count >= MaxCDROMsPerVM {
		return nil, api.Conflict(
			"光驱数量已达上限。每多一个光驱就多占一条总线通道，填满之后**加不上磁盘**——" +
				"而那时的报错来自虚拟化层，与「你加了太多光驱」联系不起来。")
	}

	// **序号取当前的 max+1，而不是 count。**
	//
	// 用 count 的话，删掉 0 号之后序号会与现有的一条撞上（原来的 1 号还在，
	// 而 count 又变回 1）——插入直接失败，而报错是一句唯一约束冲突。
	var maxOrder struct {
		M *int
	}
	if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Select("MAX(order_no) as m").Where("vm_id = ?", vm.ID).
		Scan(&maxOrder).Error; err != nil {
		return nil, api.Internal()
	}
	next := 0
	if maxOrder.M != nil {
		next = *maxOrder.M + 1
	}

	row := model.VMCDROM{VMID: vm.ID, NodeID: vm.NodeID, OrderNo: next, Bus: bus}
	if isoFileID > 0 {
		row.ISOFileID = &isoFileID
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[vm] 创建光驱失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.enqueueCDROM(ctx, vm, row, "attach", v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.cdrom.attach",
		Params: map[string]any{
			"order_no": row.OrderNo, "bus": bus, "iso_file_id": isoFileID,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// LoadCDROM 给光驱放盘或换盘。
func (s *Service) LoadCDROM(
	ctx context.Context, vmID, cdromID, isoFileID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	row, vm, err := s.loadCDROM(ctx, vmID, cdromID, v)
	if err != nil {
		return nil, err
	}
	if err := s.checkISO(ctx, vm.NodeID, isoFileID); err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Where("id = ?", row.ID).Update("iso_file_id", isoFileID).Error; err != nil {
		return nil, api.Internal()
	}
	row.ISOFileID = &isoFileID

	t, err := s.enqueueCDROM(ctx, vm, *row, "load", v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action:  "vm.cdrom.load",
		Params:  map[string]any{"order_no": row.OrderNo, "iso_file_id": isoFileID},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// EjectCDROM 弹出光盘。
//
// **弹出不等于移除。** 弹出之后光驱仍在，来宾里看得到一个空的托盘
// （/dev/srN 还在）。两者混为一谈的话，用户在来宾里找不到设备而不知道是
// 哪一种情况——而这两种情况的排查方向完全不同。
func (s *Service) EjectCDROM(
	ctx context.Context, vmID, cdromID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	row, vm, err := s.loadCDROM(ctx, vmID, cdromID, v)
	if err != nil {
		return nil, err
	}
	if !row.Loaded() {
		// 已经是空的：静默成功会让用户以为刚才那次弹出生效了，而它本来
		// 就是空的——那他上一次操作的结果就成了无法解释的事。
		return nil, api.ValidationFailed("这个光驱里没有光盘")
	}
	if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Where("id = ?", row.ID).Update("iso_file_id", nil).Error; err != nil {
		return nil, api.Internal()
	}

	t, err := s.enqueueCDROM(ctx, vm, *row, "eject", v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action:  "vm.cdrom.eject",
		Params:  map[string]any{"order_no": row.OrderNo, "note": "弹出介质，光驱保留"},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// RemoveCDROM 摘掉整个光驱（设备也消失）。
func (s *Service) RemoveCDROM(
	ctx context.Context, vmID, cdromID int64,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	row, vm, err := s.loadCDROM(ctx, vmID, cdromID, v)
	if err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Delete(&model.VMCDROM{}, row.ID).Error; err != nil {
		return nil, api.Internal()
	}

	// **不重排剩下的光驱。** 重排会让它们的序号前移，而来宾里 /dev/sr0 与
	// /dev/sr1 就对调了——用户按上次记的设备名去找会找错。宁可留一个空号。
	t, err := s.enqueueCDROM(ctx, vm, *row, "remove", v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.cdrom.remove",
		Params: map[string]any{
			"order_no": row.OrderNo,
			"note":     "移除整个光驱，且不重排其余光驱的序号",
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// SetCDROMBus 换总线类型。
//
// **几乎一定需要重启**：热插拔的总线支持各不相同（sata 通常可以，ide 与
// scsi 多数不行）。因此这里不假装是即时生效的——结果里会带上 RebootNeeded，
// 而界面据此提示。
func (s *Service) SetCDROMBus(
	ctx context.Context, vmID, cdromID int64, bus string,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	if !model.ValidCDROMBus(bus) {
		return nil, api.InvalidParameter("总线类型只能是 ide / sata / scsi")
	}
	row, vm, err := s.loadCDROM(ctx, vmID, cdromID, v)
	if err != nil {
		return nil, err
	}
	if row.Bus == bus {
		return nil, api.ValidationFailed("总线类型没有变化")
	}
	if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
		Where("id = ?", row.ID).Update("bus", bus).Error; err != nil {
		return nil, api.Internal()
	}

	t, err := s.enqueueCDROM(ctx, vm, *row, "bus", v)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.cdrom.bus",
		Params: map[string]any{
			"order_no": row.OrderNo, "from": row.Bus, "to": bus,
			"note": "换总线类型通常需要重启虚拟机才生效",
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// --- 内部 ---

// checkISO 校验要挂载的文件确实是本节点上的一份 ISO。
//
// **必须校验类别**：storage_file 里还放着普通文件与虚拟磁盘，而把一份
// qcow2 挂成光盘的话，来宾会把它当成一张坏盘——排查方向完全错。
func (s *Service) checkISO(ctx context.Context, nodeID, fileID int64) error {
	var f model.StorageFile
	if err := s.db.WithContext(ctx).First(&f, fileID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("文件不存在")
		}
		return api.Internal()
	}
	if f.Category != model.FileCategoryISO {
		return api.ValidationFailed("只有安装镜像（ISO）可以挂载到光驱上")
	}
	// **必须与虚拟机在同一节点**：光驱是宿主机上的设备，而文件在节点的
	// 存储里。跨节点的话，那个路径在目标宿主机上根本不存在——而报错会是
	// 一句"文件未找到"，与"你选了一个别的节点的镜像"联系不起来。
	if f.NodeID != nodeID {
		return api.ValidationFailed("该镜像不在虚拟机的节点上，无法挂载")
	}
	return nil
}

func (s *Service) loadCDROM(
	ctx context.Context, vmID, cdromID int64, v authz.Viewer,
) (*model.VMCDROM, *model.VM, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, nil, err
	}
	var row model.VMCDROM
	if err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", cdromID, vm.ID).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, api.NotFound("光驱不存在")
		}
		return nil, nil, api.Internal()
	}
	return &row, vm, nil
}

func (s *Service) enqueueCDROM(
	ctx context.Context, vm *model.VM, row model.VMCDROM, action string, v authz.Viewer,
) (*model.Task, error) {
	isoPath := ""
	if row.ISOFileID != nil {
		var f model.StorageFile
		if err := s.db.WithContext(ctx).Select("rel_path").
			First(&f, *row.ISOFileID).Error; err == nil {
			isoPath = f.RelPath
		}
	}
	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMCDROMApply,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action": action, "vm_id": vm.ID, "vm_name": vm.Name,
			"order_no": row.OrderNo, "bus": row.Bus,
			"iso_file_id": row.ISOFileID, "iso_path": isoPath,
		},
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Service) isoNames(ctx context.Context, rows []model.VMCDROM) map[int64]string {
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		if rows[i].ISOFileID != nil {
			ids = append(ids, *rows[i].ISOFileID)
		}
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var files []model.StorageFile
	if err := s.db.WithContext(ctx).Select("id", "filename").
		Where("id IN ?", ids).Find(&files).Error; err != nil {
		log.Printf("[vm] 查询镜像名失败: %v", err)
		return out
	}
	for _, f := range files {
		out[f.ID] = f.Filename
	}
	return out
}

func toCDROMView(c *model.VMCDROM, names map[int64]string) CDROMView {
	v := CDROMView{
		ID: c.ID, VMID: c.VMID, OrderNo: c.OrderNo, Bus: c.Bus,
		Loaded: c.Loaded(), ISOFileID: c.ISOFileID,
		// 设备名由**服务端**算好：它由序号决定，而让界面自己拼（sr + order）
		// 在序号从 0 开始时是对的，某天变了就悄悄错掉。
		Device:    "sr" + itoa(int64(c.OrderNo)),
		MaxCDROMs: MaxCDROMsPerVM,
	}
	if c.ISOFileID != nil {
		v.ISOName = names[*c.ISOFileID]
	}
	return v
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
