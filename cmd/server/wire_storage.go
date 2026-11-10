// wire_storage 装配 internal/service/storage 域：存储池、用户存储与存储配额。
//
// quota 排在域内第一个：compute（模板、镜像导入）、platform（userAdmin 的
// 配额初始化）都依赖它，因此整个 storage 域先于那两个域装配。
package main

import (
	"log"
	"path/filepath"

	"k_cockpit/internal/service/storage/pool"
	"k_cockpit/internal/service/storage/quota"
	"k_cockpit/internal/service/storage/userstorage"
)

// storageServices 承载 internal/service/storage 域的服务实例。
type storageServices struct {
	quota *quota.Service
	// userStorage 的分片暂存区在构造时落盘（见 setupStorageServices）。
	userStorage *userstorage.Service
	storageSvc  *pool.Service
}

// setupStorageServices 装配存储配额、用户存储与存储池服务。
func (a *app) setupStorageServices() {
	db, queue, agentClient := a.db, a.queue, a.agentClient

	// 存储配额（f-9-02）：按用户按节点。
	a.storage.quota = quota.NewService(db, a.recorder)
	// 分片暂存区放在可写数据目录下（DATA_DIR，默认 data）——控制面运行期
	// 产生的字节都在那里。**不从 DB_PATH 推导**：PostgreSQL 形态下 DB_PATH
	// 没有意义，而按 DB_PATH 的默认值会拼出相对发布目录的 `data`，用专用
	// 用户启动时直接 mkdir 失败退出。
	chunkStore, err := userstorage.NewChunkStore(filepath.Join(a.cfg.Data, "uploads"))
	if err != nil {
		log.Fatalf("[server] 初始化上传暂存区失败: %v", err)
	}
	a.storage.userStorage = userstorage.NewService(db, a.recorder, a.storage.quota).
		WithChunks(chunkStore).WithAgent(agentClient)
	a.storage.storageSvc = pool.NewService(db, queue, a.recorder, agentClient, a.platform.settings)
}
