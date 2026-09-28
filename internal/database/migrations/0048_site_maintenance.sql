-- 站点级维护模式（G-46）。
--
-- 语义与单机面板不同：我们不会「关掉全部再停服务」，而是**按节点逐个进入
-- 维护**并汇总结果——多节点控制面上照抄单机语义，表现为"全部变灰但没有
-- 一台真的被关"。
--
-- 单行表（id 恒为 1）：站点维护是全局状态，不需要多行。
-- node_ids 记录**本次由站点模式接管**的节点：退出时只清这些，管理员手工
-- 设置维护的节点不受牵连——否则"结束站点维护"会顺手解除别人设的维护。
CREATE TABLE IF NOT EXISTS site_maintenance (
    id              bigserial    PRIMARY KEY,
    in_maintenance  boolean      NOT NULL DEFAULT false,
    reason          varchar(255),
    shutdown_vms    boolean      NOT NULL DEFAULT false,
    entered_by      bigint,
    entered_by_name varchar(64),
    entered_at      timestamptz,
    -- 逗号分隔的节点 ID 清单（空串 = 没有接管任何节点）。
    node_ids        text         NOT NULL DEFAULT ''
);

-- 初始化唯一的一行：读取方约定 id=1 存在，不存在即视为「从未进入过维护」，
-- 因此这里只是把行铺好，避免每次 Enter 都要走「不存在则创建」的分支。
INSERT INTO site_maintenance (id, in_maintenance, node_ids)
VALUES (1, false, '')
ON CONFLICT (id) DO NOTHING;
