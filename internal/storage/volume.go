package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// VolumeView 是存储卷的对外视图。
type VolumeView struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`

	SizeGB      int `json:"size_gb"`
	StripeCount int `json:"stripe_count"`
	MirrorCount int `json:"mirror_count"`

	// PhysicalGB 是**实际占用的物理空间**。
	//
	// 与 SizeGB 分开给：镜像会把每份数据写多遍，因此物理占用是可用容量的
	// MirrorCount 倍。只给 SizeGB 的话，用户会以为「还能再建一个这么大的
	// 卷」，而物理盘早就满了。
	PhysicalGB int `json:"physical_gb"`

	// HasRedundancy 为 true 表示能容忍一块盘故障。
	//
	// **只有镜像能**，条带完全不能。把这个判断从文案变成字段，界面才能
	// 无条件地把它显示出来——而不是依赖某个页面记得写这句话。
	HasRedundancy bool `json:"has_redundancy"`

	Devices []string `json:"devices"`
	VGName  string   `json:"vg_name,omitempty"`
	LVName  string   `json:"lv_name,omitempty"`

	Status string `json:"status"`
	// StatusNote 用人话解释当前状态，尤其是 sync 与 degraded。
	StatusNote string `json:"status_note,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// VolumeRequest 是创建存储卷的请求。
type VolumeRequest struct {
	NodeID int64
	Name   string
	SizeGB int
	// StripeCount / MirrorCount 为 0 或 1 表示不使用该特性。
	StripeCount int
	MirrorCount int
	Devices     []string
}

// VolumePlan 是创建前的换算结果。
//
// 它存在的理由是**条带与镜像的组合有一个直觉容易出错的乘法**，以及容量
// 与物理占用不是一回事。这些不放到界面上算，而是由服务端给出——因为
// 「需要几块盘」「实际占多少」是约束的一部分，让界面自己推会出现两处
// 各算一遍、迟早对不上的情况。
type VolumePlan struct {
	// RequiredDevices 是聚合**至少**需要的设备数（stripe × mirror）。
	RequiredDevices int `json:"required_devices"`
	// GivenDevices 是本次提供了几块。
	GivenDevices int `json:"given_devices"`
	SizeGB       int `json:"size_gb"`
	// PhysicalGB 是实际占用的物理空间。
	PhysicalGB int `json:"physical_gb"`
	// HasRedundancy 表示建成后能否容忍一块盘故障。
	HasRedundancy bool `json:"has_redundancy"`
	// Warnings 是需要注意的地方；有内容时创建需要显式确认。
	Warnings []string `json:"warnings,omitempty"`
}

// ListVolumes 返回节点上的存储卷。
func (s *Service) ListVolumes(ctx context.Context, nodeID int64) ([]VolumeView, error) {
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return nil, err
	}
	var rows []model.StorageVolume
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("id ASC").Find(&rows).Error; err != nil {
		log.Printf("[storage] 查询存储卷失败: %v", err)
		return nil, api.Internal()
	}
	views := make([]VolumeView, 0, len(rows))
	for i := range rows {
		views = append(views, toVolumeView(&rows[i]))
	}
	return views, nil
}

// PreviewVolume 计算这次创建会得到什么，不产生任何改动。
//
// 它同时被界面用来在用户填参数时实时显示"需要几块盘、实际占多少、
// 有没有冗余"——那三个数字正是这块功能最容易想错的地方。
func (s *Service) PreviewVolume(ctx context.Context, req VolumeRequest) (*VolumePlan, error) {
	if err := s.ensureNodeUsable(ctx, req.NodeID); err != nil {
		return nil, err
	}
	return s.planVolume(ctx, req)
}

