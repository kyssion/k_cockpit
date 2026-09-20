-- 计算资源配额（vCPU / 内存 / 实例数）。
--
-- 单独立表而不是复用 resource_quota：后者是**周期累计型**（月流量、月运行
-- 时长），按月重置、超限后限速或断网；这里是**存量型**——此刻占着几个核、
-- 几 GB、几台，没有周期概念，超限的处置是**拒绝新建**而不是限速。
--
-- 两种语义挤在一张表里，每一列都会出现例外：周期列对它无意义，处置列要
-- 为它塞一个既不是限速也不是断网的值，而周期评估循环还会按月把上限"重置"
-- 掉——那是一条长期有效的约束被静默清空。
--
-- 三个上限在同一行：它们总是**一起**设置（"给这个用户 8 核 / 16 GB / 5 台"）。
-- 拆成按维度分行的话，三处各自可能只填了一半，而"这个用户到底能建几台"
-- 就要遍历三行才能回答。

CREATE TABLE IF NOT EXISTS compute_quota (
    id         bigserial   PRIMARY KEY,
    node_id    bigint      NOT NULL,
    user_id    bigint      NOT NULL,
    vcpu       integer     NOT NULL DEFAULT 0,
    memory_mb  integer     NOT NULL DEFAULT 0,
    vm_count   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 一个用户在一个节点上**只有一份**计算配额。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_compute_quota_node_user
    ON compute_quota (node_id, user_id);

CREATE INDEX IF NOT EXISTS idx_compute_quota_user_id
    ON compute_quota (user_id);
