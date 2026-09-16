-- ============================================================
-- 0005_vm_console —— 虚拟机的控制台配置
-- ------------------------------------------------------------
-- 依据：docs/07-specs/f-2-08-vnc-console.md（R-001 / R-004 / R-011）
--       docs/02-architecture/DATA_MODEL.md §4.6
-- 背景：f-2-08 的数据变更声明「复用 vm 表的配置投影」，但 vm 表实际
--       并没有这些列——规格写就时把它们当成了既有字段。本迁移补齐。
-- 变更要点：
--   vnc_enabled    控制台是否开启
--   vnc_port       宿主上的 VNC 端口
--   vnc_bind       监听地址，**默认 127.0.0.1**（R-001）：
--                  只有显式开启「对外暴露」才会改变，而那是需要二次
--                  验证的高危操作（R-004）
--   vnc_exposed    是否对外暴露。与 vnc_bind 分开记录，是为了让「曾经
--                  暴露过」这件事在审计与界面上都留下痕迹，而不是只从
--                  监听地址去推断
--   display_device 显示设备类型；'none' 表示该虚拟机没有控制台（R-011）——
--                  界面据此隐藏入口，而不是给用户一个打不开的按钮
--
-- 密码存于既有 vm_credential 表（password_enc），不在此新增字段。
--
-- 可重复执行：全部语句带 IF NOT EXISTS
-- ============================================================

ALTER TABLE vm ADD COLUMN IF NOT EXISTS vnc_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS vnc_port integer;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS vnc_bind varchar(64) NOT NULL DEFAULT '127.0.0.1';
ALTER TABLE vm ADD COLUMN IF NOT EXISTS vnc_exposed boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS display_device varchar(16) NOT NULL DEFAULT 'vnc';
