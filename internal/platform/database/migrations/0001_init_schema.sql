-- ============================================================
-- 0001_init_schema —— k_cockpit 初始表结构（43 张表）
-- ------------------------------------------------------------
-- 目标库：PostgreSQL 14+
-- 设计依据：docs/02-architecture/DATA_MODEL.md（表名单数、无数据库外键、
--           JSON 统一 text、资源表带 node_id、任务落库）
-- 可重复执行：全部语句使用 IF NOT EXISTS，重复执行不报错
-- 注意：本脚本为 PostgreSQL 专用；SQLite（本地开发）走 GORM AutoMigrate
-- ============================================================


-- ============================================================
-- 一、身份与安全（7）
-- ============================================================

CREATE TABLE IF NOT EXISTS "user" (
    id                    bigserial    PRIMARY KEY,
    username              varchar(64)  NOT NULL,
    password_hash         varchar(255) NOT NULL,
    role                  varchar(16)  NOT NULL DEFAULT 'tenant',
    status                varchar(16)  NOT NULL DEFAULT 'pending',
    email                 varchar(128),
    email_verified_at     timestamptz,
    totp_secret_enc       text,
    totp_enabled          boolean      NOT NULL DEFAULT false,
    recovery_codes_hash   text,
    bootstrap_skipped     boolean      NOT NULL DEFAULT false,
    force_password_change boolean      NOT NULL DEFAULT false,
    security_updated_at   timestamptz,
    breach_checked_at     timestamptz,
    breach_hit            boolean      NOT NULL DEFAULT false,
    ssh_access_enabled    boolean      NOT NULL DEFAULT false,
    max_cpu               integer      NOT NULL DEFAULT 0,
    max_memory_mb         integer      NOT NULL DEFAULT 0,
    max_disk_gb           integer      NOT NULL DEFAULT 0,
    max_vm                integer      NOT NULL DEFAULT 0,
    max_storage_gb        integer      NOT NULL DEFAULT 0,
    max_bandwidth_mbps    integer      NOT NULL DEFAULT 0,
    max_traffic_gb        integer      NOT NULL DEFAULT 0,
    max_public_ips        integer      NOT NULL DEFAULT 0,
    max_port_forwards     integer      NOT NULL DEFAULT 0,
    max_snapshots         integer      NOT NULL DEFAULT 0,
    max_runtime_hours     integer      NOT NULL DEFAULT 0,
    remark                varchar(255),
    created_at            timestamptz  NOT NULL DEFAULT now(),
    updated_at            timestamptz  NOT NULL DEFAULT now(),
    deleted_at            timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_username ON "user" (username);
CREATE INDEX IF NOT EXISTS idx_user_role_status ON "user" (role, status);
CREATE INDEX IF NOT EXISTS idx_user_email ON "user" (email);

CREATE TABLE IF NOT EXISTS user_session (
    id             bigserial   PRIMARY KEY,
    session_id     varchar(64) NOT NULL,
    user_id        bigint      NOT NULL,
    token_type     varchar(24) NOT NULL DEFAULT 'access',
    fingerprint    varchar(128),
    client_ip      varchar(64),
    user_agent     varchar(255),
    issued_at      timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    last_active_at timestamptz,
    revoked_at     timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_session_session_id ON user_session (session_id);
CREATE INDEX IF NOT EXISTS idx_user_session_user_id ON user_session (user_id);
CREATE INDEX IF NOT EXISTS idx_user_session_expires_at ON user_session (expires_at);

CREATE TABLE IF NOT EXISTS user_api_key (
    id           bigserial    PRIMARY KEY,
    user_id      bigint       NOT NULL,
    key_prefix   varchar(16)  NOT NULL,
    key_hash     varchar(128) NOT NULL,
    allowed_ips  text,
    expires_at   timestamptz,
    revoked_at   timestamptz,
    last_used_at timestamptz,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_api_key_user_id ON user_api_key (user_id);
CREATE INDEX IF NOT EXISTS idx_user_api_key_prefix ON user_api_key (key_prefix);

CREATE TABLE IF NOT EXISTS auth_action_token (
    id         bigserial    PRIMARY KEY,
    user_id    bigint       NOT NULL,
    purpose    varchar(32)  NOT NULL,
    token_hash varchar(128) NOT NULL,
    expires_at timestamptz  NOT NULL,
    used_at    timestamptz,
    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_auth_action_token_token_hash ON auth_action_token (token_hash);
CREATE INDEX IF NOT EXISTS idx_auth_action_token_user_purpose ON auth_action_token (user_id, purpose);

CREATE TABLE IF NOT EXISTS security_challenge (
    id          bigserial    PRIMARY KEY,
    user_id     bigint       NOT NULL,
    purpose     varchar(32)  NOT NULL,
    code_hash   varchar(128) NOT NULL,
    target      varchar(255),
    expires_at  timestamptz  NOT NULL,
    consumed_at timestamptz,
    attempts    integer      NOT NULL DEFAULT 0,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_security_challenge_user_purpose ON security_challenge (user_id, purpose);
CREATE INDEX IF NOT EXISTS idx_security_challenge_expires_at ON security_challenge (expires_at);

CREATE TABLE IF NOT EXISTS audit_log (
    id            bigserial    PRIMARY KEY,
    at            timestamptz  NOT NULL DEFAULT now(),
    operator_id   bigint,
    operator_name varchar(64),
    source        varchar(16)  NOT NULL DEFAULT 'web',
    node_id       bigint,
    resource_type varchar(32)  NOT NULL,
    resource_id   bigint,
    resource_name varchar(128),
    action        varchar(48)  NOT NULL DEFAULT '',
    params        text,
    before_state  text,
    after_state   text,
    success       boolean      NOT NULL DEFAULT true,
    error         varchar(512),
    client_ip     varchar(64)
);
CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log (at);
CREATE INDEX IF NOT EXISTS idx_audit_log_resource ON audit_log (resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_audit_log_operator_id ON audit_log (operator_id);

CREATE TABLE IF NOT EXISTS system_setting (
    key        varchar(128) PRIMARY KEY,
    value      text,
    updated_by bigint,
    updated_at timestamptz  NOT NULL DEFAULT now()
);


-- ============================================================
-- 二、节点（1）
-- ============================================================

CREATE TABLE IF NOT EXISTS node (
    id                  bigserial    PRIMARY KEY,
    name                varchar(64)  NOT NULL,
    api_base_url        varchar(255),
    api_id              varchar(64),
    api_key_enc         text,
    ssh_host            varchar(255),
    ssh_port            integer      NOT NULL DEFAULT 22,
    ssh_user            varchar(64),
    ssh_auth_type       varchar(16)  NOT NULL DEFAULT 'key',
    ssh_password_enc    text,
    ssh_private_key_enc text,
    enabled             boolean      NOT NULL DEFAULT true,
    status              varchar(16)  NOT NULL DEFAULT 'unknown',
    maintenance_mode    boolean      NOT NULL DEFAULT false,
    is_migration_target boolean      NOT NULL DEFAULT true,
    capabilities        text,
    last_probe_at       timestamptz,
    last_probe_message  varchar(255),
    probe_detail        text,
    remark              varchar(255),
    created_at          timestamptz  NOT NULL DEFAULT now(),
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    deleted_at          timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_node_name ON node (name);
CREATE INDEX IF NOT EXISTS idx_node_enabled_status ON node (enabled, status);


-- ============================================================
-- 三、存储（6）
-- ============================================================

CREATE TABLE IF NOT EXISTS storage_pool (
    id           bigserial    PRIMARY KEY,
    node_id      bigint       NOT NULL,
    device_id    varchar(128) NOT NULL,
    device_path  varchar(255),
    kind         varchar(16)  NOT NULL DEFAULT 'local',
    fs_type      varchar(16),
    mount_path   varchar(255),
    total_bytes  bigint       NOT NULL DEFAULT 0,
    usable_bytes bigint       NOT NULL DEFAULT 0,
    is_default   boolean      NOT NULL DEFAULT false,
    status       varchar(16)  NOT NULL DEFAULT 'ready',
    remark       varchar(255),
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_pool_node_device ON storage_pool (node_id, device_id);
CREATE INDEX IF NOT EXISTS idx_storage_pool_node_id ON storage_pool (node_id);
-- 同一节点至多一个默认存储池
CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_pool_default ON storage_pool (node_id) WHERE is_default;

CREATE TABLE IF NOT EXISTS storage_volume (
    id            bigserial   PRIMARY KEY,
    node_id       bigint      NOT NULL,
    name          varchar(64) NOT NULL,
    kind          varchar(16) NOT NULL DEFAULT 'lvm',
    vg_name       varchar(64),
    lv_name       varchar(64),
    size_gb       integer     NOT NULL DEFAULT 0,
    stripe_count  integer     NOT NULL DEFAULT 0,
    mirror_count  integer     NOT NULL DEFAULT 0,
    devices       text,
    status        varchar(16) NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_volume_node_name ON storage_volume (node_id, name);

CREATE TABLE IF NOT EXISTS user_storage (
    id             bigserial   PRIMARY KEY,
    user_id        bigint      NOT NULL,
    node_id        bigint      NOT NULL,
    enabled        boolean     NOT NULL DEFAULT false,
    quota_bytes    bigint      NOT NULL DEFAULT 0,
    used_bytes     bigint      NOT NULL DEFAULT 0,
    read_only      boolean     NOT NULL DEFAULT false,
    root_path      varchar(512),
    initialized_at timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_storage_user_node ON user_storage (user_id, node_id);

-- 上传登记与文件索引：sha256 用于秒传，ISO 元数据用于创建向导；文件实体在存储池
CREATE TABLE IF NOT EXISTS storage_file (
    id           bigserial    PRIMARY KEY,
    node_id      bigint       NOT NULL,
    user_id      bigint,
    rel_path     varchar(512) NOT NULL,
    category     varchar(16)  NOT NULL,
    filename     varchar(255) NOT NULL,
    size_bytes   bigint       NOT NULL DEFAULT 0,
    sha256       varchar(64),
    os_type      varchar(64),
    os_variant   varchar(64),
    min_disk_gb  integer      NOT NULL DEFAULT 0,
    uploaded_at  timestamptz,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_storage_file_rel_path ON storage_file (node_id, rel_path);
CREATE INDEX IF NOT EXISTS idx_storage_file_sha256 ON storage_file (sha256);

CREATE TABLE IF NOT EXISTS upload_session (
    upload_id       varchar(64)  PRIMARY KEY,
    target          varchar(32)  NOT NULL,
    file_key        varchar(255) NOT NULL,
    owner_id        bigint,
    node_id         bigint,
    total_size      bigint       NOT NULL DEFAULT 0,
    chunk_size      integer      NOT NULL DEFAULT 0,
    received_bitmap text,
    sha256          varchar(64),
    status          varchar(16)  NOT NULL DEFAULT 'pending',
    expires_at      timestamptz  NOT NULL,
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_upload_session_file_key ON upload_session (file_key);
CREATE INDEX IF NOT EXISTS idx_upload_session_expires_at ON upload_session (expires_at);

CREATE TABLE IF NOT EXISTS share_mount (
    id             bigserial    PRIMARY KEY,
    vm_id          bigint       NOT NULL,
    node_id        bigint       NOT NULL,
    host_path      varchar(512) NOT NULL,
    tag            varchar(64)  NOT NULL,
    security_model varchar(16)  NOT NULL DEFAULT 'mapped',
    read_only      boolean      NOT NULL DEFAULT true,
    mounted_at     timestamptz,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_share_mount_vm_tag ON share_mount (vm_id, tag);


-- ============================================================
-- 四、网络（16）
-- ============================================================

CREATE TABLE IF NOT EXISTS network_bridge (
    id           bigserial   PRIMARY KEY,
    node_id      bigint      NOT NULL,
    name         varchar(64) NOT NULL,
    backend      varchar(16) NOT NULL DEFAULT 'bridge',
    mode         varchar(16) NOT NULL DEFAULT 'nat',
    uplink_if    varchar(64),
    cidr         varchar(64),
    gateway_ip   varchar(64),
    dhcp_start   varchar(64),
    dhcp_end     varchar(64),
    dhcp_enabled boolean     NOT NULL DEFAULT false,
    vlan_id      integer,
    is_system    boolean     NOT NULL DEFAULT false,
    status       varchar(16) NOT NULL DEFAULT 'active',
    remark       varchar(255),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_network_bridge_node_name ON network_bridge (node_id, name);
CREATE INDEX IF NOT EXISTS idx_network_bridge_node_system ON network_bridge (node_id, is_system);

CREATE TABLE IF NOT EXISTS vpc_switch (
    id                    bigserial   PRIMARY KEY,
    node_id               bigint      NOT NULL,
    owner_id              bigint,
    name                  varchar(64) NOT NULL,
    mode                  varchar(16) NOT NULL DEFAULT 'empty',
    bridge_name           varchar(64) NOT NULL,
    vlan_id               integer,
    cidr                  varchar(64),
    gateway_ip            varchar(64),
    dhcp_start            varchar(64),
    dhcp_end              varchar(64),
    uplink_if             varchar(64),
    bandwidth_in_mbps     integer     NOT NULL DEFAULT 0,
    bandwidth_out_mbps    integer     NOT NULL DEFAULT 0,
    traffic_quota_in_gb   integer     NOT NULL DEFAULT 0,
    traffic_quota_out_gb  integer     NOT NULL DEFAULT 0,
    is_system             boolean     NOT NULL DEFAULT false,
    status                varchar(16) NOT NULL DEFAULT 'active',
    remark                varchar(255),
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    deleted_at            timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vpc_switch_node_name ON vpc_switch (node_id, name);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vpc_switch_node_vlan ON vpc_switch (node_id, vlan_id);

CREATE TABLE IF NOT EXISTS security_group (
    id         bigserial    PRIMARY KEY,
    node_id    bigint       NOT NULL,
    owner_id   bigint,
    name       varchar(64)  NOT NULL,
    is_default boolean      NOT NULL DEFAULT false,
    remark     varchar(255),
    created_at timestamptz  NOT NULL DEFAULT now(),
    updated_at timestamptz  NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_security_group_node_name ON security_group (node_id, name);
CREATE INDEX IF NOT EXISTS idx_security_group_owner_id ON security_group (owner_id);

CREATE TABLE IF NOT EXISTS security_group_rule (
    id             bigserial   PRIMARY KEY,
    group_id       bigint      NOT NULL,
    direction      varchar(8)  NOT NULL,
    protocol       varchar(8)  NOT NULL DEFAULT 'tcp',
    port_start     integer,
    port_end       integer,
    target_type    varchar(16) NOT NULL DEFAULT 'cidr',
    target_value   varchar(64),
    address_family varchar(8)  NOT NULL DEFAULT 'ipv4',
    priority       integer     NOT NULL DEFAULT 100,
    remark         varchar(255),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_security_group_rule_group_id ON security_group_rule (group_id);

CREATE TABLE IF NOT EXISTS vm_interface (
    id                bigserial   PRIMARY KEY,
    vm_id             bigint      NOT NULL,
    node_id           bigint      NOT NULL,
    "order"           integer     NOT NULL,
    is_primary        boolean     NOT NULL DEFAULT false,
    switch_id         bigint,
    security_group_id bigint,
    model             varchar(16) NOT NULL DEFAULT 'virtio',
    mac               varchar(32),
    allowed_addresses text,
    rate_limit_mbps   integer     NOT NULL DEFAULT 0,
    last_applied_at   timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_interface_vm_order ON vm_interface (vm_id, "order");
CREATE INDEX IF NOT EXISTS idx_vm_interface_switch_id ON vm_interface (switch_id);

CREATE TABLE IF NOT EXISTS static_ip (
    id                 bigserial   PRIMARY KEY,
    node_id            bigint      NOT NULL,
    vm_id              bigint,
    interface_order    integer,
    ip                 varchar(64) NOT NULL,
    mac                varchar(32),
    address_family     varchar(8)  NOT NULL DEFAULT 'ipv4',
    is_dhcp_reservation boolean    NOT NULL DEFAULT false,
    applied_at         timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_static_ip_node_ip ON static_ip (node_id, ip);
CREATE INDEX IF NOT EXISTS idx_static_ip_vm_id ON static_ip (vm_id);

CREATE TABLE IF NOT EXISTS port_forward (
    id              bigserial   PRIMARY KEY,
    node_id         bigint      NOT NULL,
    vm_id           bigint,
    protocol        varchar(8)  NOT NULL DEFAULT 'tcp',
    host_port       integer     NOT NULL,
    target_ip       varchar(64),
    target_port     integer     NOT NULL,
    static_ip_id    bigint,
    allowed_ips     text,
    allowed_regions varchar(255),
    enabled         boolean     NOT NULL DEFAULT true,
    last_applied_at timestamptz,
    created_by      bigint,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_port_forward_node_proto_port ON port_forward (node_id, protocol, host_port);
CREATE INDEX IF NOT EXISTS idx_port_forward_vm_id ON port_forward (vm_id);

CREATE TABLE IF NOT EXISTS public_ip (
    id              bigserial    PRIMARY KEY,
    node_id         bigint       NOT NULL,
    ip              varchar(64)  NOT NULL,
    cidr            varchar(64),
    gateway         varchar(64),
    egress_if       varchar(64),
    address_family  varchar(8)   NOT NULL DEFAULT 'ipv4',
    supported_modes varchar(128),
    status          varchar(16)  NOT NULL DEFAULT 'available',
    remark          varchar(255),
    created_at      timestamptz  NOT NULL DEFAULT now(),
    updated_at      timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_public_ip_node_ip ON public_ip (node_id, ip);
CREATE INDEX IF NOT EXISTS idx_public_ip_node_status ON public_ip (node_id, status);

CREATE TABLE IF NOT EXISTS public_ip_binding (
    id             bigserial   PRIMARY KEY,
    public_ip_id   bigint      NOT NULL,
    node_id        bigint      NOT NULL,
    vm_id          bigint,
    mode           varchar(24) NOT NULL,
    runtime_status varchar(16) NOT NULL DEFAULT 'pending',
    bound_at       timestamptz,
    released_at    timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_public_ip_binding_public_ip_id ON public_ip_binding (public_ip_id);
CREATE INDEX IF NOT EXISTS idx_public_ip_binding_vm_id ON public_ip_binding (vm_id);

CREATE TABLE IF NOT EXISTS port_mirror (
    id              bigserial   PRIMARY KEY,
    node_id         bigint      NOT NULL,
    name            varchar(64),
    source_ports    text,
    target_switches text,
    direction       varchar(8)  NOT NULL DEFAULT 'both',
    vlan_preserve   boolean     NOT NULL DEFAULT false,
    enabled         boolean     NOT NULL DEFAULT false,
    watchdog_until  timestamptz,
    last_applied_at timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_port_mirror_node_id ON port_mirror (node_id);

CREATE TABLE IF NOT EXISTS port_security_policy (
    id             bigserial    PRIMARY KEY,
    node_id        bigint       NOT NULL,
    port_ref       varchar(128) NOT NULL,
    switch_id      bigint,
    vm_id          bigint,
    spoofing_guard boolean      NOT NULL DEFAULT false,
    isolation      boolean      NOT NULL DEFAULT false,
    pps_limit      integer      NOT NULL DEFAULT 0,
    status         varchar(16)  NOT NULL DEFAULT 'pending',
    detail         text,
    applied_at     timestamptz,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_port_security_policy_node_port ON port_security_policy (node_id, port_ref);

CREATE TABLE IF NOT EXISTS firewall_policy (
    id              bigserial   PRIMARY KEY,
    node_id         bigint      NOT NULL,
    enabled         boolean     NOT NULL DEFAULT false,
    default_action  varchar(8)  NOT NULL DEFAULT 'deny',
    geoip_regions   varchar(512),
    whitelist       text,
    version         integer     NOT NULL DEFAULT 0,
    applied_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_firewall_policy_node_id ON firewall_policy (node_id);

CREATE TABLE IF NOT EXISTS firewall_vm_policy (
    id            bigserial   PRIMARY KEY,
    vm_id         bigint      NOT NULL,
    policy_id     bigint,
    action        varchar(8)  NOT NULL DEFAULT 'accept',
    geoip_regions varchar(512),
    whitelist     text,
    enabled       boolean     NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_firewall_vm_policy_vm_id ON firewall_vm_policy (vm_id);

CREATE TABLE IF NOT EXISTS firewall_rule (
    id            bigserial   PRIMARY KEY,
    node_id       bigint      NOT NULL,
    action        varchar(8)  NOT NULL,
    protocol      varchar(8)  NOT NULL DEFAULT 'tcp',
    port_start    integer,
    port_end      integer,
    source_cidr   varchar(64),
    is_protected  boolean     NOT NULL DEFAULT false,
    order_no      integer     NOT NULL DEFAULT 100,
    applied       boolean     NOT NULL DEFAULT false,
    remark        varchar(255),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
-- 去重键：NULL 参与比较时用 coalesce 归一，避免"同一条规则重复插入"
CREATE UNIQUE INDEX IF NOT EXISTS uniq_firewall_rule_dedup ON firewall_rule (
    node_id, action, protocol,
    coalesce(port_start, 0), coalesce(port_end, 0), coalesce(source_cidr, '')
);
CREATE INDEX IF NOT EXISTS idx_firewall_rule_node_protected ON firewall_rule (node_id, is_protected);

CREATE TABLE IF NOT EXISTS network_capture (
    id           bigserial    PRIMARY KEY,
    node_id      bigint       NOT NULL,
    vm_id        bigint,
    interface    varchar(64),
    filter       varchar(255),
    duration_sec integer      NOT NULL DEFAULT 0,
    file_path    varchar(512),
    size_bytes   bigint       NOT NULL DEFAULT 0,
    task_id      bigint,
    created_by   bigint,
    expires_at   timestamptz,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_network_capture_node_id ON network_capture (node_id);
CREATE INDEX IF NOT EXISTS idx_network_capture_task_id ON network_capture (task_id);

-- 流量日统计：以 scope_type + scope_id 统一"用户 / 交换机 / 虚拟机"三个口径
CREATE TABLE IF NOT EXISTS traffic_stat_daily (
    id         bigserial   PRIMARY KEY,
    scope_type varchar(8)  NOT NULL,
    scope_id   bigint      NOT NULL,
    owner_id   bigint,
    date       date        NOT NULL,
    bytes_in   bigint      NOT NULL DEFAULT 0,
    bytes_out  bigint      NOT NULL DEFAULT 0,
    limited_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_traffic_stat_scope_date ON traffic_stat_daily (scope_type, scope_id, date);
CREATE INDEX IF NOT EXISTS idx_traffic_stat_owner_date ON traffic_stat_daily (owner_id, date);


-- ============================================================
-- 五、虚拟机（7）
-- ============================================================

-- status / vcpu / memory_mb / disk_gb / ip_summary 均为投影字段，以虚拟化层为准
CREATE TABLE IF NOT EXISTS vm (
    id             bigserial    PRIMARY KEY,
    node_id        bigint       NOT NULL,
    name           varchar(63)  NOT NULL,
    uuid           varchar(64),
    owner_id       bigint,
    template_id    bigint,
    status         varchar(16)  NOT NULL DEFAULT 'unknown',
    vcpu           integer      NOT NULL DEFAULT 0,
    memory_mb      integer      NOT NULL DEFAULT 0,
    disk_gb        integer      NOT NULL DEFAULT 0,
    ip_summary     varchar(255),
    remark         varchar(200),
    group_name     varchar(64),
    present        boolean      NOT NULL DEFAULT true,
    last_synced_at timestamptz,
    created_at     timestamptz  NOT NULL DEFAULT now(),
    updated_at     timestamptz  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_node_name ON vm (node_id, name);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_node_uuid ON vm (node_id, uuid);
CREATE INDEX IF NOT EXISTS idx_vm_owner_id ON vm (owner_id);
CREATE INDEX IF NOT EXISTS idx_vm_status ON vm (status);
CREATE INDEX IF NOT EXISTS idx_vm_group_name ON vm (group_name);

CREATE TABLE IF NOT EXISTS vm_tag (
    id         bigserial   PRIMARY KEY,
    vm_id      bigint      NOT NULL,
    tag        varchar(32) NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_tag_vm_tag ON vm_tag (vm_id, tag);
CREATE INDEX IF NOT EXISTS idx_vm_tag_tag ON vm_tag (tag);

CREATE TABLE IF NOT EXISTS vm_credential (
    id           bigserial   PRIMARY KEY,
    vm_id        bigint      NOT NULL,
    username     varchar(64),
    password_enc text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_credential_vm_id ON vm_credential (vm_id);

CREATE TABLE IF NOT EXISTS vm_lock (
    id         bigserial   PRIMARY KEY,
    vm_id      bigint      NOT NULL,
    locked     boolean     NOT NULL DEFAULT false,
    locked_by  bigint,
    reason     varchar(255),
    locked_at  timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_lock_vm_id ON vm_lock (vm_id);

CREATE TABLE IF NOT EXISTS vm_schedule (
    id           bigserial   PRIMARY KEY,
    vm_id        bigint      NOT NULL,
    node_id      bigint      NOT NULL,
    action       varchar(16) NOT NULL,
    schedule_type varchar(16) NOT NULL,
    weekdays     varchar(32),
    run_at       time,
    next_run_at  timestamptz,
    last_run_at  timestamptz,
    last_result  varchar(32),
    last_task_id bigint,
    enabled      boolean     NOT NULL DEFAULT true,
    created_by   bigint,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_vm_schedule_next_run ON vm_schedule (next_run_at, enabled);
CREATE INDEX IF NOT EXISTS idx_vm_schedule_vm_id ON vm_schedule (vm_id);

CREATE TABLE IF NOT EXISTS vm_stats_record (
    id               bigserial      PRIMARY KEY,
    vm_id            bigint         NOT NULL,
    node_id          bigint         NOT NULL,
    at               timestamptz    NOT NULL,
    cpu_percent      double precision NOT NULL DEFAULT 0,
    mem_used_mb      bigint         NOT NULL DEFAULT 0,
    mem_percent      double precision NOT NULL DEFAULT 0,
    net_in_bytes     bigint         NOT NULL DEFAULT 0,
    net_out_bytes    bigint         NOT NULL DEFAULT 0,
    disk_read_bytes  bigint         NOT NULL DEFAULT 0,
    disk_write_bytes bigint         NOT NULL DEFAULT 0,
    disk_iops        integer        NOT NULL DEFAULT 0,
    uptime_seconds   bigint         NOT NULL DEFAULT 0,
    created_at       timestamptz    NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_vm_stats_vm_at ON vm_stats_record (vm_id, at);
CREATE INDEX IF NOT EXISTS idx_vm_stats_node_at ON vm_stats_record (node_id, at);

CREATE TABLE IF NOT EXISTS vm_runtime_daily (
    id         bigserial   PRIMARY KEY,
    vm_id      bigint      NOT NULL,
    node_id    bigint      NOT NULL,
    owner_id   bigint,
    date       date        NOT NULL,
    seconds    integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_vm_runtime_vm_date ON vm_runtime_daily (vm_id, date);
CREATE INDEX IF NOT EXISTS idx_vm_runtime_owner_date ON vm_runtime_daily (owner_id, date);


-- ============================================================
-- 六、模板（1）
-- ============================================================

CREATE TABLE IF NOT EXISTS template (
    id                bigserial    PRIMARY KEY,
    node_id           bigint       NOT NULL,
    name              varchar(64)  NOT NULL,
    parent_id         bigint,
    version           integer      NOT NULL DEFAULT 1,
    status            varchar(16)  NOT NULL DEFAULT 'preparing',
    storage_pool_id   bigint,
    disk_path         varchar(512),
    disk_format       varchar(16)  NOT NULL DEFAULT 'qcow2',
    disk_size_gb      integer      NOT NULL DEFAULT 0,
    os_type           varchar(64),
    os_variant        varchar(64),
    min_disk_gb       integer      NOT NULL DEFAULT 0,
    default_cpu       integer      NOT NULL DEFAULT 0,
    default_memory_mb integer      NOT NULL DEFAULT 0,
    default_spec      text,
    published         boolean      NOT NULL DEFAULT false,
    visibility        varchar(16)  NOT NULL DEFAULT 'private',
    clone_enabled     boolean      NOT NULL DEFAULT true,
    immutable         boolean      NOT NULL DEFAULT false,
    prepare_mode      varchar(16),
    error             varchar(255),
    created_by        bigint,
    remark            varchar(255),
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),
    deleted_at        timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS uniq_template_node_name ON template (node_id, name);
CREATE INDEX IF NOT EXISTS idx_template_parent_id ON template (parent_id);
CREATE INDEX IF NOT EXISTS idx_template_node_published ON template (node_id, published);


-- ============================================================
-- 七、任务与调度（3）
-- ============================================================

CREATE TABLE IF NOT EXISTS task (
    id               bigserial    PRIMARY KEY,
    type             varchar(48)  NOT NULL,
    status           varchar(16)  NOT NULL DEFAULT 'pending',
    node_id          bigint,
    resource_type    varchar(32),
    resource_id      bigint,
    resource_name    varchar(128),
    owner_id         bigint,
    params           text,
    progress         integer      NOT NULL DEFAULT 0,
    current_stage    varchar(64),
    result           text,
    error            varchar(512),
    cancel_requested boolean      NOT NULL DEFAULT false,
    created_by       bigint,
    started_at       timestamptz,
    finished_at      timestamptz,
    created_at       timestamptz  NOT NULL DEFAULT now(),
    updated_at       timestamptz  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_status_created ON task (status, created_at);
CREATE INDEX IF NOT EXISTS idx_task_node_id ON task (node_id);
CREATE INDEX IF NOT EXISTS idx_task_resource ON task (resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_task_type ON task (type);
CREATE INDEX IF NOT EXISTS idx_task_owner_id ON task (owner_id);

CREATE TABLE IF NOT EXISTS task_stage (
    id               bigserial    PRIMARY KEY,
    task_id          bigint       NOT NULL,
    seq              integer      NOT NULL,
    key              varchar(64)  NOT NULL,
    name             varchar(64),
    status           varchar(16)  NOT NULL DEFAULT 'pending',
    message          varchar(512),
    retryable        boolean      NOT NULL DEFAULT false,
    retry_of_stage_id bigint,
    started_at       timestamptz,
    finished_at      timestamptz
);
CREATE INDEX IF NOT EXISTS idx_task_stage_task_seq ON task_stage (task_id, seq);

CREATE TABLE IF NOT EXISTS scheduler_event (
    id             bigserial    PRIMARY KEY,
    scheduler_key  varchar(64)  NOT NULL,
    scheduler_name varchar(64),
    group_name     varchar(64),
    node_id        bigint,
    scope          varchar(128),
    status         varchar(16)  NOT NULL,
    message        varchar(512),
    at             timestamptz  NOT NULL DEFAULT now(),
    created_at     timestamptz  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_scheduler_event_key ON scheduler_event (scheduler_key);
CREATE INDEX IF NOT EXISTS idx_scheduler_event_created_at ON scheduler_event (at);


-- ============================================================
-- 八、监控（1）
-- ============================================================

-- 设备级明细（每网卡 / 每磁盘）以 JSON 内嵌 device_stats，不单独建表
CREATE TABLE IF NOT EXISTS host_stats_record (
    id               bigserial       PRIMARY KEY,
    node_id          bigint          NOT NULL,
    at               timestamptz     NOT NULL,
    cpu_percent      double precision NOT NULL DEFAULT 0,
    mem_used_mb      bigint          NOT NULL DEFAULT 0,
    mem_total_mb     bigint          NOT NULL DEFAULT 0,
    swap_used_mb     bigint          NOT NULL DEFAULT 0,
    load1            double precision NOT NULL DEFAULT 0,
    load5            double precision NOT NULL DEFAULT 0,
    load15           double precision NOT NULL DEFAULT 0,
    net_in_bytes     bigint          NOT NULL DEFAULT 0,
    net_out_bytes    bigint          NOT NULL DEFAULT 0,
    disk_read_bytes  bigint          NOT NULL DEFAULT 0,
    disk_write_bytes bigint          NOT NULL DEFAULT 0,
    device_stats     text,
    uptime_seconds   bigint          NOT NULL DEFAULT 0,
    created_at       timestamptz     NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_host_stats_node_at ON host_stats_record (node_id, at);


-- ============================================================
-- 九、迁移登记（1）
-- ============================================================

CREATE TABLE IF NOT EXISTS schema_migration (
    migration_id varchar(128) PRIMARY KEY,
    checksum     varchar(64),
    note         varchar(255),
    applied_at   timestamptz  NOT NULL DEFAULT now()
);
