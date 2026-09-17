-- 磁盘与镜像导入（F-2-13）。
--
-- 导入的产物是**一个模板**，不是一台虚拟机。这个选择值得写清楚：
-- 导入一份别人的镜像之后，用户接下来多半是「用它开几台机」，而不是「就要
-- 这一台」。产出模板把人带到已有的克隆流程（f-3-02）上，也顺便获得了
-- 模板那一整套可见性、发布与依赖管理——从导入直接建一台机的话，那份镜像
-- 就成了一次性的东西，想再开一台还得重导一次。
--
-- source_* 字段记录**来源**：文件名、格式、大小。格式是转换前的原始格式
-- （qcow2 / raw / vmdk / vhd / vhdx / img / ova），导入过程中会被统一转成
-- qcow2——但原始格式要留档，因为「导进来的盘当初是什么格式」在排查
-- 转换相关问题时是第一条线索。
--
-- status 与 task.status 用同一套词汇（pending/running/success/failed），
-- 避免界面上出现两种说法指着同一件事。
CREATE TABLE IF NOT EXISTS image_import (
    id                  bigserial    PRIMARY KEY,
    node_id             bigint       NOT NULL,
    name                varchar(64)  NOT NULL,
    source_filename     varchar(255) NOT NULL,
    source_format       varchar(16)  NOT NULL DEFAULT 'qcow2',
    source_size_bytes   bigint       NOT NULL DEFAULT 0,
    status              varchar(16)  NOT NULL DEFAULT 'pending',
    -- 转换后的磁盘落在哪、以及由此产出的模板。产物模板为空表示还没走完。
    disk_path           varchar(512),
    template_id         bigint,
    -- 导入是「先解析预览、再创建」（f-2-13），预览到的配置留存下来：
    -- 用户按下确认时看到的东西，与真正被创建的东西应当是同一份。
    preview             text,
    error               varchar(512),
    created_by          bigint,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    deleted_at          timestamptz
);

CREATE INDEX IF NOT EXISTS idx_image_import_node_id ON image_import (node_id);
