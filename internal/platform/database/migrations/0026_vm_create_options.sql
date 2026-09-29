-- 创建向导新增的两个硬件选项（F-2-02）。
--
-- 两者都是**创建时选定、之后长期有效**的事实，因此落在 vm 上而不是只留在
-- 创建参数里：详情页要显示「这台机器用的是哪种磁盘控制器 / 网卡型号」，
-- 而任务参数是一次性的（它描述的是那次操作，不是这台机器的现状）。
--
-- 默认值取 virtio 而不是兼容性更好的 e1000 / ide：半虚拟化在同样硬件上
-- 的吞吐高一个量级，而缺驱动的代价（装系统时加载 virtio 驱动）只发生在
-- 首次安装，且可由用户在向导里改。把「偶尔要改一次」的成本留给少数人，
-- 而不是让所有人的机器都跑在兼容模式下。

ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_bus varchar(16) NOT NULL DEFAULT 'virtio';
ALTER TABLE vm ADD COLUMN IF NOT EXISTS nic_model varchar(16) NOT NULL DEFAULT 'virtio';
