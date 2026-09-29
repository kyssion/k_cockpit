-- 存储池的开机自动挂载（F-5-01 的后续迭代）。
--
-- 列在**控制面**，因为它是"我们希望启动后是什么样"的配置；真正的写入
-- （fstab）由节点在 storage.pool.config 任务里完成。
--
-- 默认 true：绝大多数部署都希望宿主机重启后存储池仍然可用，而默认值取
-- false 的表现是"重启之后所有虚拟机找不到磁盘"——那是一个要重启才会暴露
-- 的默认值，代价太贵。
ALTER TABLE storage_pool ADD COLUMN IF NOT EXISTS auto_mount boolean NOT NULL DEFAULT true;
