-- 计算配额补齐「数量型」维度：快照 / 端口转发 / 公网 IP。
--
-- 它们与 vcpu / memory_mb / vm_count 同属一类——**此刻占着多少**，没有
-- 周期、超限不是"限速"而是"拒绝新建"。因此放在同一张表而不是另起一张：
-- 管理员给一个用户配额度时，这六项本来就是一次说完的事（"8 核 / 16 GB /
-- 5 台 / 每机 10 个快照 / 20 条转发 / 2 个公网地址"）；分成两张表的结果
-- 是配了一半，而"这个人到底能建几条转发"要翻两个页面才知道。
--
-- 与周期型配额（resource_quota：流量 / 运行时长）保持分离的理由不变：
-- 那张表按月重置，把长期有效的数量上限放进去会被一起重置掉。
--
-- **0 仍然表示不限**：这是本表一贯的语义，新增列沿用同一个约定，不引入
-- 第二种"上限为多少算不限"的写法。

ALTER TABLE compute_quota ADD COLUMN IF NOT EXISTS snapshots     integer NOT NULL DEFAULT 0;
ALTER TABLE compute_quota ADD COLUMN IF NOT EXISTS port_forwards integer NOT NULL DEFAULT 0;
ALTER TABLE compute_quota ADD COLUMN IF NOT EXISTS public_ips    integer NOT NULL DEFAULT 0;
