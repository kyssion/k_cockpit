package agent

// OpImageImport 导入已有磁盘或 OVA 包（F-2-13）。
//
// 导入产出的是一块**已注册到节点的系统盘**，由控制面据此建立模板。节点
// 负责的只是「把这个文件变成一块能用的盘」：格式转换、完整性校验、放到
// 存储池里。
const OpImageImport OpKind = "image.import"

// OpImageParse 解析 OVA 包里的机器描述（OVF），供用户预览。
//
// 与 OpImageImport 分开是因为它**必须发生在受理之前**：f-2-13 要求
// 「先解析预览再创建」，而预览的意义就是让用户在按下确认之前看到将要创建的
// 是什么。并进导入操作的话，预览就只能出现在事后。
const OpImageParse OpKind = "image.parse"

// ImageParseDataKey 是解析结果中承载预览信息的键。
const ImageParseDataKey = "image_preview"

// ImageImportDataKey 是导入结果中承载产物信息的键。
const ImageImportDataKey = "image_import"

// ImageImportInfo 是节点在导入完成后回传的信息。
type ImageImportInfo struct {
	// DiskPath 是转换后磁盘在宿主机上的路径。
	DiskPath string
	// SizeGB 是导入后的实际磁盘大小。
	SizeGB int
	// Format 是转换后的格式，正常情况是 qcow2。
	Format string
	// Notes 是转换过程中的提示，例如「源格式为 vmdk，已转换」。
	Notes []string
}

// ImagePreview 是节点从打包格式（OVA）里解析出的机器描述。
//
// 本包**不引用 model**（协议层与存储层解耦，见 mock.go 的说明），因此这里
// 自己声明一份。控制面在受理时把它转成 model.ImportPreview——两份结构相同
// 但归属不同：一份是「节点说了什么」，一份是「控制面决定存什么」。
type ImagePreview struct {
	VCPU     int    `json:"vcpu"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
	OSType   string `json:"os_type"`
	// OSVariant 是更具体的系统标识（如 ubuntu24.04）。
	OSVariant string `json:"os_variant"`
	// Sources 说明每个字段的来源，供界面标注「这项是从包里读出来的」。
	Sources map[string]string `json:"sources"`
}
