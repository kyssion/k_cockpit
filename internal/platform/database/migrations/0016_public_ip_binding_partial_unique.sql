-- 修正公网 IP 绑定的唯一索引：从「一辈子一条」改为「同一时刻一条」。
--
-- 建库时的定义是：
--   CREATE UNIQUE INDEX uniq_public_ip_binding_public_ip_id ON public_ip_binding (public_ip_id);
--
-- 它约束的是「一个地址**总共只能有一条**绑定记录」。而真正要表达的规则是
-- f-4-06 的那一条：**一个地址同一时刻只能指向一个地方**（网络层的事实，
-- 不是控制面的偏好）。
--
-- 两者的差别在浮动迁移上直接暴露：迁移是「释放旧绑定、建立新绑定」，
-- 前者会留下一条 released_at 非空的记录。在旧索引下这次迁移**必然失败**
-- ——而失败信息是一句唯一约束冲突，与「地址只能指向一处」这件事看起来
-- 毫无关系。
--
-- 副产物更重要：旧索引让「保留绑定历史」在设计上就不可能。而这恰恰是
-- 公网地址最需要留住的东西——「这个地址在某个时间点指向谁」是安全审计
-- 与故障排查的核心信息，删掉记录就无从查起。
--
-- 部分唯一索引（WHERE released_at IS NULL）同时满足两件事：有效绑定唯一，
-- 而历史记录可以任意多。PostgreSQL 与 SQLite 都支持它。
DROP INDEX IF EXISTS uniq_public_ip_binding_public_ip_id;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_public_ip_binding_active
    ON public_ip_binding (public_ip_id)
    WHERE released_at IS NULL;

-- 历史查询走这个索引：按地址查「它曾经指向过谁」，按时间倒序。
CREATE INDEX IF NOT EXISTS idx_public_ip_binding_history
    ON public_ip_binding (public_ip_id, released_at DESC);
