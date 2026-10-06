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
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	// 存储配额（f-9-02）：按用户按节点。
	a.storage.quota = quota.NewService(db, a.recorder)
	// 分片暂存区放在数据库同级的 data 目录下——控制面持久化的东西都在那里。
	chunkStore, err := userstorage.NewChunkStore(filepath.Join(filepath.Dir(a.cfg.DB.Path), "uploads"))
	if err != nil {
		log.Fatalf("[server] 初始化上传暂存区失败: %v", err)
	}
	a.storage.userStorage = userstorage.NewService(db, a.recorder, a.storage.quota).
		WithChunks(chunkStore).WithAgent(mockAgent)
	a.storage.storageSvc = pool.NewService(db, queue, a.recorder, mockAgent, a.platform.settings)
}
