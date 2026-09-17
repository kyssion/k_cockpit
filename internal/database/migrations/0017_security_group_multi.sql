-- 让「多组叠加生效」成为可能，并修掉软删除与唯一索引的冲突。
--
-- 两件事都属于「表结构表达不了 PRD 已经写明的规则」。

-- 1) 网口与附加安全组的关联。
--
-- f-4-03 要求「多组叠加生效」，而 vm_interface.security_group_id 是单个
-- 外键，只能表达「一个网口挂一个组」。
--
-- 主组保留在 vm_interface 上而不一起迁过来：网口的编辑流程已经在用那个
-- 字段，迁移会让那部分代码同时改动。生效规则 = 主组 ∪ 附加组，两者在语义
-- 上完全平等，只是存放位置不同。
CREATE TABLE IF NOT EXISTS interface_security_group (
    id           bigserial   PRIMARY KEY,
    interface_id bigint      NOT NULL,
    group_id     bigint      NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- 同一个组不该重复挂到同一个网口上：重复挂载不会改变生效规则（并集去重），
-- 却会让「这台机器挂了几个组」这个数字虚高，界面上的组标签也会重复出现。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_interface_security_group
    ON interface_security_group (interface_id, group_id);
CREATE INDEX IF NOT EXISTS idx_interface_security_group_group_id
    ON interface_security_group (group_id);

-- 2) 修正安全组名称的唯一索引：从「一辈子占住名字」改为「当前存在的才算」。
--
-- 建库时的定义是：
--   CREATE UNIQUE INDEX uniq_security_group_node_name ON security_group (node_id, name);
--
-- 而 security_group 有 deleted_at（软删除）。两者合在一起的含义是：**一个
-- 被删除的组会永久占住它的名字**。用户删掉「web」之后想再建一个「web」，
-- 会撞上一句唯一约束冲突——而他刚刚明明把这个名字删掉了。那句报错他无法
-- 理解，更无法自行解决（改名能绕过，但没人会想到问题出在一条看不见的记录上）。
--
-- 部分唯一索引（WHERE deleted_at IS NULL）让「当前存在的组名唯一」成立，
-- 而历史记录可以任意多。这与 public_ip_binding 那次（迁移 0016）是同一个
-- 模式：唯一性应当约束**有效**的那些行。
DROP INDEX IF EXISTS uniq_security_group_node_name;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_security_group_node_name
    ON security_group (node_id, name)
    WHERE deleted_at IS NULL;
