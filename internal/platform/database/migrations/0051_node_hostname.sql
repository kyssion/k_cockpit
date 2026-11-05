-- agent 心跳上报的宿主机名（Heartbeat.hostname），仅用于节点列表展示。
-- 之前该字段在 proto 里定义、节点侧也上报，但控制面没有落库位置。
ALTER TABLE node ADD COLUMN IF NOT EXISTS hostname varchar(255) NOT NULL DEFAULT '';
