-- 修正存储池「节点 + 设备」唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- 存储池是**软删除**（storage_pool.deleted_at，删除执行器只置 deleted_at，
-- 见 pool/executor.go 的 DeleteExecutor），而 0001 建的是无条件唯一索引：
--   CREATE UNIQUE INDEX uniq_storage_pool_node_device ON storage_pool (node_id, device_id);
--
-- 后果：删掉存储池之后，同一块盘上**永远**无法再建池——软删行仍占住
-- 唯一索引，插入冲突，任务 failed「该设备已被存储池占用」，而设备在
-- 节点上明明是空闲的（实机测试抓出）。与 uniq_template_node_name
-- （0054，DEF-R3-06）、uniq_vpc_switch_node_name（DEF-R3-12）完全同型：
-- 软删除的表配无条件唯一索引，删除即自锁。
--
-- 部分唯一索引把两者统一到同一个口径：当前存在的「节点 + 设备」唯一，
-- 历史（已删）记录可以任意多。SQLite 与 PostgreSQL 都支持。
--
-- 数据侧不需要清理：旧索引比新索引更严，旧库里不可能存在两条同
-- (node_id, device_id) 的在用池。
DROP INDEX IF EXISTS uniq_storage_pool_node_device;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_pool_node_device
    ON storage_pool (node_id, device_id) WHERE deleted_at IS NULL;
