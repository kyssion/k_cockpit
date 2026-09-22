-- 创建向导补齐的高级字段（F-2-02 的后续迭代）。
--
-- CPU 拓扑：vcpu 只说"给几个线程"，而拓扑决定它们在来宾里呈现成几路几核。
-- 有些系统的授权与调度按**物理 CPU 路数**计算，同样 8 线程，"2 路 4 核"与
-- "1 路 8 核"在来宾里不是一回事。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_sockets  integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_cores    integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS cpu_threads  integer NOT NULL DEFAULT 0;

-- 隐藏 KVM：让来宾看不到自己是虚拟机。
--
-- 有合法用途（某些旧授权软件检测到虚拟化后拒绝运行），也有代价（失去
-- 半虚拟化驱动的部分优化）。因此它是**默认关闭**的显式选项，而不是默认行为。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS hide_kvm boolean NOT NULL DEFAULT false;

-- 嵌套虚拟化：让虚拟机里再跑虚拟机。
--
-- 默认关闭：开销是实打实的，而绝大多数机器用不上。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS nested_virt boolean NOT NULL DEFAULT false;

-- 软盘镜像。用文件 ID 而不是路径：它来自「我的存储」，而路径是节点侧的
-- 内部细节，让用户填路径等于让他去猜节点上的目录结构。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS floppy_file_id bigint;
