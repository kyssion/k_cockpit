-- 虚拟机导出（F-2-14）。
--
-- 独立成表而不是复用 task.result：导出产物是**长期存在的东西**，用户可以
-- 下载、可以删除，而任务记录是可以被清理的运维数据。把产物挂在任务上，
-- 等于让「三个月前导出的那个镜像」随着任务清理一起消失。
--
-- file_path 由节点在导出完成后返回并写入。控制面只保存不解释这个路径——
-- 它可能落在某个存储池的导出目录里，具体在哪由实现决定。
--
-- status 与 task.status 用同一套词汇（pending/running/success/failed），
-- 避免界面上出现两种说法指着同一件事。
CREATE TABLE IF NOT EXISTS vm_export (
    id                  bigserial    PRIMARY KEY,
    vm_id               bigint       NOT NULL,
    node_id             bigint       NOT NULL,
    vm_name             varchar(128),
    format              varchar(16)  NOT NULL DEFAULT 'qcow2',
    include_data_disks  boolean      NOT NULL DEFAULT false,
    status              varchar(16)  NOT NULL DEFAULT 'pending',
    file_path           varchar(512),
    file_name           varchar(255),
    size_bytes          bigint       NOT NULL DEFAULT 0,
    error               varchar(512),
    created_by          bigint,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    deleted_at          timestamptz
);

CREATE INDEX IF NOT EXISTS idx_vm_export_vm_id ON vm_export (vm_id);