// CreateVolume 创建存储卷。
//
// 有警告且未确认时**不创建、也不报错**，而是把计划原样返回——与防火墙、
// 端口镜像同一套语义：那是一个需要用户做决定的岔路口，不是一次失败。
func (s *Service) CreateVolume(
	ctx context.Context, req VolumeRequest, acknowledge bool,
	v authz.Viewer, operatorName, clientIP string,
) (*VolumePlan, *model.Task, error) {
	plan, err := s.planVolume(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if len(plan.Warnings) > 0 && !acknowledge {
		return plan, nil, nil
	}

	devices, err := s.resolveDevices(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	vg, lv := volumeNames(req.Name)
	volume := model.StorageVolume{
		NodeID: req.NodeID, Name: req.Name, Kind: model.VolumeKindLVM,
		VGName: &vg, LVName: &lv,
		SizeGB: req.SizeGB, StripeCount: req.StripeCount, MirrorCount: req.MirrorCount,
		// 初始状态按**是否真的会有同步阶段**来定。
		//
		// 镜像卷建好之后要把数据复制到第二份上，那确实要几小时，期间写明显
		// 变慢——先标成 active 会让用户在这几小时里认为卷是好的、慢是别的
		// 原因。而单盘卷没有这个过程，标 sync 只是在说一句很快就会变成假话
		// 的话。
		//
		// 两者最终都由执行器按节点回报改写。
		Status: initialStatus(req.MirrorCount),
	}
	joined := strings.Join(devices, "\n")
	volume.Devices = &joined

	if err := s.db.WithContext(ctx).Create(&volume).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, nil, api.Conflict("该节点下已有同名存储卷")
		}
		log.Printf("[storage] 创建存储卷失败: %v", err)
		return nil, nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStorageVolumeApply,
		NodeID:       req.NodeID,
		ResourceType: "storage_volume",
		ResourceID:   volume.ID,
		ResourceName: volume.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			// **action 不能省。** 执行器用它区分创建与删除，缺了它就会走
			// 默认分支——而默认是 create。表现是一次删除操作**静默地变成
			// 了创建**：记录被删掉、设备却从没释放，而任务显示成功。
			"action":    "create",
			"volume_id": volume.ID, "name": volume.Name,
			"vg_name": vg, "lv_name": lv,
			"size_gb": req.SizeGB, "stripe_count": req.StripeCount,
			"mirror_count": req.MirrorCount, "devices": devices,
		},
	})
	if err != nil {
		log.Printf("[storage] 存储卷任务入队失败: %v", err)
		return nil, nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "storage_volume",
		ResourceID: volume.ID, ResourceName: volume.Name,
		Action: "storage_volume.create",
		Params: map[string]any{
			"size_gb": req.SizeGB, "stripe_count": req.StripeCount,
			"mirror_count": req.MirrorCount, "devices": devices,
			"physical_gb": plan.PhysicalGB, "has_redundancy": plan.HasRedundancy,
			"acknowledged_warnings": plan.Warnings,
		},
		Success: true, ClientIP: clientIP,
	})
	return plan, t, nil
}

