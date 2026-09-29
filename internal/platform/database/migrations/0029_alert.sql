-- 告警中心。
--
-- **状态表而不是事件流**：它要回答的是"现在有什么问题还没处理"，而不是
-- "发生过什么"（后者由 audit_log 与 scheduler_event 负责）。用事件流当
-- 告警，同一个问题每轮评估都会留一条，用户看到的是几百条重复；状态表
-- 里同一问题只有一行，靠 first_at / last_at 表达"从什么时候开始、最近
-- 一次仍见到是什么时候"。
--
-- 唯一键取 (kind, resource_type, resource_id)：它保证"同一个问题的同一
-- 个对象"只有一条，是去重的依据——评估每五分钟跑一次，没有它就会每轮
-- 插一条。
--
-- cleared 状态**保留行**而不是删除：一个问题反复出现又恢复时，"上次是
-- 什么时候好的"是判断它是否偶发的关键线索。

CREATE TABLE IF NOT EXISTS alert (
    id            bigserial   PRIMARY KEY,
    node_id       bigint,
    kind          varchar(32) NOT NULL,
    level         varchar(16) NOT NULL,
    resource_type varchar(32) NOT NULL DEFAULT '',
    resource_id   bigint      NOT NULL DEFAULT 0,
    resource_name varchar(128),
    title         varchar(255) NOT NULL,
    detail        text,
    status        varchar(16) NOT NULL DEFAULT 'active',

    first_at   timestamptz NOT NULL DEFAULT now(),
    last_at    timestamptz NOT NULL DEFAULT now(),
    ack_by     bigint,
    ack_at     timestamptz,
    cleared_at timestamptz,

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_alert_kind_resource
    ON alert (kind, resource_type, resource_id);

CREATE INDEX IF NOT EXISTS idx_alert_status_level
    ON alert (status, level);

CREATE INDEX IF NOT EXISTS idx_alert_node_id
    ON alert (node_id);
