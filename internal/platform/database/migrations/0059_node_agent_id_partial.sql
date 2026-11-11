-- 修正 agent 身份唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- node 是**软删除**（node.deleted_at，见 node.Service.Remove），而 0002 建的
-- 是无条件唯一索引：
--   CREATE UNIQUE INDEX uniq_node_agent_id ON node (agent_id);
--
-- 后果：移除一台节点之后，用**同一个 agent 身份**重新接入永远失败——服务层
-- 按 agent_id 找节点走 GORM（自动过滤 deleted_at，查不到已删行），入库却撞
-- 唯一约束，返回「服务内部错误」。而这正是重装系统 / 换盘重装后的标准动作：
-- agent 的 /var/lib/k_cockpit-agent 还在，uuid 不变。
--
-- 与 uniq_node_name（0057）、uniq_user_username（0058）完全同型：软删除的表
-- 配无条件唯一索引，删除即自锁。
--
-- 数据侧不需要清理：旧索引比新索引更严，旧库里不可能存在两条同 agent_id 的
-- 在用节点。
DROP INDEX IF EXISTS uniq_node_agent_id;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_node_agent_id
    ON node (agent_id) WHERE deleted_at IS NULL;
