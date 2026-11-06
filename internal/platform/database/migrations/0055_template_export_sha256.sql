-- 模板导出产物补内容摘要（第 3 轮 DEF-R3-01）。
--
-- 节点在打包完成后本来就回传了 SHA256（TemplateExportInfo.SHA256），但控制面
-- 既没有这一列、也没有在 API 里透出——用户拿不到摘要，就无法在导入前核对包
-- 是否被改动过，也没法把摘要抄给别人做完整性校验。节点侧的校验逻辑不变，
-- 这里只是把已有的值存下来并透出去。
ALTER TABLE template_export ADD COLUMN IF NOT EXISTS sha256 varchar(64);
