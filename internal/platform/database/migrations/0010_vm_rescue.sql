-- 救援系统（F-2-12）：改盘型/网卡/引导顺序后从救援镜像启动，退出还原。
--
-- rescue_config 保存的是**进入救援之前**的那份配置快照（引导顺序、盘型、
-- 网卡型号、显示设备）。没有它，退出救援时只能猜一个默认值填回去——
-- 而那是悄悄改掉用户的配置：他进入救援是为了修系统，退出后发现引导顺序
-- 被重置成了默认值，机器起不来了。
--
-- 用 text 存 JSON 而不是拆成多列：快照的字段集合会随救援能力扩展而变化
-- （将来可能要改 BIOS、CPU 型号），拆列意味着每加一项都要一次迁移，而这份
-- 数据只是「原样存、原样还」，控制面从不按字段查询它。
--
-- 迁移只增不改：ADD COLUMN IF NOT EXISTS 可重复执行。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS rescue_active boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS rescue_config text;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS rescue_since timestamptz;
