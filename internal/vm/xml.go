package vm

import (
	"context"
	"regexp"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
)

// XMLView 是虚拟机定义的对外视图。
type XMLView struct {
	VMID int64 `json:"vm_id"`
	// Protocol 说明这份定义是为哪种控制台取的。
	Protocol string `json:"protocol"`
	// Live 为 true 表示这是**运行中**的定义。
	//
	// 它与持久定义可能不同：热插拔一块磁盘之后，运行中的定义有它、而持久
	// 定义没有——重启之后那块盘就消失了。两份都要能看，而"看的是哪一份"
	// 必须标出来，否则用户会拿着运行时的定义去判断"重启后还在不在"。
	Live bool   `json:"live"`
	XML  string `json:"xml"`
	// Redacted 列出被替换掉的字段名。
	//
	// **必须列出来**：不说的话，用户看到一个 `passwd='[已脱敏]'` 会以为
	// 面板把它读错了——而真相是它本来就在那里、我们只是没给他看。
	Redacted []string `json:"redacted,omitempty"`
}

// XML 返回虚拟机的 libvirt 定义。
//
// 只读，且**不缓存**：它是排查"面板显示的和实际跑的不是一回事"时的唯一真相，
// 而缓存会让它变成"上次读到的真相"——正好掩盖要排查的那类变化。
func (s *Service) XML(
	ctx context.Context, id int64, protocol string, live bool, v authz.Viewer,
) (*XMLView, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	protocol = normalizeProtocol(protocol)
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMXML,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{"live": live, "protocol": protocol},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，无法读取虚拟机定义")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}
	info, _ := result.Data[agent.VMXMLKey].(agent.VMXMLInfo)

	xml, redacted := redactXML(info.XML)
	return &XMLView{
		VMID: vm.ID, Protocol: protocol,
		Live: info.Live, XML: xml, Redacted: redacted,
	}, nil
}

