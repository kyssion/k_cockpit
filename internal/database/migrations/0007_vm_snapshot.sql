-- ============================================================
-- 0007 虚拟机快照（F-2-07）
-- ------------------------------------------------------------
-- 为什么快照要落库而不是只放虚拟化层：
--
--   1. 虚拟化层的快照**没有归属信息**，控制面无法回答「这条快照是谁建的」
--      ——而多租户下「只能看到自己的快照」是靠归属判定的；
--   2. 创建与恢复都是异步任务，界面需要在这期间展示中间状态（创建中 /
--      恢复中），那个状态必须有个地方存；
--   3. 快照配额要在**创建之前**就能算出来，临时去问节点会导致「先创建
--      再发现超配额」——那时已经产生了一个需要回滚的快照。
--
-- 因此本表是**投影 + 元数据**的混合体，与 vm 表同样的定位：虚拟化层是权威，
-- 这里缓存它并补充控制面自己的信息。
-- ============================================================

CREATE TABLE IF NOT EXISTS vm_snapshot (
    id             bigserial    PRIMARY KEY,
    vm_id          bigint       NOT NULL,
    node_id        bigint       NOT NULL,

    name           varchar(128) NOT NULL,
    description    varchar(255),

    -- 快照种类：
    --   internal —— 保存在磁盘镜像内部。含内存时可恢复**运行现场**，
    --               代价是体积大、且对磁盘 I/O 有持续影响；
    --   external —— 以 `--disk-only` 另存为独立文件，只能恢复磁盘内容。
    kind           varchar(16)  NOT NULL,

    -- 是否包含内存状态。
    --
    -- 含内存的快照能把虚拟机恢复到「按下快照那一刻」的运行现场（内存内容
    -- 一并保存），代价是体积可能数倍于磁盘本身；不含内存的只能恢复到关机
    -- 状态。这是一个用户必须自己做的取舍，因此不做默认值猜测。
    include_memory boolean      NOT NULL DEFAULT false,

    -- 创建时虚拟机的状态。
    --
    -- 恢复时据此判断是否必须先关机：把运行中虚拟机的磁盘状态直接回滚，
    -- 得到的是一个文件系统损坏的来宾系统——它可能还能启动，然后在某个
    -- 随机时刻崩溃。
    vm_status      varchar(16),

    -- 虚拟化层里的快照标识。
    --
    -- 与 name **分开记录**：用户可以重命名快照（改名只影响界面），
    -- 而虚拟化层的标识一旦建立就不该再变——变了就找不到那条快照了。
    domain_name    varchar(255),

    -- 快照占用空间（字节）。
    size_bytes     bigint       NOT NULL DEFAULT 0,

    -- 父快照。外部快照会形成链，链上中间节点被删除会让子快照失去依赖。
    parent_id      bigint,

    -- creating / ready / restoring / deleting / error
    status         varchar(16)  NOT NULL DEFAULT 'creating',

    -- 是否存在子快照。为 true 时不可删除——界面据此禁用删除按钮并说明原因。
    has_children   boolean      NOT NULL DEFAULT false,

    -- 虚拟机当前是否运行在这个快照上。为 true 时恢复它没有意义。
    is_current     boolean      NOT NULL DEFAULT false,

    created_by     bigint,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now()
);

-- 列表的查询形状是「某台虚拟机、按时间倒序」，这个复合索引直接覆盖，
-- 不必再回表排序。
CREATE INDEX IF NOT EXISTS idx_vm_snapshot_vm_created
    ON vm_snapshot (vm_id, created_at DESC);

-- 同一台虚拟机内快照名唯一；跨虚拟机可重名。
--
-- 唯一约束只到 vm_id 这一层：用户在不同虚拟机上各建一个叫「初始状态」的
-- 快照是完全正常的做法，把唯一性放到全局会把这些正常操作变成报错。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_snapshot_vm_name
    ON vm_snapshot (vm_id, name);
