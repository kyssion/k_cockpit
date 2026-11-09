// userdata_root.go：把「我的存储」的根目录收敛到默认存储池里。
//
// 背景（实机抓出）：节点的用户存储根此前写死在系统盘
// （/var/lib/k_cockpit/userdata），即使建了 15G 的数据池也用不上——上传的
// 600MB 镜像写进系统盘，"镜像写满根分区"会直接拖垮节点。
//
// 期望态由这里算：**默认存储池的 <mount_path>/userdata**；没有默认池时回落
// 到 agent 的历史默认根。控制面只负责下发期望值，节点侧负责校验（必须落在
// 池挂载点之下）与落盘。
package pool

import (
	"context"
	"errors"
	"log"
	"path"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// userdataSubdir 是用户存储在池内的子目录名。
//
// 用固定名字而不是散落在池根：池根下还有 lost+found 之类的东西，用户的
// 文件不该与它们混在一起，清点容量时也更容易说清"这一块是用户的"。
const userdataSubdir = "userdata"

// UserdataRootOf 返回节点上「我的存储」应有的根目录。
//
// 空串表示"没有可用的默认池，回到 agent 的历史默认根"。
func UserdataRootOf(ctx context.Context, db *gorm.DB, nodeID int64) (string, error) {
	var p model.StoragePool
	err := db.WithContext(ctx).
		Where("node_id = ? AND is_default = ? AND status = ? AND deleted_at IS NULL",
			nodeID, true, model.StoragePoolReady).
		First(&p).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	if p.MountPath == nil || *p.MountPath == "" {
		return "", nil
	}
	// 这是**被管宿主机**上的路径，一律 path 语义：用 filepath 会在控制面
	// 跑在 Windows 时把 /srv/... 变成 \srv\...（架构文档里的硬规则）。
	return path.Join(*p.MountPath, userdataSubdir), nil
}

// SyncUserdataRoot 把节点上的用户存储根收敛到期望值（幂等，一次往返）。
//
// 失败只由调用方记日志：用户存储根是**可收敛的配置**，一次下发失败不该
// 让建池/刷新这类主流程失败——下一轮会自动纠正。
func SyncUserdataRoot(ctx context.Context, db *gorm.DB, client agent.Client, nodeID int64) error {
	root, err := UserdataRootOf(ctx, db, nodeID)
	if err != nil {
		return err
	}
	return dispatchUserdataRoot(ctx, client, nodeID, root)
}

// dispatchUserdataRoot 下发一次根目录设置。
func dispatchUserdataRoot(ctx context.Context, client agent.Client, nodeID int64, root string) error {
	if client == nil {
		return nil
	}
	result, err := client.Execute(ctx, agent.Operation{
		Kind:   agent.OpStorageRootSet,
		NodeID: nodeID,
		Target: "storage",
		Params: map[string]any{"path": root},
	})
	if err != nil {
		return err
	}
	if !result.Success {
		return errors.New(result.Message)
	}
	if prev, _ := result.Data["root"].(string); prev != "" && prev != root {
		log.Printf("[storage] 节点 %d 用户存储根已切换为 %s", nodeID, prev)
	}
	return nil
}
