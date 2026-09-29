-- 虚拟机挂载的直通设备（F-4-11 之外的 PCIe 直通能力）。
--
-- **设备清单本身不入库**：它来自节点探测，而设备可热插拔、绑定状态也会被
-- 宿主机上的人手工改动。存一份会出现"库里有、机器上没有"的分歧，而那种
-- 分歧不会报错，只会在用户点"挂载"时以一个看不懂的理由失败。
--
-- 因此本表只存**控制面的意图**：哪个虚拟机挂了哪个 PCI 设备。

CREATE TABLE IF NOT EXISTS vm_passthrough (
    id            bigserial   PRIMARY KEY,
    vm_id         bigint      NOT NULL,
    node_id       bigint      NOT NULL,

    -- pci_address 是设备在宿主机上的 PCI 地址（如 0000:01:00.0）。
    --
    -- 它是**在宿主机上定位设备的唯一标识**，而 vendor:device 不是——同一台
    -- 机器上可以有两块完全相同的卡，而它们的直通分组、所在槽位都不同。
    pci_address   varchar(32) NOT NULL,

    -- device_desc 是挂载时的设备描述，**冗余存一份**。
    --
    -- 设备被拔掉或换到别的槽位之后，探测结果里就没有它了。那时用户需要
    -- 知道"这台机器挂了什么"，而回查探测结果是查不到的——设备已经不在了。
    -- 「当时挂的是什么」是历史的一部分。
    device_desc   varchar(255),

    -- iommu_group 同样是快照。
    --
    -- 它的作用是在排查时回答"当初是不是因为同组冲突才出的问题"。分组会
    -- 随硬件与拓扑变化，因此不能当作当前值使用，只能当作历史。
    iommu_group   integer,

    remark        varchar(255),
    attached_at   timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- 一台虚拟机上同一个 PCI 地址只能挂一次。
--
-- 挂两次的含义是"把同一块卡直通给同一台机器两遍"——那在 libvirt 层面
-- 会生成两条 hostdev，而只有一条能真正生效。用户看到的却是"我明明挂上了"。
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_passthrough_vm_addr
    ON vm_passthrough (vm_id, pci_address);

CREATE INDEX IF NOT EXISTS idx_vm_passthrough_node
    ON vm_passthrough (node_id);
