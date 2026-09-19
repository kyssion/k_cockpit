package vm

import (
	"context"
	"regexp"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
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
