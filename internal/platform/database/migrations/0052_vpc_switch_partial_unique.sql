-- 修正 vpc_switch 的唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- 建库时（0001）的写法是：
--   CREATE UNIQUE INDEX uniq_vpc_switch_node_name ON vpc_switch (node_id, name)
--   CREATE UNIQUE INDEX uniq_vpc_switch_node_vlan ON vpc_switch (node_id, vlan_id)
-- 两条都**不排除 deleted_at 非空的行**，而删除交换机是**软删除**（记录要留给
-- 审计与历史任务引用，见 vswitch.deleteRecord 的说明）。
--
-- 两者叠加的后果在真机上是这样：删掉的交换机永久占着名字与 VLAN。同名重建时
-- 控制面的受理校验（`node_id = ? AND deleted_at IS NULL`，见
-- vswitch.checkUnique）放行，节点上也把桥建好了，最后在写记录时撞上唯一约束
-- ——任务失败，而节点上的新桥留了下来（第 3 轮真机：库里 28 条软删测试行占着
-- 索引，失败后残留 kbr056539e3）。失败信息是一句唯一约束冲突，与「这个名字
-- 现在其实没人用」看起来毫无关系。
--
-- 部分唯一索引（WHERE deleted_at IS NULL）同时满足两件事：**当前存在**的交换机
-- 名字与 VLAN 唯一，而历史记录可以任意多。SQLite 与 PostgreSQL 都支持。
--
-- 与 0017_security_group_multi.sql、0022_cpu_affinity_preset.sql 同一口径。
-- 模型的 gorm tag 必须同步带 `where:deleted_at IS NULL`（见 model/network.go），
-- 否则测试库（AutoMigrate 按模型建）与生产库在「什么算重复」上会给出不同答案。
--
-- vlan_id 为 NULL 的行按现有语义不参与唯一性（NULL 在两种数据库里都互不相等），
-- 因此「多个不划 VLAN 的交换机」仍然可以共存。
DROP INDEX IF EXISTS uniq_vpc_switch_node_name;
DROP INDEX IF EXISTS uniq_vpc_switch_node_vlan;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_vpc_switch_node_name
    ON vpc_switch (node_id, name) WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_vpc_switch_node_vlan
    ON vpc_switch (node_id, vlan_id) WHERE deleted_at IS NULL;
