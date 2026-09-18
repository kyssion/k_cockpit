package storage

import (
	"context"
	"encoding/json"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// volumeParams 是 storage.volume.create / delete 任务的参数。
type volumeParams struct {
	Action   string `json:"action"` // create / delete
	VolumeID int64  `json:"volume_id"`
	Name     string `json:"name"`
	VGName   string `json:"vg_name"`
	LVName   string `json:"lv_name"`
	SizeGB   int    `json:"size_gb"`
	// StripeCount / MirrorCount 决定聚合方式，也决定**节点侧要跑哪几条
	// 命令**（镜像要多一条 lvconvert，条带要在 lvcreate 上传参）。
	StripeCount int      `json:"stripe_count"`
	MirrorCount int      `json:"mirror_count"`
	Devices     []string `json:"devices"`
}

// VolumeExecutor 执行存储卷的创建与删除（F-5-02）。
type VolumeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewVolumeExecutor 构造执行器。
func NewVolumeExecutor(db *gorm.DB, client agent.Client) *VolumeExecutor {
	return &VolumeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
//
// 创建与删除是同一个类型（由 action 区分），因此这一个执行器负责两者——
// 拆分会让删除那条路径需要再注册一次，而漏注册的表现是「任务永远停在
// 待执行」，不会有任何报错。
func (e *VolumeExecutor) Type() string { return model.TaskStorageVolumeApply }

// Run 下发卷的创建/删除。
//
// 顺序与目录共享一致：**先让节点成功、再改记录**。反过来的话，节点失败时
// 控制面会认为卷已经建好了——用户去看「为什么挂不上」，而真正的问题在
// 另一个地方。
func (e *VolumeExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p volumeParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[storage] 解析存储卷任务参数失败: %v", err)
		return api.Internal()
	}
	if p.Action == "" {
		p.Action = "create"
	}

	if p.Action == "delete" {
		return e.runDelete(ctx, &p)
	}
	return e.runCreate(ctx, &p)
}

func (e *VolumeExecutor) runCreate(ctx context.Context, p *volumeParams) error {
	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpStorageVolumeApply,
		NodeID: nodeIDFromParams(ctx, e.db, p.VolumeID, "storage_volume"),
		Target: p.Name,
		Params: map[string]any{
			"action": "create", "vg_name": p.VGName, "lv_name": p.LVName,
			"size_gb": p.SizeGB, "stripe_count": p.StripeCount,
			"mirror_count": p.MirrorCount, "devices": p.Devices,
		},
	})
	if err != nil {
		e.markVolume(ctx, p.VolumeID, model.VolumeFailed, "节点不可达")
		return api.Unavailable("节点不可达，存储卷未创建")
	}
	if !result.Success {
		e.markVolume(ctx, p.VolumeID, model.VolumeFailed, result.Message)
		return api.ValidationFailed(result.Message)
	}

	// 按节点回报的状态落库，而不是一律写成 active。
	//
	// 镜像卷建好之后还要同步几小时，期间写会明显变慢——把这段时间标成
	// active，用户会认为卷是好的、慢是别的原因，于是一路排查到网络。
	status := model.VolumeActive
	if info, ok := result.Data[agent.VolumeDataKey].(agent.VolumeInfo); ok {
		if info.Status != "" && model.ValidVolumeStatus(info.Status) {
			status = info.Status
		}
	}
	e.markVolume(ctx, p.VolumeID, status, "")
	return nil
}

func (e *VolumeExecutor) runDelete(ctx context.Context, p *volumeParams) error {
	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpStorageVolumeApply,
		NodeID: nodeIDFromParams(ctx, e.db, p.VolumeID, "storage_volume"),
		Target: p.Name,
		Params: map[string]any{
			"action": "delete", "vg_name": p.VGName, "lv_name": p.LVName,
			"devices": p.Devices,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，存储卷未删除")
	}
	if !result.Success {
		// 删除失败**保留记录**：卷还在那儿，而且里面的数据也还在。
		// 把记录删掉会让一块真实存在的盘从界面上消失，而它仍然占用着设备。
		return api.ValidationFailed(result.Message)
	}

	// **硬删除**：表里没有 deleted_at 列，而且删除是用户的明确意图——
	// 卷与其中的数据都已经销毁了，留一条记录与事实不符。设备在这一步
	// 被释放（节点侧已 pvremove），保留记录反而会让那块盘一直显示为
	// 「被占用」，用户再也建不了新卷。
	//
	// 「谁在什么时候删了哪个卷、销毁了多少数据」由审计流水回答。
	if err := e.db.WithContext(ctx).
		Delete(&model.StorageVolume{}, p.VolumeID).Error; err != nil {
		log.Printf("[storage] 删除存储卷记录失败 id=%d: %v", p.VolumeID, err)
	}
	return nil
}

// markVolume 更新卷状态。
func (e *VolumeExecutor) markVolume(ctx context.Context, id int64, status, note string) {
	if id == 0 {
		return
	}
	// 失败原因**不写进记录**（表里没有那一列）：它由任务承担。这里只落
	// 状态，而失败时状态本身已经说明了问题。
	_ = note
	if err := e.db.WithContext(ctx).Model(&model.StorageVolume{}).
		Where("id = ?", id).Update("status", status).Error; err != nil {
		log.Printf("[storage] 更新存储卷状态失败 id=%d: %v", id, err)
	}
}

// nodeIDFromParams 查出该卷所属的节点。
//
// 任务参数里没有 node_id（入队时由 task.Spec 承载，而执行器拿到的是
// spec 展开后的 Params）。这里从卷记录反查——多一次查询，但比在每处
// 入队的地方都记得塞一份 node_id 要可靠。
func nodeIDFromParams(ctx context.Context, db *gorm.DB, volumeID int64, _ string) int64 {
	var v model.StorageVolume
	if err := db.WithContext(ctx).
		Select("node_id").Where("id = ?", volumeID).First(&v).Error; err != nil {
		log.Printf("[storage] 查询卷所属节点失败 id=%d: %v", volumeID, err)
		return 0
	}
	return v.NodeID
}
