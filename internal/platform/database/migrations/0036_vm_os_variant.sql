-- 虚拟机的具体系统版本（libosinfo short id，如 ubuntu24.04 / win11）。
--
-- 与 os_type（linux / windows / other）的关系：后者是**大类**，决定虚拟化层
-- 呈现硬件的大方向；这一列是**具体版本**，决定更细的默认设备与驱动建议。
-- 合到一列会让"知道它是 Linux"与"知道它是 Ubuntu 24.04"这两种精度混在一起，
-- 而前者在没有识别结果时也必须有值（默认 linux），后者可以为空。
--
-- 可为空：多数机器是从镜像装的，只有镜像被识别过（f-5-05）或在向导里手选
-- 才有值。空串与 NULL 都按"未指定"处理。
ALTER TABLE vm ADD COLUMN IF NOT EXISTS os_variant varchar(64);
