-- 模板族：把「同一条派生链上的模板」标识出来。
--
-- 此前只有 parent_id 与 version 两列，而 version **从未被写过**（恒为 1）。
-- 结果是界面只能显示"派生自 #N"——一个扁平的父子关系，既看不出这族有
-- 几个版本，也无从判断删掉中间一个会波及哪些。
--
-- family_id 取**根模板的 ID**，整条链共享：
--
--	- 判断"是不是同一族"是一次相等比较，而不是递归查 parent_id；
--	- 删除策略（级联 / 提升）要作用的范围就是 `family_id = ?` 的子树；
--	- 版本在同一族内自增，跨族不冲突——两个不同的模板都可以有 v1。
--
-- 版本用同一族内的最大值 +1，而不是 parent.version + 1：同一父下派生两次
-- 应当得到 v2 与 v3，若按父版本算则两者都叫 v2，而名字唯一约束会让第二
-- 次制备直接失败。
ALTER TABLE template ADD COLUMN IF NOT EXISTS family_id bigint;
CREATE INDEX IF NOT EXISTS idx_template_family ON template (family_id);

-- 模板的**默认硬件配置**。
--
-- 它们此前只存在于虚拟机侧（vm.disk_bus / vm.nic_model / vm.machine_type
-- / vm.firmware，迁移 0008），模板侧只有 default_cpu 与 default_memory_mb。
-- 于是"从模板克隆"只能继承 CPU 与内存——磁盘驱动会退回 VirtIO、机型退回
-- 默认，一台原本用 SATA 的 Windows 模板克隆出来可能起不来。
--
-- 用独立列而不是塞进 default_spec 那个 JSON 字段：这些值都取自**同一份
-- 配置矩阵**（与创建向导、编辑页同源），独立列才能在服务端校验取值，也
-- 才能在列表上直接展示。JSON 字段适合存没有固定形状的扩展信息，不适合
-- 存有候选集的枚举。
ALTER TABLE template ADD COLUMN IF NOT EXISTS default_disk_bus     varchar(16);
ALTER TABLE template ADD COLUMN IF NOT EXISTS default_nic_model    varchar(32);
ALTER TABLE template ADD COLUMN IF NOT EXISTS default_video_model  varchar(32);
ALTER TABLE template ADD COLUMN IF NOT EXISTS default_machine_type varchar(64);
ALTER TABLE template ADD COLUMN IF NOT EXISTS default_firmware     varchar(16);
