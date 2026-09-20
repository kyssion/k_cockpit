-- 磁盘吞吐限制（MB/s）：总量 / 读 / 写。
--
-- IOPS 限制的是"每秒多少次操作"，吞吐限制的是"每秒多少字节"。两者针对的
-- 是完全不同的负载：小文件随机读写先撞到 IOPS，大文件顺序读写先撞到吞吐。
-- 只有 IOPS 上限时，一次几百 MB 的连续拷贝不会被任何规则拦住，而那恰恰
-- 是最容易把共享存储打满的操作。
--
-- 三列并存、由服务层校验互斥（与 IOPS 那三列同一套做法）：用"表里只存一种"
-- 来表达互斥，会让用户在两种模式之间切换时丢掉另一组已经设好的值。
--
-- 单位是 MB/s 而不是字节：这个数字是人填的，而人不会填字节。
-- 换算到 libvirt（bytes/s）由节点侧完成——控制面不该替节点决定换算口径。

ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_bytes_total integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_bytes_read  integer NOT NULL DEFAULT 0;
ALTER TABLE vm ADD COLUMN IF NOT EXISTS disk_bytes_write integer NOT NULL DEFAULT 0;
