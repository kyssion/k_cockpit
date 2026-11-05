package sys

import (
	"context"
	"log"
	"strings"
)

// SPICESupported 探测本机 libvirt 是否把 SPICE 列为可用的 graphics 类型。
//
// **保守启发式，不解析 XML**：仓库里没有 virsh domcapabilities 的解析器或
// 真机输出样例，假设任何字段名都会在某些发行版上失效。这里只检查原始输出
// 是否出现 spice 字面量；命令失败、无输出或输出格式不同一律判为不支持——
// 误判的方向必须落在「少给一个可选协议」，而不是「给出点了打不开的入口」。
//
// 真机验收需确认该启发式在实际 libvirt 版本上的结果（见 AGENT_ACCEPTANCE.md）。
func SPICESupported(ctx context.Context) bool {
	out, err := Run(ctx, "virsh", "domcapabilities")
	if err != nil {
		log.Printf("[sys] 探测 libvirt 控制台能力失败，按不支持 SPICE 处理: %v", err)
		return false
	}
	return spiceListed(out)
}

func spiceListed(out string) bool {
	return strings.Contains(out, "spice")
}
