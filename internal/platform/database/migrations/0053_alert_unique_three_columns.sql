-- 修正告警唯一索引：从「单列 resource_id」改回「kind + resource_type + resource_id」。
--
-- 0029 建表时写的是三列（正确），但模型（internal/model/alert.go）只给
-- ResourceID 打了 uniqueIndex tag——而 SQLite 部署的表结构由 AutoMigrate
-- **按模型**派生，于是真机上建出来的是：
--
--   CREATE UNIQUE INDEX uniq_alert_kind_resource ON alert (resource_id)
--
-- 后果不是"少了个索引"，而是一条**永久失败**的写入路径（第 3 轮 DEF-R4-01）：
-- 评估器按三列查询（kind/resource_type/resource_id），查不到就 Create；
-- 单列索引下只要同一个 resource_id 已有别的种类的告警，Create 必然撞
-- `UNIQUE constraint failed: alert.resource_id` → 代码 continue → 下一秒重试。
-- 真机 126 次/60s、5 次 SLOW SQL、SQLITE_BUSY——SQLite 是单写者，整个面板的
-- 写入（包括应急改密）都被这条重试挤住。
--
-- 修正方式：删掉旧索引，按三列重建。模型 tag 已同步补齐（priority 1/2/3），
-- 否则测试库（AutoMigrate）与真库会再次分叉。
--
-- 数据侧不需要清理：同一 (kind, resource_type, resource_id) 在一个错误索引下
-- 可能已插入过多行吗？不会——单列索引比三列**更严**，所以旧库里不可能存在
-- 三列重复的行，直接建新索引不会失败。
DROP INDEX IF EXISTS uniq_alert_kind_resource;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_alert_kind_resource
    ON alert (kind, resource_type, resource_id);
