-- 修正节点同名唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- node 是**软删除**（node.deleted_at，见 node.Service.Remove），而 0001 建的是
-- 无条件唯一索引：
--   CREATE UNIQUE INDEX uniq_node_name ON node (name);
--
-- 后果：移除一台节点之后，用同一个名字重新接入**永远**失败——服务层的重名
-- 检查走 GORM（自动过滤 deleted_at，查不到已删行），入库却撞唯一约束，
-- 返回「节点名已存在」，而节点列表里根本没有这个名字（实机测试抓出：
-- 删掉待接入节点后重新生成注册令牌，稳定复现）。
--
-- 与 uniq_template_node_name（0054）、uniq_vpc_switch_node_name（DEF-R3-12）、
-- uniq_storage_pool_node_device（0056）完全同型：软删除的表配无条件唯一索引，
-- 删除即自锁。
--
-- 数据侧不需要清理：旧索引比新索引更严，旧库里不可能存在两条同名的在用节点。
DROP INDEX IF EXISTS uniq_node_name;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_node_name
    ON node (name) WHERE deleted_at IS NULL;