// DeleteVolume 删除存储卷。
//
// **会销毁卷里的全部数据**，且不可恢复。这是它与存储池最不同的一点：
// 池是设备的组织方式，删掉只是"不再用这几块盘"；而卷里装的是真实数据。
func (s *Service) DeleteVolume(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	var volume model.StorageVolume
	if err := s.db.WithContext(ctx).
		Where("id = ?", id).First(&volume).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("存储卷不存在")
		}
		log.Printf("[storage] 查询存储卷失败: %v", err)
		return nil, api.Internal()
	}
	if err := s.ensureNodeUsable(ctx, volume.NodeID); err != nil {
		return nil, err
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskStorageVolumeApply,
		NodeID:       volume.NodeID,
		ResourceType: "storage_volume",
		ResourceID:   volume.ID,
		ResourceName: volume.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action":    "delete",
			"volume_id": volume.ID, "name": volume.Name,
			"vg_name": derefStr(volume.VGName), "lv_name": derefStr(volume.LVName),
			"devices": volume.DeviceList(),
		},
	})
	if err != nil {
		log.Printf("[storage] 存储卷删除任务入队失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: volume.NodeID, ResourceType: "storage_volume",
		ResourceID: volume.ID, ResourceName: volume.Name,
		Action: "storage_volume.delete",
		Params: map[string]any{
			"size_gb": volume.SizeGB, "mirror_count": volume.MirrorCount,
			// 记下「销毁了多少数据」：事后追查「我的数据什么时候没的」
			// 时，这条是唯一能回答的记录。
			"destroyed_gb": volume.SizeGB,
			"devices":      volume.DeviceList(),
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// --- 内部 ---

// planVolume 做容量与冗余的换算，并给出该提醒的话。
func (s *Service) planVolume(ctx context.Context, req VolumeRequest) (*VolumePlan, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, api.InvalidParameter("必须填写卷名称")
	}
	if len(name) > 64 {
		return nil, api.InvalidParameter("卷名称最长 64 字")
	}
	if req.SizeGB <= 0 {
		return nil, api.InvalidParameter("容量必须大于 0")
	}
	if req.StripeCount < 0 || req.MirrorCount < 0 {
		return nil, api.InvalidParameter("条带数与镜像数不能为负")
	}
	if req.MirrorCount > 4 {
		// 上限不是技术限制而是理性限制：4 份以上很少有意义，而每多一份
		// 就多占一份空间——用户多半是误填。
		return nil, api.InvalidParameter("镜像份数最多 4 份")
	}

	probe := model.StorageVolume{StripeCount: req.StripeCount, MirrorCount: req.MirrorCount}
	plan := &VolumePlan{
		RequiredDevices: probe.DeviceCount(),
		GivenDevices:    len(req.Devices),
		SizeGB:          req.SizeGB,
		PhysicalGB:      req.SizeGB * maxInt(req.MirrorCount, 1),
		HasRedundancy:   probe.HasRedundancy(),
	}

	if len(req.Devices) < plan.RequiredDevices {
		return nil, api.ValidationFailed(fmt.Sprintf(
			"需要至少 %d 块物理盘（条带 %d × 镜像 %d），当前只有 %d 块",
			plan.RequiredDevices, maxInt(req.StripeCount, 1),
			maxInt(req.MirrorCount, 1), len(req.Devices)))
	}

	// **条带没有冗余**——这是这块功能里最需要说清的一件事，也是几乎所有人
	// 都会有的误解：看到「用了 4 块盘」很自然会以为那是"4 块盘的冗余"。
	if req.StripeCount > 1 && req.MirrorCount <= 1 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"条带把数据分散在 %d 块盘上以提升吞吐，但它没有冗余——"+
				"任何一块盘故障都会让整个卷不可用，且因为数据是分散的，"+
				"剩下那些盘上的内容也无法单独恢复。"+
				"要容忍坏盘请把镜像份数设为 2（可用容量不变，物理占用翻倍）。",
			req.StripeCount))
	}

	// 镜像占用的物理空间。
	if req.MirrorCount > 1 {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf(
			"镜像 %d 份会占用 %d GB 物理空间（可用容量仍是 %d GB），"+
				"并且建好之后需要一段初始同步时间——期间读正常、写会明显变慢。",
			req.MirrorCount, plan.PhysicalGB, req.SizeGB))
	}

	return plan, nil
}

