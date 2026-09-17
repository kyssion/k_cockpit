-- 物理口入桥的自动回滚窗口。
--
-- f-4-01 要求「涉及宿主机网络连通性的操作（物理口入桥）必须可回滚且有显式
-- 确认」。风险是具体的：把一个物理口加到桥上会**重置它的 IP 配置**——如果
-- 那恰好是管理口（或管理流量经过它），操作者当场失联，而那时他已经没有
-- 任何界面路径可以改回来。
--
-- 「显式确认」只能防住「没想清楚就点了」这一种情况；另一种情况是**确认过
-- 之后才发现不行**——口加进去、网络断了、面板打不开了。那时需要的是一条
-- 不依赖界面的退路。
--
-- 因此除了确认，还要有一个到点自动摘口的窗口。执行者在**节点侧**，理由与
-- 端口镜像（迁移未涉及，见 portmirror 包）完全相同：控制面是通过网络下发
-- 指令的，而这个操作的结果恰恰可能是网络断掉——一个依赖网络的保险，在它
-- 最需要起作用的时候一定不在。
--
-- 本列是控制面对这件事的**记录**（用于界面倒计时与状态对齐），不是执行者。
ALTER TABLE network_bridge ADD COLUMN IF NOT EXISTS uplink_watchdog_until timestamptz;

-- 按到期时刻查「哪些桥的窗口还没走完」：对齐任务要按它扫描。
CREATE INDEX IF NOT EXISTS idx_network_bridge_watchdog
    ON network_bridge (uplink_watchdog_until)
    WHERE uplink_watchdog_until IS NOT NULL;
