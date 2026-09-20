package vm

import (
	"context"
	"errors"
	"log"
	"strconv"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// describeErr 取业务错误的面向用户文案。
//
// 前置条件要展示给普通用户，而内部错误里可能带着 SQL 与节点地址——原样
// 回显既读不懂也可能泄漏。因此只取 *api.Error 的 Message，其余统一兜底。
func describeErr(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return "检查未通过"
}

// 创建向导的步骤（FRONTEND §5.3.2）。
//
// 与编辑页的子选项卡**共用分组标识与字段矩阵**，只是顺序与名称不同：
// 创建是「从无到有」的顺序（先定规格，再定系统），编辑是「按主题归类」。
// 字段本身只有一份（editFields），新增一项时两处同时生效。
var createGroups = []EditGroupInfo{
	{Key: EditGroupBasic, Label: "基础信息"},
	{Key: EditGroupHardware, Label: "硬件规格"},
	{Key: EditGroupDisk, Label: "存储介质"},
	{Key: EditGroupNetwork, Label: "网络设置"},
	{Key: EditGroupBoot, Label: "系统配置"},
	{Key: EditGroupAdvanced, Label: "高级选项"},
	{
		Key: EditGroupPassthru, Label: "硬件直通", Planned: true,
		Note: "PCI 直通需要设备表与宿主机的 IOMMU 分组信息（F-2-06），尚未建模。",
	},
}

// 前置条件的标识。
const (
	PreNode    = "node"
	PreStorage = "storage_pool"
	PreNetwork = "network"
	PreQuota   = "quota"
)

// Prerequisite 是一项创建前置条件及其检查结果。
//
// **逐项返回而不是只给一个布尔**：向导要在用户填了八步之后才告诉他
// 「这个节点没有存储池」是最糟糕的时机。状态与修复指引一起下发，界面
// 就能在第一步就把话说清楚（f-2-02 R-001）。
type Prerequisite struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	OK    bool   `json:"ok"`
	// Message 是不通过时的**可执行**提示。
	Message string `json:"message,omitempty"`
	// Link 是面板内的修复入口。
	Link string `json:"link,omitempty"`
}

// ISOOption 是一个可挂载的安装镜像。
type ISOOption struct {
	ID        int64  `json:"id"`
	Filename  string `json:"filename"`
	SizeBytes int64  `json:"size_bytes"`
	// OSType / OsVariant / MinDiskGB 来自识别结果（f-5-05），供向导自动
	// 补全系统类型与最小磁盘——它们的意思不是「必须照做」，而是「给个
	// 不用查文档的起点」。
	OSType    string `json:"os_type,omitempty"`
	OsVariant string `json:"os_variant,omitempty"`
	MinDiskGB int    `json:"min_disk_gb"`
}

// SwitchOption 是一个可接入的虚拟交换机。
type SwitchOption struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	CIDR     string `json:"cidr,omitempty"`
	IsSystem bool   `json:"is_system"`
}

// SecurityGroupOption 是一个可挂载的安全组。
type SecurityGroupOption struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

// CreateForm 是创建向导的表单元数据。
type CreateForm struct {
	// Fields 是创建向导可见的配置项（含默认值与取值约束）。
	Fields []EditField `json:"fields"`
	// Values 是各项的初始值，来自矩阵里的 Default。
	Values map[string]any `json:"values"`
	// Groups 是向导步骤的顺序与名称。
	Groups []EditGroupInfo `json:"groups"`

	Prerequisites  []Prerequisite        `json:"prerequisites"`
	ISOFiles       []ISOOption           `json:"iso_files"`
	Switches       []SwitchOption        `json:"switches"`
	SecurityGroups []SecurityGroupOption `json:"security_groups"`
	// CanSubmit 报告前置条件是否全部满足。
	//
	// 前端据此禁用提交并高亮缺失项，但**它不是安全边界**：真正的校验
	// 在 Create 里还会做一遍（这里的结论可能因为资源随后被删除而过期）。
	CanSubmit bool `json:"can_submit"`
}

