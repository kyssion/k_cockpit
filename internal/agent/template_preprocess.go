package agent

// OpTemplatePreprocess 对模板做离线预处理。
//
// 它必须由节点执行：预处理要把镜像挂进一个临时环境再改里面的内容（装驱动、
// 注入 SSH 公钥、清 machine-id、移除 cloud-init 残留）。
// 控制面既不挂载镜像，也没有 virt-customize 一类工具链，因此这里只下发"要做哪几项"，
// 由节点决定怎么做。
const OpTemplatePreprocess OpKind = "template.preprocess"

// TemplatePreprocessDataKey 是预处理结果的键。
const TemplatePreprocessDataKey = "template_preprocess"

// TemplatePreprocessInfo 是预处理的结果。
type TemplatePreprocessInfo struct {
	// Steps 是实际执行的步骤名，界面按它展示"这次动了什么"。
	Steps []string
	// SizeDeltaBytes 是镜像体积变化（可能为负）。
	SizeDeltaBytes int64
	Message        string
	// Unavailable 非空表示节点缺少工具链——**一项都没做**。
	Unavailable string
	// Rollback 为 true 表示中途失败且已还原为原镜像。
	Rollback bool
}
