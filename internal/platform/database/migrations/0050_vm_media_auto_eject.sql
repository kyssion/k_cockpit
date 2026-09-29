-- 首次启动初始化完成后自动弹出安装介质（F-2-17，Windows / ConfigDrive）。
--
-- 为什么要一列而不是实时推断：弹出是"一次性"动作（弹过就完），必须有
-- 地方记录"还欠着这一次弹出"。置位发生在创建（configdrive 初始化且挂了
-- 安装 ISO），清零发生在弹出发起。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS media_auto_eject boolean NOT NULL DEFAULT false;
