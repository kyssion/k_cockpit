-- CPU 亲和性预设。
--
-- 为什么要"预设"而不是每次手填 cpuset：绑定物理核是**为性能做的一件事**
-- （避免虚拟机在核之间漂移、或让一台机器只用自己的 NUMA 节点），而它的写法
-- （0-3,8）对用户不友好且**写错了不报错**——只表现为"性能不如预期"。
-- 存成有名字的预设之后，常用组合只需理解一次，之后按名字选。
--
-- 与 cpuset 相关的一条约束值得写在这里：这个字符串最终会被拼进 libvirt 的
-- 域配置，因此**只接受数字、逗号与连字符**（由服务层强制）。少一种可表达的
-- 写法，就少一整类绕过方式——与目录共享只收相对路径是同一个思路。

CREATE TABLE IF NOT EXISTS cpu_affinity_preset (
    id         bigserial   PRIMARY KEY,
    node_id    bigint      NOT NULL,
    name       varchar(64) NOT NULL,
    cpuset     varchar(128) NOT NULL,
    remark     varchar(255),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- 软删除：预设被引用在别处（审计、以及将来可能的虚拟机配置）时，
    -- 硬删除会让那些引用指向一个不存在的行。
    deleted_at timestamptz
);

-- 名字的唯一性**只针对未删除的行**。
--
-- 不给条件的话，删掉「高性能」之后就再也建不了同名的了——用户会撞上一句
-- 他无法理解、也无法自行解决的唯一约束冲突（改名能绕过，但没人会想到问题
-- 出在一条看不见的记录上）。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_cpu_affinity_node_name
    ON cpu_affinity_preset (node_id, name) WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_cpu_affinity_preset_deleted_at
    ON cpu_affinity_preset (deleted_at);
