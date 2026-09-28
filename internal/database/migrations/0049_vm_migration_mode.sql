-- 跨节点迁移的方式（F-2-15）。
--
-- live（热迁移）此前一直没有：迁移一律要求先关机。此列记录每次迁移走
-- 的方式，供迁移历史回答"那次是停机搬的还是不停机搬的"——两种方式的
-- 业务影响（停顿时长、失败后的现场）完全不同，排查时先要分清。
--
-- 存量行不回填：默认值 offline 与当时唯一可用的方式一致。
ALTER TABLE vm_migration ADD COLUMN IF NOT EXISTS mode varchar(8) NOT NULL DEFAULT 'offline';