// resolveDevices 校验设备可用性并返回规范化列表。
func (s *Service) resolveDevices(ctx context.Context, req VolumeRequest) ([]string, error) {
	devices := make([]string, 0, len(req.Devices))
	seen := map[string]bool{}
	for _, raw := range req.Devices {
		d := strings.TrimSpace(raw)
		if d == "" {
			continue
		}
		if !strings.HasPrefix(d, "/dev/") {
			return nil, api.InvalidParameter("设备必须是 /dev/ 下的路径：" + d)
		}
		// 同一块盘出现两次：LVM 会拒绝，但报错来自 lvm 命令、与「你选重了」
		// 联系不起来。而且它看起来像"我选了 4 块盘"，实际只有 3 块。
		if seen[d] {
			return nil, api.InvalidParameter("同一块设备被选了两次：" + d)
		}
		seen[d] = true
		devices = append(devices, d)
	}
	sort.Strings(devices)

	occupied, err := s.occupiedDevices(ctx, req.NodeID)
	if err != nil {
		return nil, err
	}
	// 已被存储池或其它卷占用的盘不能再进聚合：一个块设备同时属于两处会
	// 让两边都写出错乱的数据，而这种损坏**不会立刻报错**——它要等到文件
	// 系统层面的元数据互相覆盖时才暴露，那时已经很难判断是谁写的。
	var volumes []model.StorageVolume
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", req.NodeID).
		Find(&volumes).Error; err != nil {
		log.Printf("[storage] 查询已有卷失败: %v", err)
		return nil, api.Internal()
	}
	for i := range volumes {
		for _, d := range volumes[i].DeviceList() {
			if _, exists := occupied[d]; !exists {
				occupied[d] = "存储卷 " + volumes[i].Name
			}
		}
	}

	for _, d := range devices {
		// 查两次：存储池的 DeviceID 存的是**裸名**（sdb），而这里拿到的是
		// 路径（/dev/sdb）。只按路径查会永远查不到，于是「盘已被占用」这条
		// 检查形同虚设——而它的失效方式是静默的：两个聚合写到同一块盘上，
		// 要等到文件系统元数据互相覆盖才暴露。
		if owner, taken := occupied[d]; taken {
			return nil, api.Conflict("设备 " + d + " 已被「" + owner + "」使用")
		}
		if owner, taken := occupied[deviceBase(d)]; taken {
			return nil, api.Conflict("设备 " + d + " 已被「" + owner + "」使用")
		}
	}
	return devices, nil
}

// deviceBase 取出设备的裸名（/dev/sdb → sdb）。
func deviceBase(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// initialStatus 决定新建卷的初始状态。
func initialStatus(mirrorCount int) string {
	if mirrorCount > 1 {
		return model.VolumeSync
	}
	return model.VolumeActive
}

// volumeNames 把用户取的名称映射成合法的 LVM 名称。
//
// LVM 的名称有字符集限制（字母数字与 . _ - +），而用户取的名字可能带空格、
// 中文或斜杠。直接用会导致建不出来，而报错来自 lvm 命令、与名字里的那个
// 空格毫无关系。因此这里生成一个**稳定的合法名称**，用户的原始名称只
// 用于展示。
func volumeNames(name string) (vg, lv string) {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "volume"
	}
	// LVM 名称最长 128，留足前缀与后缀的余量。
	if len(slug) > 48 {
		slug = slug[:48]
	}
	return "kc-vg-" + slug, "kc-lv-" + slug
}

// toVolumeView 组装对外视图。
func toVolumeView(v *model.StorageVolume) VolumeView {
	view := VolumeView{
		ID: v.ID, NodeID: v.NodeID, Name: v.Name, Kind: v.Kind,
		SizeGB: v.SizeGB, StripeCount: v.StripeCount, MirrorCount: v.MirrorCount,
		PhysicalGB:    v.PhysicalGB(),
		HasRedundancy: v.HasRedundancy(),
		Devices:       v.DeviceList(),
		Status:        v.Status,
		StatusNote:    volumeStatusNote(v),
		CreatedAt:     v.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	view.VGName = derefStr(v.VGName)
	view.LVName = derefStr(v.LVName)
	return view
}

// volumeStatusNote 用人话解释状态。
//
// 尤其重要的是 sync 与 degraded：前者会让写变慢好几小时，后者**看起来
// 一切正常**却已经没有任何冗余了——这两种情况如果只显示一个英文单词，
// 用户不会知道要不要做什么。
func volumeStatusNote(v *model.StorageVolume) string {
	switch v.Status {
	case model.VolumeSync:
		if v.MirrorCount > 1 {
			return "镜像正在初始同步：读正常，写会明显变慢，同步完成后自动转为正常"
		}
		return "卷正在初始化"
	case model.VolumeDegraded:
		// 这句必须说透：它看起来正常，但再坏一块就全丢。
		return "镜像缺失一份，卷仍可用但已无冗余——此时再坏一块盘会丢失全部数据"
	case model.VolumeFailed:
		return "设备出错或聚合已损坏，卷中的数据可能无法访问"
	case model.VolumeActive:
		if v.StripeCount > 1 && v.MirrorCount <= 1 {
			return "正常（条带，无冗余）"
		}
		if v.MirrorCount > 1 {
			return "正常（镜像，可容忍一块盘故障）"
		}
		return "正常"
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
