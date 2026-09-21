-- VPC 网络的 ACL 规则（F-4-05）。
--
-- 与安全组规则（security_group_rule）分表：前者挂在**交换机（网段）**上，
-- 后者挂在虚拟机网口上。它们的作用域不同，合表的代价是每个查询都要多带
-- 一个"这是哪种作用域"的条件，漏掉一处就会把网段级规则当成单台机器的。
--
-- switch_id 可为空：空表示这条是节点级默认，作用于该节点的全部 VPC 网络。
-- 没有它，"这个节点上所有网段都不许访问某地址"就要在每个网段上各写一遍。
--
-- priority 小的先匹配（与 iptables 一致）。同优先级时按 id，保证顺序稳定——
-- 顺序不稳定的规则集在每次应用后可能产生不同的结果，而那是最难复现的一类
-- 故障。
CREATE TABLE IF NOT EXISTS vpc_acl_rule (
    id         bigserial    PRIMARY KEY,
    node_id    bigint       NOT NULL,
    switch_id  bigint,

    priority   integer      NOT NULL DEFAULT 100,
    action     varchar(8)   NOT NULL,
    direction  varchar(4)   NOT NULL DEFAULT 'in',
    protocol   varchar(8)   NOT NULL DEFAULT 'any',

    src_cidr   varchar(64),
    dst_cidr   varchar(64),

    port_start integer,
    port_end   integer,

    enabled    boolean      NOT NULL DEFAULT true,
    remark     varchar(255),

    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_vpc_acl_switch ON vpc_acl_rule (node_id, switch_id);
CREATE INDEX IF NOT EXISTS idx_vpc_acl_order  ON vpc_acl_rule (node_id, switch_id, priority, id);
