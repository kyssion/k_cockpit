-- 跨节点迁移（F-2-09）。
--
-- 独立成表而不是只看 vm.node_id：迁移是**一次性但需要留痕**的操作——
-- 「这台机器原来在哪台宿主机上」在排查存储、网络、性能问题时是第一条线索，
-- 而 vm.node_id 只记录了「现在在哪」。
--
-- 迁移与其它操作最大的不同是**有两个失败面**：源侧（读不出磁盘）与目标侧
-- （写不下、起不来）。因此 from_/to_ 两侧都记，且失败时要能说清是哪一侧
-- 出的问题——只说「迁移失败」会让排查从两台机器里猜一台开始。
--
-- status 与 task.status 用同一套词汇（pending/running/success/failed）。
CREATE TABLE IF NOT EXISTS vm_migration (
    id             bigserial    PRIMARY KEY,
    vm_id          bigint       NOT NULL,
    vm_name        varchar(128) NOT NULL,
    from_node_id   bigint       NOT NULL,
    to_node_id     bigint       NOT NULL,
    status         varchar(16)  NOT NULL DEFAULT 'pending',
    -- result 是迁移的补充结果（如「已迁移 3 块网卡、2 个静态地址」），
    -- 供界面说明**跟着搬了些什么**。
    result         varchar(512),
    error          varchar(512),
    created_by     bigint,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    finished_at    timestamptz
);

CREATE INDEX IF NOT EXISTS idx_vm_migration_vm_id ON vm_migration (vm_id);
