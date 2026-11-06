-- 修正模板同名唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- 模板是**软删除**（template.deleted_at，见 maintain.go 的删除路径），而
-- 0001 建的是无条件唯一索引：
--   CREATE UNIQUE INDEX uniq_template_node_name ON template (node_id, name);
--
-- 后果与 DEF-R3-12（vpc_switch）完全同型，但表现更隐蔽（第 3 轮 DEF-R3-06）：
-- 导入预览用 GORM 查 `model.Template`，而 gorm.DeletedAt 会自动加上
-- `deleted_at IS NULL` → 看到"没有同名" → can_import=true；落库时唯一索引
-- **不排除**软删行 → 插入冲突 → 任务 failed。于是预览说能导、实际必然失败，
-- 用户无从判断该信哪一个。
--
-- 部分唯一索引把两者统一到同一个口径：当前存在的模板名字唯一，
-- 历史（已删）记录可以任意多。SQLite 与 PostgreSQL 都支持。
--
-- 数据侧不需要清理：旧索引比新索引更严，旧库里不可能存在两条同名的在用模板。
DROP INDEX IF EXISTS uniq_template_node_name;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_template_node_name
    ON template (node_id, name) WHERE deleted_at IS NULL;
