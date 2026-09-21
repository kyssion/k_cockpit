-- 请求日志（F-10-07）。
--
-- 与审计（audit_log）分表：审计记的是**谁改了什么**（业务语义），请求日志
-- 记的是**每次接口调用**（运维视角）。它们的量级差两个数量级——创建一个
-- 虚拟机会产生 1 条审计，却可能产生几十条请求日志。合表的结果是审计被淹
-- 在健康检查与轮询里，而那正是审计唯一的价值所在。
--
-- 默认不开启：它是排查时才需要的东西，默认打开只会把磁盘用在心跳与轮询上。
CREATE TABLE IF NOT EXISTS request_log (
    id           bigserial    PRIMARY KEY,
    at           timestamptz  NOT NULL,
    user_id      bigint,
    method       varchar(8)   NOT NULL,
    path         varchar(512) NOT NULL,
    status       integer      NOT NULL,
    duration_ms  integer      NOT NULL DEFAULT 0,
    client_ip    varchar(64),
    user_agent   varchar(255)
);

-- 请求日志只有"翻最近这段"这一种用法，因此只需要按时间倒序的索引。
CREATE INDEX IF NOT EXISTS idx_request_log_at ON request_log (at DESC);
CREATE INDEX IF NOT EXISTS idx_request_log_user ON request_log (user_id, at DESC);
