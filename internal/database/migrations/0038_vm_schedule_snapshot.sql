-- 定时任务的「创建快照」动作所需的两列。
--
-- snapshot_name 是快照名模板（留空由服务端按时间生成）。
-- include_memory 表示是否保存运行现场。
--
-- 单独两列而不是塞进某个 JSON 字段：它们会被调度器在**每次触发时**读取，
-- 打成一个 JSON 再解析只会让"这个动作要哪些参数"变得不可见。

ALTER TABLE vm_schedule ADD COLUMN IF NOT EXISTS snapshot_name varchar(128);
ALTER TABLE vm_schedule ADD COLUMN IF NOT EXISTS include_memory boolean NOT NULL DEFAULT false;
