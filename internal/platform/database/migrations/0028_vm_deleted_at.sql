-- 虚拟机删除（移入回收站）的时刻。
--
-- **不用软删除的通用约定**（即不为它启用 GORM 的自动过滤）：回收站要能
-- 查到这些记录，审计与历史任务也要引用它们。是否"出现在列表里"由
-- present 决定，这一列只回答"什么时候删的"——那是回收站里唯一能帮用户
-- 判断「这是我上周误删的那台吗」的信息，而 updated_at 会被别的写操作
-- 刷新掉。

ALTER TABLE vm ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
