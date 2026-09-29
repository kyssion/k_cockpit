-- 创建向导的补充配置维度（F-2-02 后续迭代，DEMO_PLAN G-29）。
--
-- 显示设备 / RTC / 架构：影响虚拟化层对硬件的呈现方式，与 machine_type、
-- firmware 同属「这台机器第一次开机就该是什么样」的一部分——建好再改要关机，
-- 创建时一次给定最省事。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS video_model varchar(16) NOT NULL DEFAULT 'virtio';
ALTER TABLE vm ADD COLUMN IF NOT EXISTS rtc_mode varchar(16) NOT NULL DEFAULT 'utc';
ALTER TABLE vm ADD COLUMN IF NOT EXISTS arch varchar(16) NOT NULL DEFAULT 'x86_64';

-- CPU / 内存热添加开关（F-2-05 运行态可改矩阵的前置）。
--
-- 运行态热扩的前提是创建时打开了热添加：libvirt 需要在域定义里预置
-- hotpluggable 的设备槽与内存气球设备。没有这个开关，「编辑页能不能热改」
-- 就只能靠猜，两处迟早对不上。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_hotplug boolean NOT NULL DEFAULT false;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS memory_hotplug boolean NOT NULL DEFAULT false;