// redactXML 替换定义里的敏感值，并返回被替换的字段名。
//
// **这是只读接口里最要紧的一步。** libvirt 的域定义里有密码，而且不止一处：
//
//	<graphics type='vnc' passwd='...'/>           控制台密码
//	<spice ... passwd='...'/>                     SPICE 密码
//	<secret type='ceph' uuid='...'/>              磁盘加密密钥的引用
//	<disk><auth username='..'><secret .../></auth></disk>   存储认证
//	<source ... password='...'/>                  远端存储口令
//
// 原样返回等于**把控制台上那些"只写不读"的密码从后门送出去**——界面特意
// 不给看，而这个接口给了。
//
// 替换而不是抹掉整行：保留结构才能看出"这里有一个密码字段"，而那是排查时
// 需要的信息。全删掉的话，用户会以为面板把配置读丢了。
func redactXML(xml string) (string, []string) {
	if xml == "" {
		return xml, nil
	}
	seen := map[string]bool{}
	out := xml

	for _, rule := range xmlSecretAttrs {
		out = rule.re.ReplaceAllStringFunc(out, func(m string) string {
			seen[rule.name] = true
			// 保留属性名与引号，只换值。
			idx := strings.Index(m, "=")
			if idx < 0 {
				return redactPlaceholder
			}
			attr := m[:idx+1]
			quote := `"`
			if i := strings.Index(m, "'"); i >= 0 {
				quote = "'"
			}
			return attr + quote + redactPlaceholder + quote
		})
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sortStrings(names)
	return out, names
}

const redactPlaceholder = "[已脱敏]"

// xmlSecretAttrs 是要脱敏的属性。
//
// 按**属性名**匹配而不是按元素名：同一个 `passwd` 出现在 graphics、spice、
// source 等好几个元素上，而按元素名写会漏掉没想到的那一处。
//
// **只有这两个**，是刻意收敛的结果。`key=` 与 `secret=` 看起来也该脱敏，
// 但它们在 libvirt 定义里更常见的用法是**引用**而不是凭据：
//
//	<secret type='ceph' uuid='...'/>      密钥本身在 libvirt 的 secret store 里
//	                                                （不在 XML 中），uuid 只是一个句柄
//	<source ... key='...'>                有些存储后端用它当对象名，不是密码
//
// 把它们也脱掉会**误伤正常的配置**——而用户看到一处不该被遮的地方被遮了，
// 会开始怀疑这个开关到底遮了什么。与日志脱敏是同一条取舍：宁可精确，
// 不要覆盖面。
var xmlSecretAttrs = []struct {
	name string
	re   *regexp.Regexp
}{
	{"passwd", regexp.MustCompile(`passwd\s*=\s*('[^']*'|"[^"]*")`)},
	{"password", regexp.MustCompile(`password\s*=\s*('[^']*'|"[^"]*")`)},
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ExportRedactForTest 导出脱敏函数供测试使用。
//
// 脱敏是这一块里最需要被单独验证的部分：它的失败方式是**静默的**——日志
// 看起来正常、接口返回 200，而密码就在响应体里。因此它必须能被独立测。
func ExportRedactForTest(xml string) (string, []string) {
	return redactXML(xml)
}

// XMLPrecheck 是编辑前的预检结果。
type XMLPrecheck struct {
	// Diff 是这份改动的影响范围。
	//
	// **必须在保存之前给出**：别处用户改的是「字段」，而这里他改的是
	// **整份定义**——能动的范围没有边界。不给 diff 的话，他看到的是一大段
	// XML，而要判断的是"我这一改会动到什么"，那两件事对不上。
	Diff *XMLDiff `json:"diff"`
	// Valid 为 false 时 Errors 说明原因。
	Valid bool `json:"valid"`
	// Errors 是**节点**给出的校验问题，原样返回不改写——libvirt 的报错里
	// 有行号与元素名，改写之后那些信息往往就丢了，而用户正是靠它们定位。
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// PrecheckXML 校验一份新的域定义并给出 diff。**只读**，不应用。
//
// 两件事一起做是有意的：用户提交新定义之后，他要同时知道「我改了什么」与
// 「这份能不能用」。分两次请求会让他在看到 diff 之后还得再点一次才知道
// 行不行——而那时他已经做好了决定。
func (s *Service) PrecheckXML(
	ctx context.Context, id int64, newXML string, v authz.Viewer,
) (*XMLPrecheck, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(newXML) == "" {
		return nil, api.ValidationFailed("定义不能为空")
	}

	// 当前定义（持久那一份）：它是 diff 的基准。
	current, err := s.XML(ctx, id, "", false, v)
	if err != nil {
		return nil, err
	}

	out := &XMLPrecheck{Diff: diffXML(current.XML, newXML)}

	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMXMLApply,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{"action": "validate", "xml": newXML},
	})
	if err != nil {
		// 节点不可达时**不给"可以用"的结论**：让人以为能保存、点下去才失败，
		// 比直接说读不到要糟。
		return nil, api.Unavailable("节点不可达，无法校验这份定义")
	}
	if !result.Success {
		out.Valid = false
		out.Errors = []string{result.Message}
		return out, nil
	}
	if info, ok := result.Data[agent.VMXMLValidateKey].(agent.VMXMLValidateInfo); ok {
		out.Valid = info.Valid
		out.Errors = info.Errors
		out.Warnings = info.Warnings
	}
	return out, nil
}

// UpdateXML 应用一份新的域定义。
//
// **它绕过我们建立的其它全部校验**：同节点、配额、地址唯一性、端口安全的
// 前置条件——在 XML 里都可以被绕开。因此它必须经过二次验证（由 handler
// 强制），而审计里要记下**改动的规模**（加了几行、删了几行）：事后追查
// 「这条配置什么时候来的」时，那是最先要看的东西，而 XML 全文塞进审计表
// 会让那张表迅速膨胀。
func (s *Service) UpdateXML(
	ctx context.Context, id int64, newXML string,
	v authz.Viewer, operatorName, clientIP string,
) (*XMLPrecheck, error) {
	if err := s.ensureXMLVerified(ctx); err != nil {
		return nil, err
	}
	precheck, err := s.PrecheckXML(ctx, id, newXML, v)
	if err != nil {
		return nil, err
	}
	if !precheck.Valid {
		// 校验不过就不下发。**这是"改坏一台机器"的唯一防线**——在节点上
		// 先 validate 再 define，而 define 本身是原子的（失败不改动现有
		// 定义），因此不需要控制面自己写回滚。
		return precheck, api.ValidationFailed("定义未通过校验：" + strings.Join(precheck.Errors, "；"))
	}
	if precheck.Diff.Identical {
		// 没有变化就不下发：一次 define 会触发节点侧完整的重新定义，而
		// 那可能是几百毫秒的停机感知。界面也会据 Identical 禁用保存按钮，
		// 但接口这一层也要挡住——界面不是唯一的调用方。
		return precheck, nil
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	result, err := s.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMXMLApply,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{"action": "define", "xml": newXML},
	})
	if err != nil {
		return nil, api.Unavailable("节点不可达，定义未应用")
	}
	if !result.Success {
		return nil, api.ValidationFailed(result.Message)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.xml.update",
		Params: map[string]any{
			// **记规模而不是全文**：XML 全文塞进审计表会让它迅速膨胀，
			// 而"改了多少"已经足够回答"这条配置什么时候来的"。
			"added_lines":   precheck.Diff.Added,
			"removed_lines": precheck.Diff.Removed,
			"note":          "经由 XML 直编，绕过配额与唯一性校验",
		},
		Success: true, ClientIP: clientIP,
	})
	return precheck, nil
}

// ensureXMLVerified 检查本次调用是否已完成二次验证。
//
// 与 ensureExposureVerified 同一套做法：具体的许可校验在 handler 层完成
// （那里才有请求上下文），这里只做一次兜底——调用方没有标记「已验证」时
// 一律拒绝。这样即使将来有人新增了一个绕过 guard 的入口，也不会静默地让
// 一份任意定义落到节点上。
//
// **这个兜底在这里比在暴露那处更要紧**：XML 直编绕过的是我们建立的**全部**
// 校验（同节点、配额、地址唯一性、端口安全的前置条件），而不只是暴露一个端口。
func (s *Service) ensureXMLVerified(ctx context.Context) error {
	if verified := ctx.Value(ctxKeyXMLVerified); verified == true {
		return nil
	}
	return api.ValidationFailed("XML 直编需要先完成二次验证——它会绕过配额与唯一性校验")
}

// ctxKeyXMLVerified 是「已完成 XML 编辑验证」在 context 中的键。
type ctxKeyXMLVerifiedType struct{}

var ctxKeyXMLVerified = ctxKeyXMLVerifiedType{}

// WithXMLVerified 标记本次调用已完成 XML 编辑的二次验证。
func WithXMLVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyXMLVerified, true)
}
