// routes_storage 登记存储域的路由：存储池与分区、存储卷（LVM 聚合）、
// 用户存储与分片上传、存储配额，以及目录共享到虚拟机（9p）。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	"k_cockpit/internal/handler/storage"
)

func registerStorageRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	storageHandler := storage.NewStorage(deps.Storage, deps.Risk)
	quotaHandler := storage.NewQuota(deps.Quota)
	userStorageHandler := storage.NewUserStorage(deps.UserStorage)
	volumeHandler := storage.NewStorageVolume(deps.Storage)

	{
		// 存储池（F-5-01）：管理员专属。创建与删除会格式化/销毁设备，
		// 属受二次验证保护的操作（在 handler 入口调用 guard）。
		v1.GET("/nodes/:id/disks", g.requireAuth, g.adminOnly, storageHandler.Disks)
		v1.GET("/nodes/:id/storage-pools", g.requireAuth, g.adminOnly, storageHandler.ListPools)
		v1.GET("/storage-pools/:id", g.requireAuth, g.adminOnly, storageHandler.GetPool)
		v1.POST("/storage-pools", g.requireAuth, g.adminOnly, storageHandler.CreatePool)
		v1.PATCH("/storage-pools/:id", g.requireAuth, g.adminOnly, storageHandler.UpdatePool)
		v1.DELETE("/storage-pools/:id", g.requireAuth, g.adminOnly, storageHandler.DeletePool)
		// 分区、池配置、卸载、trim（F-5-01 的后续迭代）。
		//
		// 池配置单独一个接口而不是扩展 PATCH：PATCH 是"只改控制面一列、
		// 同步返回"，而配置要下发到节点改挂载与 fstab——两者放在一起会让
		// "保存"在不同字段上有完全不同的耗时与失败语义。
		v1.GET("/storage-partitions", g.requireAuth, g.adminOnly, storageHandler.Partitions)
		v1.POST("/storage-partitions", g.requireAuth, g.adminOnly, storageHandler.CreatePartition)
		v1.DELETE("/storage-partitions", g.requireAuth, g.adminOnly, storageHandler.DeletePartitions)
		v1.POST("/storage-pools/:id/config", g.requireAuth, g.adminOnly, storageHandler.UpdatePoolConfig)
		v1.POST("/storage-pools/:id/unmount", g.requireAuth, g.adminOnly, storageHandler.UnmountPool)
		v1.POST("/storage/trim", g.requireAuth, g.adminOnly, storageHandler.TrimStorage)

		// 存储卷（F-5-02，LVM 多盘聚合）。
		//
		// 归管理员：卷会**独占物理设备**，选错盘会影响这台宿主机上所有
		// 虚拟机的存储。与存储池同一档。
		//
		// 创建走**预检 → 确认**两步：条带与镜像的组合里有一个直觉容易
		// 出错的乘法（需要 stripe × mirror 块盘），而"条带没有冗余"更是
		// 几乎所有人都会有的误解——看到「用了 4 块盘」很自然会以为那是
		// 4 块盘的冗余。
		v1.GET("/storage-volumes", g.requireAuth, g.adminOnly, volumeHandler.List)
		v1.POST("/storage-volumes/preview", g.requireAuth, g.adminOnly, volumeHandler.Preview)
		v1.POST("/storage-volumes", g.requireAuth, g.adminOnly, volumeHandler.Create)
		// 删除**会销毁卷里的全部数据**，不可恢复。
		v1.DELETE("/storage-volumes/:id", g.requireAuth, g.adminOnly, volumeHandler.Delete)

		// 用户存储空间与文件管理（F-5-03/04/05）。
		//
		// 上传分两步：创建会话（可命中**秒传**）→ 登记分片 → 收尾。
		// 会话把「已经收到了哪些分片」变成可持久化的事实，于是断点续传
		// 只是「问服务端我还要传哪几片」。
		v1.GET("/my-storage", g.requireAuth, userStorageHandler.Get)
		v1.POST("/my-storage", g.requireAuth, userStorageHandler.Ensure)
		v1.GET("/my-storage/files", g.requireAuth, userStorageHandler.ListFiles)
		v1.GET("/my-storage/files/:id/download", g.requireAuth, userStorageHandler.Download)
		v1.DELETE("/my-storage/files/:id", g.requireAuth, userStorageHandler.DeleteFile)
		v1.POST("/my-storage/uploads", g.requireAuth, userStorageHandler.CreateUpload)
		v1.GET("/my-storage/uploads/:uploadID", g.requireAuth, userStorageHandler.GetUpload)
		v1.POST("/my-storage/uploads/:uploadID/complete", g.requireAuth, userStorageHandler.CompleteUpload)
		// 分片**字节**：请求体是裸二进制，不走 JSON。
		v1.PUT("/my-storage/uploads/:uploadID/chunks/:index", g.requireAuth, userStorageHandler.PutChunkData)

		// 目录共享到虚拟机（F-5-06，9p VirtFS）。
		//
		// **接口只接受相对路径**（相对于调用者的存储根），这是整块功能的
		// 安全前提：共享是一个跨越虚拟化边界的读取入口，而参数来自用户。
		// 允许绝对路径意味着一个租户可以把 /etc 挂进自己的虚拟机读出来，
		// 而这不会触发任何权限检查——qemu 是以一个有权读它的用户在跑。
		//
		// 卸载不需要二次验证：目录还在宿主机上，随时可以再挂回去。
		v1.GET("/vms/:id/shares", g.requireAuth, storageHandler.ListShares)
		v1.POST("/vms/:id/shares", g.requireAuth, storageHandler.MountShare)
		v1.DELETE("/vms/:id/shares/:tag", g.requireAuth, storageHandler.UnmountShare)

		// 存储配额（F-9-02）。
		//
		// 自己的用量对所有登录用户开放；设置配额与查看全部用户的用量是
		// admin 专属——用量数字会暴露「谁有多少资源」。
		v1.GET("/quota", g.requireAuth, quotaHandler.Mine)
		v1.GET("/quotas", g.requireAuth, g.adminOnly, quotaHandler.List)
		v1.PUT("/quotas", g.requireAuth, g.adminOnly, quotaHandler.Set)
	}
}
