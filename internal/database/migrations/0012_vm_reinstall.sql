-- 重装系统（F-2-11）。
--
-- 重装会**替换整块系统盘**——这是本项目里对单台虚拟机破坏性最强的操作
-- （比删除轻一档，但同样不可逆）。因此过程分两步走：先把原系统盘改名成
-- 备份，再用模板造一块新的。
--
-- reinstall_backup 记录那份备份盘的位置。它有两个用途：
--   1. 重装过程中任何一步失败，用它还原回原来的系统；
--   2. 重装**成功之后**它依然存在——那是用户的数据，什么时候回收由他决定，
--      系统不能替他做主删掉。界面据此提示「有一份 N GB 的备份可清理」。
--
-- 只有一份备份：第二次重装会覆盖上一次的。这一点在服务层显式拒绝而不是
-- 静默覆盖——静默覆盖会让用户失去「回到上一个系统」这个唯一的退路，而
-- 他可能正指望那条退路。
--
-- 迁移只增不改：ADD COLUMN IF NOT EXISTS 可重复执行。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS reinstall_backup varchar(512);
ALTER TABLE vm ADD COLUMN IF NOT EXISTS reinstall_at timestamptz;
