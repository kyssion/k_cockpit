-- 虚拟机的光驱（F-2-06 / G-21）。
--
-- 做成一**张表**（可以有多个光驱）而不是 VM 上的三个字段。理由不是"更接近
-- libvirt"，而是那三个字段表达不了顺序：多个光驱必须有稳定的编号（ide0 /
-- ide1 / sata0 …），否则加一个、删一个之后**设备号会漂**——而来宾里
-- /dev/sr0 与 /dev/sr1 就对调了，用户按上次记的设备名去找会找错。
--
-- （storage_file 上的注释一直写着「ISO 可以被挂载到虚拟机的光驱上」，
--   但那件事从未实现——这一版是第一次把它接上。）

CREATE TABLE IF NOT EXISTS vm_cdrom (
    id         bigserial   PRIMARY KEY,
    vm_id      bigint      NOT NULL,
    node_id    bigint      NOT NULL,

    -- order_no 是光驱序号，从 0 开始，在**同一台虚拟机内唯一**。
    --
    -- 它是设备号的来源：来宾里看到的就是按这个顺序排的 /dev/srN。
    -- 不管它的话，"删掉 0 号再加一个"会让原来的 1 号变成 0 号，
    -- 而来宾里挂载脚本写死的设备名就指到别的盘上了。
    order_no   integer     NOT NULL,

    -- iso_file_id 指向 storage_file（category = iso）。
    --
    -- 为空表示**光驱在但没放盘**——即"弹出"的状态。它与"没有光驱"是
    -- 两件事：前者在来宾里看得到一个空的托盘，后者连设备都没有。
    -- 混为一谈的话，用户在来宾里找不到设备而不知道是哪种情况。
    iso_file_id bigint,

    -- bus 取值 ide / sata / scsi。
    --
    -- 默认 sata：它在绝大多数来宾里都能被识别，而且支持热插拔；
    -- ide 出现在老系统里，scsi 需要驱动。**换 bus 几乎一定要重启**，
    -- 而这一点要写进接口说明——不写的话，用户改完看不到变化会以为没生效。
    bus        varchar(8)  NOT NULL DEFAULT 'sata',

    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- 同一台虚拟机内光驱序号唯一。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_cdrom_vm_order
    ON vm_cdrom (vm_id, order_no);

CREATE INDEX IF NOT EXISTS idx_vm_cdrom_node ON vm_cdrom (node_id);

-- 同一个 ISO 文件可以被多台虚拟机同时挂载（读多份是允许的），
-- 因此这里**不加** (node_id, iso_file_id) 的唯一约束。
