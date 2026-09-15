-- ============================================================
-- 0002_node_agent_fields —— 节点接入方式改为节点代理（agent）
-- ------------------------------------------------------------
-- 依据：docs/06-decisions/0005-control-plane-node-agent-architecture.md
-- 变更要点：
--   1. 去掉「目标面板 API + 宿主机 SSH」双通道相关字段（控制面不再持有宿主机登录凭据）
--   2. 新增 agent 注册与信任字段（注册令牌哈希、证书指纹、注册状态）
--   3. 新增 agent 版本与协议版本、心跳与能力上报时间、"最近错误"摘要
--   4. 探测明细不再由控制面采集，改由 agent 自报（capabilities 已有，另加 capabilities_at）
--
-- 可重复执行：全部语句带 IF / IF EXISTS
-- 说明：本次变更会**丢弃**被删列的既有数据（当前环境 node 表为空）；若在已有数据的
--       环境执行，需先人工确认这些列的用途并迁移。
-- ============================================================

-- 一、移除旧双通道字段（API 通道 + SSH 通道）
ALTER TABLE node DROP COLUMN IF EXISTS api_base_url;
ALTER TABLE node DROP COLUMN IF EXISTS api_id;
ALTER TABLE node DROP COLUMN IF EXISTS api_key_enc;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_host;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_port;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_user;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_auth_type;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_password_enc;
ALTER TABLE node DROP COLUMN IF EXISTS ssh_private_key_enc;

-- 二、移除"控制面远程探测"的字段
ALTER TABLE node DROP COLUMN IF EXISTS last_probe_at;
ALTER TABLE node DROP COLUMN IF EXISTS last_probe_message;
ALTER TABLE node DROP COLUMN IF EXISTS probe_detail;

-- 三、新增 agent 注册与信任字段
ALTER TABLE node ADD COLUMN IF NOT EXISTS agent_id varchar(64);
ALTER TABLE node ADD COLUMN IF NOT EXISTS enroll_token_hash varchar(128);
ALTER TABLE node ADD COLUMN IF NOT EXISTS enroll_expires_at timestamptz;
ALTER TABLE node ADD COLUMN IF NOT EXISTS cert_fingerprint varchar(128);
ALTER TABLE node ADD COLUMN IF NOT EXISTS enroll_state varchar(16) NOT NULL DEFAULT 'pending';

-- 四、新增版本、心跳与能力上报字段
ALTER TABLE node ADD COLUMN IF NOT EXISTS agent_version varchar(32);
ALTER TABLE node ADD COLUMN IF NOT EXISTS protocol_version integer NOT NULL DEFAULT 0;
ALTER TABLE node ADD COLUMN IF NOT EXISTS last_heartbeat_at timestamptz;
ALTER TABLE node ADD COLUMN IF NOT EXISTS last_seen_at timestamptz;
ALTER TABLE node ADD COLUMN IF NOT EXISTS capabilities_at timestamptz;
ALTER TABLE node ADD COLUMN IF NOT EXISTS last_error varchar(255);

-- 五、索引：agent 标识唯一；心跳时间用于离线判定与清理
CREATE UNIQUE INDEX IF NOT EXISTS uniq_node_agent_id ON node (agent_id);
CREATE INDEX IF NOT EXISTS idx_node_last_heartbeat ON node (last_heartbeat_at);
