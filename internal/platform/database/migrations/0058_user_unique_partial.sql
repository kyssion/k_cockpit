-- 修正用户名唯一索引：从「一辈子唯一」改为「当前存在的唯一」。
--
-- user 是**软删除**（user.deleted_at，见 useradmin.Service.Delete：保留审计与
-- 历史引用，硬删会让 audit_log 里的 operator_id 指向不存在的用户），而 0001
-- 建的是无条件唯一索引：
--   CREATE UNIQUE INDEX uniq_user_username ON "user" (username);
--
-- 后果与 uniq_node_name（0057）完全同型：删掉一个账号之后，用同一个用户名
-- **永远**建不回来——服务层的重名检查走 GORM（自动过滤 deleted_at，查不到
-- 已删行），入库却撞唯一约束，界面提示「用户名已被占用」，而用户列表里根本
-- 没有这个名字（r21 实机测试抓出：U-12 的认证用例重复执行时稳定复现）。
--
-- 数据侧不需要清理：旧索引比新索引更严，旧库里不可能存在两条同名的在用账号。
DROP INDEX IF EXISTS uniq_user_username;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_username
    ON "user" (username) WHERE deleted_at IS NULL;
