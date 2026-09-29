-- 模板克隆（F-3-02）：记录克隆方式与链式依赖。
--
-- clone_mode 区分 full 与 linked。它不只是个标签——两者的运维含义完全不同：
-- 完整克隆的虚拟机与模板**完全独立**，模板删了也不影响；链式克隆的磁盘只是
-- 一个 overlay，**父盘缺失或被改，数据就不可用了**。界面要据此显示依赖链，
-- 删除父模板时也要据此拒绝（或明确告知会破坏多少个克隆体）。
--
-- backing_path 记录链式克隆的父磁盘路径。没有它就无法回答「这个克隆体依赖
-- 谁」——而那是排查「虚拟机起不来」时第一个要看的东西。路径由节点在克隆时
-- 返回并写入，控制面只保存不解释。
--
-- 迁移只增不改：ADD COLUMN IF NOT EXISTS 可重复执行。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS clone_mode varchar(16) NOT NULL DEFAULT 'full';
ALTER TABLE vm ADD COLUMN IF NOT EXISTS backing_path varchar(512);

CREATE INDEX IF NOT EXISTS idx_vm_template_id ON vm (template_id);
