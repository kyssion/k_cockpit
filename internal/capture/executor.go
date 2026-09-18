package capture

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// captureParams 是抓包任务的参数。
type captureParams struct {
	CaptureID   int64  `json:"capture_id"`
	Interface   string `json:"interface"`
	Filter      string `json:"filter"`
	DurationSec int    `json:"duration_sec"`
}

// Executor 执行抓包与文件删除（F-4-12）。
type Executor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewExecutor 构造执行器。
func NewExecutor(db *gorm.DB, client agent.Client) *Executor {
	return &Executor{db: db, agent: client}
}

// Type 返回处理的任务类型（抓包）。
func (e *Executor) Type() string { return model.TaskNetworkCapture }

// DeleteExecutor 负责删除节点上的抓包文件。
//
// 单独一个执行器：两者虽然相关，但**失败的含义完全不同**——抓包失败是
// "没抓到"，删除失败是"一份含明文流量的文件还留在宿主机上"。混在一起
// 会让后者被当成前者而被忽略。
type DeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDeleteExecutor 构造删除执行器。
func NewDeleteExecutor(db *gorm.DB, client agent.Client) *DeleteExecutor {
	return &DeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DeleteExecutor) Type() string { return model.TaskNetworkCaptureDelete }

// Run 下发抓包。
func (e *Executor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p captureParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[capture] 解析参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkCapture,
		NodeID: nodeID,
		Target: p.Interface,
		Params: map[string]any{
			"interface":    p.Interface,
			"filter":       p.Filter,
			"duration_sec": p.DurationSec,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，抓包未开始")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	info, _ := result.Data[agent.CaptureDataKey].(agent.CaptureInfo)
	updates := map[string]any{"size_bytes": info.SizeBytes}
	if info.FilePath != "" {
		updates["file_path"] = info.FilePath
	}
	if err := e.db.WithContext(ctx).Model(&model.NetworkCapture{}).
		Where("id = ?", p.CaptureID).Updates(updates).Error; err != nil {
		log.Printf("[capture] 回写结果失败 id=%d: %v", p.CaptureID, err)
	}
	return nil
}

// Run 下载/删除节点上的文件。
func (e *DeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p struct {
		CaptureID int64  `json:"capture_id"`
		FilePath  string `json:"file_path"`
	}
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpNetworkCaptureDelete,
		NodeID: nodeID,
		Target: p.FilePath,
	})
	if err != nil {
		return api.Unavailable("节点不可达，抓包文件未删除")
	}
	if !result.Success {
		// **删除失败保留记录。** 文件还在宿主机上，而界面上如果看不见它了，
		// 就再没有人会去删——一份含明文流量的文件会就此长期留存。
		return api.ValidationFailed(result.Message)
	}

	// 节点确认删掉之后才删控制面记录。
	if err := e.db.WithContext(ctx).
		Delete(&model.NetworkCapture{}, p.CaptureID).Error; err != nil {
		log.Printf("[capture] 删除记录失败 id=%d: %v", p.CaptureID, err)
	}
	return nil
}

// 编译期断言：装饰性引用，确保 time 被用到（过期判断在服务层）。
var _ = time.Now
