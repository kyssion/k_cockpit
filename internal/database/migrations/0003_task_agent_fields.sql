-- ============================================================
-- 0003_task_agent_fields —— 任务表适配 agent 执行模型
-- ------------------------------------------------------------
-- 依据：docs/06-decisions/0005-control-plane-node-agent-architecture.md
--       docs/02-architecture/ARCHITECTURE.md §4.2（创建虚拟机）、§4.4（断连与对账）、§6（幂等与重试）
-- 变更要点：
--   1. idempotency_key：控制面生成、随指令下发给 agent；agent 侧据此去重，
--      重连重放同一任务时不会重复执行（唯一索引保证同一意图只有一个任务）
--   2. dispatched_at：指令实际下发时间（与 created_at 区分，便于判断排队与通道耗时）
--   3. last_reported_at：agent 最近一次上报进度的时间（配合 node.last_seen_at 判定任务是否失联）
--   4. task.status 新增取值 unknown —— 节点离线导致"执行中但结果未知"，
--      由 agent 重连对账后收敛（varchar 列，无需 DDL，取值登记见 DATA_MODEL §5）
--
-- 可重复执行：全部语句带 IF NOT EXISTS
-- ============================================================

ALTER TABLE task ADD COLUMN IF NOT EXISTS idempotency_key varchar(64);
ALTER TABLE task ADD COLUMN IF NOT EXISTS dispatched_at timestamptz;
ALTER TABLE task ADD COLUMN IF NOT EXISTS last_reported_at timestamptz;

-- 同一幂等键只能有一个任务（NULL 表示未使用幂等键，PG 中多行 NULL 不冲突）
CREATE UNIQUE INDEX IF NOT EXISTS uniq_task_idempotency_key ON task (idempotency_key);

-- 失联判定：按"已下发但长时间无上报"扫描
CREATE INDEX IF NOT EXISTS idx_task_dispatched_at ON task (dispatched_at);
