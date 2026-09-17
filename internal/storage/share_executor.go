package storage

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// shareParams 是 share.mount 任务的参数。
type shareParams struct {
	Action   string `json:"action"` // mount / unmount
	ShareID  int64  `json:"share_id"`
	VMID     int64  `json:"vm_id"`
	VMName   string `json:"vm_name"`
	HostPath string `json:"host_path"`
	// RootPath 是**校验用的边界**，不是要共享的目录。
	//
	// 它必须随任务一起下发：节点需要自己再校验一次真实路径是否落在根之下
	// （共享目录里的符号链接可以指向外面，而控制面看不见文件系统）。
	// 让节点自己去查一次存储根是不行的——那需要它再次访问控制面的数据库，
	// 而节点与数据库之间不该有这样的依赖。
	RootPath      string `json:"root_path"`
	Tag           string `json:"tag"`
	SecurityModel string `json:"security_model"`
	ReadOnly      bool   `json:"read_only"`
}

// ShareExecutor 执行目录共享的挂载与卸载（F-5-06）。
type ShareExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewShareExecutor 构造执行器。
func NewShareExecutor(db *gorm.DB, client agent.Client) *ShareExecutor {
	return &ShareExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *ShareExecutor) Type() string { return model.TaskShareMount }

// Run 下发挂载/卸载并同步记录状态。
//
// 顺序是**先让节点成功、再改记录**。反过来的话，节点失败时控制面会认为
// 共享已经生效——用户进去宾里找那个挂载点，找不到，然后开始怀疑是自己
// 写错了 tag。
func (e *ShareExecutor) Run(ctx context.Context, t *model.Task) error {
	var p shareParams
	if t.Params == nil {
		return api.Internal()
	}
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[storage] 解析共享任务参数失败 task=%d: %v", t.ID, err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpShareMount,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"action": p.Action, "vm_name": p.VMName,
			"host_path": p.HostPath, "root_path": p.RootPath,
			"tag": p.Tag, "security_model": p.SecurityModel,
			"read_only": p.ReadOnly,
		},
	})
	if err != nil {
		e.dropShare(ctx, p.ShareID, "节点不可达")
		return api.Unavailable("节点不可达，共享未生效")
	}
	if !result.Success {
		// 节点侧的失败原因更准（路径逃逸、tag 重复、目录不存在），原样带出。
		e.dropShare(ctx, p.ShareID, result.Message)
		return api.ValidationFailed(result.Message)
	}

	if p.Action == "unmount" {
		// 卸载成功后**删掉记录**，而不是留一个「已卸载」的状态：
		// 记录的存活期就是这个共享的存活期。历史由审计流水承担，
		// 在这里再留一份只会让人分不清哪条是当前有效的。
		if err := e.db.WithContext(ctx).
			Delete(&model.ShareMount{}, p.ShareID).Error; err != nil {
			log.Printf("[storage] 删除共享记录失败 id=%d: %v", p.ShareID, err)
			return api.Internal()
		}
		log.Printf("[storage] 共享已卸载 vm=%s tag=%s", p.VMName, p.Tag)
		return nil
	}

	now := time.Now()
	if err := e.db.WithContext(ctx).Model(&model.ShareMount{}).
		Where("id = ?", p.ShareID).
		Update("mounted_at", now).Error; err != nil {
		log.Printf("[storage] 回写共享状态失败 id=%d: %v", p.ShareID, err)
		return api.Internal()
	}

	log.Printf("[storage] 共享已生效 vm=%s tag=%s path=%s ro=%v",
		p.VMName, p.Tag, p.HostPath, p.ReadOnly)
	return nil
}

// dropShare 在下发失败时删掉记录。
//
// **不留一条「失败」状态的记录**：一条挂载失败的共享本来就不存在，留下
// 记录只会让用户卡在「tag 已存在」上，而他实际什么都没挂上，也没有任何
// 界面路径能清掉那条记录。失败原因由任务承担——那里才是排查的入口。
func (e *ShareExecutor) dropShare(ctx context.Context, shareID int64, message string) {
	if shareID == 0 {
		return
	}
	if err := e.db.WithContext(ctx).
		Delete(&model.ShareMount{}, shareID).Error; err != nil {
		log.Printf("[storage] 清理失败的共享记录出错 id=%d: %v", shareID, err)
	}
	log.Printf("[storage] 共享下发失败，已清理记录 id=%d: %s", shareID, message)
}