// CreateFormOf 返回创建向导的元数据（f-2-02 R-002：规则由后端下发）。
//
// 它按 node_id 取上下文：可选值（ISO、交换机、安全组）都是**节点内**资源，
// 换一个节点就是另一份清单。不做节点维度的话，用户能选到一台在目标宿主机
// 上根本不存在的镜像。
func (s *Service) CreateFormOf(ctx context.Context, nodeID int64, v authz.Viewer) (*CreateForm, error) {
	if nodeID <= 0 {
		return nil, api.InvalidParameter("必须指定节点")
	}

	form := &CreateForm{
		Fields:         createFields(),
		Values:         createValues(),
		Groups:         createGroups,
		ISOFiles:       []ISOOption{},
		Switches:       []SwitchOption{},
		SecurityGroups: []SecurityGroupOption{},
	}

	// --- 节点：离线节点上不允许入队（f-2-02 Q-008）---
	pre := Prerequisite{Key: PreNode, Label: "目标节点可用"}
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		pre.Message = describeErr(err)
		pre.Link = "/node"
	} else {
		pre.OK = true
	}
	form.Prerequisites = append(form.Prerequisites, pre)

	// --- 存储池：没有可用池就没有地方放磁盘 ---
	pre = Prerequisite{Key: PreStorage, Label: "存在可用存储池"}
	var pools int64
	if err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("node_id = ? AND status = ?", nodeID, model.StoragePoolReady).
		Count(&pools).Error; err != nil {
		log.Printf("[vm] 统计存储池失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	if pools > 0 {
		pre.OK = true
	} else {
		pre.Message = "该节点还没有可用的存储池，虚拟机磁盘无处存放"
		pre.Link = "/storage-pool"
	}
	form.Prerequisites = append(form.Prerequisites, pre)

	// --- 网络：至少有一个可接入的交换机 ---
	pre = Prerequisite{Key: PreNetwork, Label: "存在可接入的网络"}
	var switches []model.VpcSwitch
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).
		Order("is_system DESC, id").Find(&switches).Error; err != nil {
		log.Printf("[vm] 查询交换机失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	for i := range switches {
		sw := &switches[i]
		opt := SwitchOption{
			ID: sw.ID, Name: sw.Name, Mode: sw.Mode, IsSystem: sw.IsSystem,
		}
		if sw.CIDR != nil {
			opt.CIDR = *sw.CIDR
		}
		form.Switches = append(form.Switches, opt)
	}
	if len(switches) > 0 {
		pre.OK = true
	} else {
		pre.Message = "该节点上还没有虚拟交换机，创建出来的虚拟机将没有网络"
		pre.Link = "/network"
	}
	form.Prerequisites = append(form.Prerequisites, pre)

	// --- 配额：按默认系统盘试算，而不是等用户填完再拒 ---
	pre = Prerequisite{Key: PreQuota, Label: "存储配额充足"}
	if s.quota == nil {
		pre.OK = true
	} else {
		est := int64(defaultCreateDiskGB) * 1024 * 1024 * 1024
		if err := s.quota.Check(ctx, v.UserID, nodeID, est); err != nil {
			pre.Message = describeErr(err)
			pre.Link = "/quota"
		} else {
			pre.OK = true
		}
	}
	form.Prerequisites = append(form.Prerequisites, pre)

	// --- 可选值：ISO 与 安全组（都按节点与归属过滤）---
	var files []model.StorageFile
	err := s.db.WithContext(ctx).
		Where(`node_id = ? AND category = ? AND uploaded_at IS NOT NULL
			AND (user_id IS NULL OR user_id = ?)`, nodeID, model.FileCategoryISO, v.UserID).
		Order("filename").Find(&files).Error
	if err != nil {
		log.Printf("[vm] 查询镜像失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	for i := range files {
		f := &files[i]
		opt := ISOOption{ID: f.ID, Filename: f.Filename, SizeBytes: f.SizeBytes, MinDiskGB: f.MinDiskGB}
		if f.OsType != nil {
			opt.OSType = *f.OsType
		}
		if f.OsVariant != nil {
			opt.OsVariant = *f.OsVariant
		}
		form.ISOFiles = append(form.ISOFiles, opt)
	}

	var groups []model.SecurityGroup
	err = s.db.WithContext(ctx).Where("node_id = ?", nodeID).Order("is_default DESC, name").
		Find(&groups).Error
	if err != nil {
		log.Printf("[vm] 查询安全组失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	for i := range groups {
		g := &groups[i]
		if !v.IsAdmin && g.OwnerID != nil && *g.OwnerID != v.UserID {
			continue
		}
		form.SecurityGroups = append(form.SecurityGroups, SecurityGroupOption{
			ID: g.ID, Name: g.Name, IsDefault: g.IsDefault,
		})
	}

	form.CanSubmit = true
	for i := range form.Prerequisites {
		if !form.Prerequisites[i].OK {
			form.CanSubmit = false
			break
		}
	}
	return form, nil
}

// defaultCreateDiskGB 是前置条件试算配额时用的默认系统盘大小。
//
// 与矩阵里 disk_gb 的 Default 保持一致：两处不一致的话，界面上写着
// 「默认 40 GB」，而配额提示却说「按 20 GB 试算」，用户无法判断哪个真。
const defaultCreateDiskGB = 40

// createFields 返回创建向导可见的字段。
//
// 逐项拷贝而不是直接返回切片：editFields 是包级变量，把它的地址交给
// 调用方（最终被 JSON 序列化）没有任何问题，但直接返回会让「创建只用到
// 其中一部分」这件事无法在类型上表达——将来有人在编辑页改了顺序，
// 创建向导的字段顺序会跟着变，而那通常不是他想要的。
func createFields() []EditField {
	out := make([]EditField, 0, len(editFields))
	for _, f := range editFields {
		if f.InCreate {
			out = append(out, f)
		}
	}
	return out
}

// createValues 按矩阵里的 Default 生成初始值。
//
// 类型转换按 Kind 走而不是全都给字符串：number 项给 0 会让输入框显示
// 「0」，而 0 对磁盘与内存来说是非法值——用户看到的第一眼就是错的。
func createValues() map[string]any {
	out := make(map[string]any, len(editFields))
	for _, f := range editFields {
		if !f.InCreate {
			continue
		}
		switch f.Kind {
		case EditKindNumber:
			n, _ := strconv.Atoi(f.Default)
			out[f.Key] = n
		case EditKindBoolean:
			out[f.Key] = f.Default == "true"
		default:
			if f.Default != "" {
				out[f.Key] = f.Default
			}
		}
	}
	return out
}

// ensureCreatePrerequisites 在**入队前**复核前置条件（f-2-02 R-001 / R-014）。
//
// 它与 CreateFormOf 的检查重复，但重复是必要的：表单元数据是打开向导时
// 取的，到提交之间可能过了很久，期间存储池可能被删、节点可能离线。
// 只依赖前一次检查等于把校验结果缓存起来当事实。
func (s *Service) ensureCreatePrerequisites(ctx context.Context, nodeID int64) error {
	if err := s.ensureNodeUsable(ctx, nodeID); err != nil {
		return err
	}
	var pools int64
	if err := s.db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("node_id = ? AND status = ?", nodeID, model.StoragePoolReady).
		Count(&pools).Error; err != nil {
		log.Printf("[vm] 复核存储池失败 node=%d: %v", nodeID, err)
		return api.Internal()
	}
	if pools == 0 {
		return api.ValidationFailed("该节点没有可用存储池，请先创建存储池")
	}
	return nil
}
