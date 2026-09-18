-- 宿主机防火墙（F-4-11 第一层）。
--
-- **与 KVM 网络防火墙（firewall_policy / firewall_rule）是两套。**
-- 用独立的表名而不是给现有表加一个 layer 列，正是因为这两个概念最容易
-- 被混为一谈：
--
--   宿主机防火墙  保护的是**宿主机自己与面板**——SSH、面板端口、节点上
--                 直接对外的服务。它防的是"谁能登进这台机器"。
--   KVM 防火墙    保护的是**虚拟机**——作用在所有虚拟机的入站流量上。
--                 它防的是"谁能访问虚拟机里的服务"。
--
-- 混在一起的后果不是代码难看，而是**用户会以为改了 KVM 规则就关掉了
-- 面板的暴露面**——那是两个完全不同的攻击面，而它们的配置项长得几乎一样。

CREATE TABLE IF NOT EXISTS host_firewall_policy (
    id             bigserial   PRIMARY KEY,
    node_id        bigint      NOT NULL,

    enabled        boolean     NOT NULL DEFAULT false,

    -- default_action 是不匹配任何规则时的处置。
    --
    -- 默认 deny：防火墙的价值就在于**默认拒绝**，而 default accept 只是一组
    -- 「特定来源不许进」的例外清单——它能防的事情比它看起来能防的少得多。
    default_action varchar(8)  NOT NULL DEFAULT 'deny',

    -- 管理白名单（CIDR，逗号或换行分隔），**优先于一切拒绝规则**。
    --
    -- 这条优先级不是便利性设计，而是安全底线：管理员从某个固定 IP 管理面板，
    -- 而那个 IP 万一落在被拒绝的范围里，**他会把自己锁在门外**——而那时
    -- 他已经连不上面板去改回来了。宿主机防火墙比 KVM 那层更危险：它挡住的
    -- 是 SSH 与面板本身，没有任何"从里面绕过去"的余地。
    whitelist      text,

    -- version 每次改动自增，供「预览 → 应用」校验（与 f-4-04 同一模式）。
    version        integer     NOT NULL DEFAULT 0,

    -- applied_at 为空表示「有配置但没生效过」。
    --
    -- 它与 version 是两件事：version 是控制面的配置版本，applied_at 是
    -- 宿主机的实际状态。两者不一致时用户需要知道「我改的东西还没生效」。
    applied_at     timestamptz,

    -- last_rollback_at 记录最近一次紧急回滚的时刻。
    --
    -- 回滚本身要留痕：它意味着"刚才那次应用把机器弄坏了"。事后看审计能
    -- 查到是谁点的，而这一列让界面能直接说出「这条策略最近被回滚过」——
    -- 那正是排查"为什么规则和我配的不一样"时第一个要看的东西。
    last_rollback_at timestamptz,

    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uniq_host_firewall_policy_node_id
    ON host_firewall_policy (node_id);

CREATE TABLE IF NOT EXISTS host_firewall_rule (
    id           bigserial    PRIMARY KEY,
    node_id      bigint       NOT NULL,

    action       varchar(8)   NOT NULL,
    protocol     varchar(8)   NOT NULL DEFAULT 'tcp',
    port_start   integer,
    port_end     integer,

    -- source_cidr 为空表示任意来源。
    source_cidr  varchar(64),

    -- geoip_regions 是该规则允许的区域（逗号分隔的国家码），为空表示不按区域限制。
    geoip_regions varchar(512),

    -- is_protected 标记**不可被界面修改或删除**的规则。
    --
    -- 这类规则保护的是**管理通道本身**：面板自己的监听端口、SSH 端口。
    -- 它们必须存在，否则一次「清理规则」的操作就能把管理员关在门外——
    -- 而那种事故**无法通过面板恢复**，只能上宿主机敲命令（甚至只能进机房）。
    --
    -- 因此保护是**服务端强制的**，界面上的标红只是提示。让界面决定能不能删，
    -- 等于把一个不可恢复的操作交给一次点击。
    is_protected boolean      NOT NULL DEFAULT false,

    -- applied 标记该规则是否已下发到宿主机。
    --
    -- 新增一条规则后没下发时，界面要能指出**是哪一条**还没生效，而不是
    -- 笼统地说「有改动未应用」。
    applied      boolean      NOT NULL DEFAULT false,

    remark       varchar(255),
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_host_firewall_rule_node_protected
    ON host_firewall_rule (node_id, is_protected);
