-- 资源配额（F-4-10 / F-1-08）：用户 × 维度的上限与超限处置。
--
-- 为什么**不往 user_storage 上加列**：那张表的语义是「这个用户在哪个节点上
-- 的存储空间」，把流量、运行时长塞进去会让它变成一张什么都装一点的表，而
-- 「配额有几个维度」这件事在 schema 上就看不出来了。加一个维度要改表结构，
-- 于是每加一个维度都是一次迁移。
--
-- 为什么**不把用量也放在这张表**：用量已经有两张按天累计的表
-- （traffic_stat_daily / vm_runtime_daily），它们是采集器在写的。在这里
-- 再存一份累计值会出现两个数据源，而对不上时无法判断哪个是真的——这类
-- 分歧不会报错，只会让用户看到的数字与处置依据不一致。
--
-- 因此本表**只放策略与处置状态**，用量每次按需从统计表里算。

CREATE TABLE IF NOT EXISTS resource_quota (
    id          bigserial   PRIMARY KEY,
    node_id     bigint      NOT NULL,
    user_id     bigint      NOT NULL,

    -- 维度：traffic_in / traffic_out / runtime。
    --
    -- 都是**累计型**（一段时间内累计多少），因此都有「超限」这个概念。
    -- 带宽限速是**速率型**，没有累计也就没有"超限"，它不走这张表——
    -- 把它混进来会让人以为带宽也有"用满了"的状态。
    dimension   varchar(24) NOT NULL,

    -- 上限。0 表示不限——**默认不限**。
    --
    -- 与存储配额同一个道理：默认给一个上限会让用户在自己什么都没做的时候
    -- 撞上一堵看不见的墙，而报错指向的是「已超出配额」，与他刚做的事无关。
    limit_value bigint      NOT NULL DEFAULT 0,

    -- 超限后的处置：throttle（限速）或 block（断网）。
    --
    -- **默认 throttle**：block 会让业务直接中断，不该是默认值。选它的人
    -- 应当是有意为之，而不是"没注意"。
    action      varchar(16) NOT NULL DEFAULT 'throttle',

    -- 当前周期的状态：ok / warned / limited。
    --
    -- **warned 与 limited 必须分开**：前者是"快到了"，后者是"已经处置了"。
    -- 合并成一个"超限"状态的话，用户在网络变慢时无法判断是自己用超了，
    -- 还是会话出了问题。
    status      varchar(16) NOT NULL DEFAULT 'ok',

    -- period 记录当前状态对应的周期（YYYY-MM，UTC）。
    --
    -- 它解决的是"跨月重置"：新的一月到来时把 status 与下面两个时刻清空。
    -- 不记周期的话，上个月被限速的用户在新的一月里仍然限着——而那时他的
    -- 用量是 0，界面上显示"已超限"，没有任何地方能解释这件事。
    period      varchar(7),

    warned_at   timestamptz,
    limited_at  timestamptz,

    -- detail 是最近一次判定的说明（何时、超了多少）。
    detail      text,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- 一个用户在同一个节点上的同一个维度只有一份策略。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_resource_quota_node_user_dim
    ON resource_quota (node_id, user_id, dimension);

CREATE INDEX IF NOT EXISTS idx_resource_quota_status
    ON resource_quota (status);
