-- 维护模式的原因与时间（F-6-05）。
--
-- 独立成列而不是复用 node.remark：remark 是用户自己的备注，写入维护原因
-- 会把它**悄悄覆盖掉**——用户在维护结束后会发现自己的备注变成了一句
-- 已经不成立的「升级内核」。两个不同用途的文本共用一列，代价总在事后才显现。
--
-- 与业务软锁（vm_lock.reason / locked_at）保持一致的口径：原因与时刻
-- 随状态同进同退，退出维护时一起清空，不留「未在维护、但原因是……」这种
-- 界面无法解释的状态。
--
-- 迁移只增不改：这里用 ADD COLUMN IF NOT EXISTS，可重复执行。
ALTER TABLE node ADD COLUMN IF NOT EXISTS maintenance_reason varchar(255);
ALTER TABLE node ADD COLUMN IF NOT EXISTS maintenance_at timestamptz;
