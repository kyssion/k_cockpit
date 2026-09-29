-- 模板导出产物（F-3-05）。
--
-- 模板包是**跨节点搬运模板**的载体：导出一台宿主机上的模板、在另一台上
-- 导入。没有它，模板就只能活在它被制备出来的那个节点上——而节点会下线、
-- 会迁移、会换盘。
--
-- 产物放在用户存储里（rel_path 相对存储根），而不是某个临时目录：
--
--   - 它能被下载、能被当作导入来源选中，两条路共用同一份文件；
--   - 它受用户存储配额约束——一个几十 GB 的导出包躲在配额之外，
--     迟早会以"磁盘满了但谁也没占着"的形式暴露出来。
--
-- 与 vm_export 分表：那张表的主语是虚拟机，这里的主语是模板。共用一张
-- 表就得让"导出的是谁"变成一列可空外键，而漏写一个条件的代价是
-- "虚拟机导出列表里混进了模板包"。
CREATE TABLE IF NOT EXISTS template_export (
    id            bigserial    PRIMARY KEY,
    node_id       bigint       NOT NULL,
    template_id   bigint       NOT NULL,
    template_name varchar(64)  NOT NULL,

    rel_path      varchar(512) NOT NULL,
    filename      varchar(255) NOT NULL,
    size_bytes    bigint       NOT NULL DEFAULT 0,

    status        varchar(16)  NOT NULL DEFAULT 'pending',
    error         varchar(255),

    created_by    bigint,
    created_at    timestamptz  NOT NULL DEFAULT now(),
    finished_at   timestamptz
);

CREATE INDEX IF NOT EXISTS idx_template_export_node
    ON template_export (node_id);
CREATE INDEX IF NOT EXISTS idx_template_export_template
    ON template_export (template_id);
