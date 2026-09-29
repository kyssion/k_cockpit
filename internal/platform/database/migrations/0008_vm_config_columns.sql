-- ============================================================
-- 0008 虚拟机配置列（F-2-05 编辑页）
-- ------------------------------------------------------------
-- 编辑页的六个子选项卡需要一批当前没有建模的配置项。它们都是**虚拟化层的
-- 属性**（与备注、分组那种纯控制面元数据不同），因此：
--
--   - 修改需要下发到节点，走任务队列；
--   - 大多需要关机才能生效——具体哪些项需要，由 internal/vm/edit.go 的
--     **运行态可改矩阵**定义，不在这里用注释表达（两份说明必然漂移）。
--
-- 所有列都带 DEFAULT：已有行在迁移后立刻有合法值，不需要额外的回填步骤。
-- 这些默认值同时也是「新建虚拟机时用户没特别指定」时的行为，因此取值都
-- 选了最保守、最通用的一档。
-- ============================================================

-- --- 启动与安全（编辑页「启动与安全」子选项卡）---

-- 来宾操作系统类型。影响虚拟化层对硬件呈现方式的默认选择。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS os_type varchar(32) NOT NULL DEFAULT 'linux';

-- 机器类型：q35（较新，支持 PCIe）或 i440fx（兼容老系统）。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS machine_type varchar(32) NOT NULL DEFAULT 'q35';

-- 固件类型：bios 或 uefi。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS firmware varchar(16) NOT NULL DEFAULT 'bios';

-- 安全启动。仅在 firmware = uefi 时有意义。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS secure_boot boolean NOT NULL DEFAULT false;

-- 引导顺序，逗号分隔如 `disk,cdrom,network`。
-- 用字符串而不是数组列：顺序是一个整体，拆成多行反而难以保证「改一次
-- 引导顺序」的原子性。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS boot_order varchar(128) NOT NULL DEFAULT 'disk,cdrom,network';

-- 随宿主机启动而自启。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS auto_start boolean NOT NULL DEFAULT false;

-- 看门狗：none / reset / poweroff / shutdown。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS watchdog varchar(16) NOT NULL DEFAULT 'none';

-- --- 高级设置（编辑页「高级设置」子选项卡）---

-- CPU 型号。host 表示透传宿主机特性（性能最好，但热迁移受限）。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_type varchar(32) NOT NULL DEFAULT 'host';

-- CPU 使用率上限（百分比）。0 表示不限制。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_limit_percent integer NOT NULL DEFAULT 0;

-- 内存是否使用大页。开启可降低 TLB 缺失，但需要宿主机预留。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS memory_hugepages boolean NOT NULL DEFAULT false;

-- APIC / PAE。老系统可能需要关闭它们才能启动。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS apic boolean NOT NULL DEFAULT true;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS pae boolean NOT NULL DEFAULT true;

-- 来宾内是否运行了 QEMU Guest Agent。
--
-- 它是**探测结果**而不是配置：由 agent 上报。放这里是因为「来宾自动化」
-- （f-2-10）的每一项能力都以它为前置，界面上需要有地方显示这个状态。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS guest_agent boolean NOT NULL DEFAULT false;

-- 首次启动的初始化方式：none / nocloud / configdrive / openwrt。
-- 对应 f-2-17，目前仅记录选择，尚未执行注入。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS init_mode varchar(32) NOT NULL DEFAULT 'none';

-- 启动时冻结 CPU（调试用）。开启后虚拟机启动即暂停，需手动恢复。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS freeze_on_start boolean NOT NULL DEFAULT false;

-- --- 磁盘（编辑页「磁盘与驱动器」子选项卡）---

-- 磁盘镜像格式。qcow2 支持快照与精简置备；raw 性能略好但不支持内部快照。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_format varchar(16) NOT NULL DEFAULT 'qcow2';

-- IOPS 限制：总量与读写分离**互斥**（f-2-06）。
--
-- 三个字段都保留是刻意的：「互斥」是业务规则，记录在哪一层由服务层校验，
-- 而不是靠「表里只存一种」来强制。用表结构表达互斥会让用户在切换模式时
-- 丢失另一组已经设好的值。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_iops_total integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_iops_read integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_iops_write integer NOT NULL DEFAULT 0;
