package vm

import (
	"net"
	"strconv"
	"strings"

	"k_cockpit/internal/api"
)

// 创建向导里几个**非矩阵**字段的校验。
//
// 它们不进 editFields 的原因各不相同，但都不适合用「键 → 值」的矩阵表达：
//   - 数据盘是一组结构且数量不定，塞进矩阵会得到 data_disk_1_size 这种把
//     上限写死在键名里的设计；
//   - 主机名的规则（RFC 1123）不是取值范围能表达的；
//   - 静态地址需要按 IP 解析，矩阵只有 Min/Max 与枚举。

// maxDataDisks 是一次创建允许附带的数据盘数量。
//
// 取 4 而不是更大：每块盘都是一次完整的镜像写入，盘越多宿主机存储被占满
// 的时间越长，表现为所有虚拟机变慢。真正需要很多盘的机器应当在建好之后
// 逐块挂，那还能按用途命名与单独限速。
const maxDataDisks = 4

// validateCreateExtras 校验数据盘、主机名与静态地址。
func validateCreateExtras(req CreateRequest) error {
	if len(req.DataDisks) > maxDataDisks {
		return api.InvalidParameter(
			"一次最多附带 " + strconv.Itoa(maxDataDisks) + " 块数据盘")

	}
	for i := range req.DataDisks {
		d := &req.DataDisks[i]
		if d.SizeGB <= 0 {
			return api.InvalidParameter("数据盘大小必须为正数")
		}
		if d.SizeGB > maxDiskGB {
			return api.InvalidParameter("单块数据盘不能超过 " + strconv.Itoa(maxDiskGB) + " GB")
		}
		if d.Format != "" && !allowedValue("disk_format", d.Format) {
			return api.InvalidParameter("数据盘格式非法：" + d.Format)
		}
		if d.Bus != "" && !allowedValue("disk_bus", d.Bus) {
			return api.InvalidParameter("数据盘驱动非法：" + d.Bus)
		}
	}

	if h := strings.TrimSpace(req.Hostname); h != "" {
		if err := validateHostname(h); err != nil {
			return err
		}
	}
	if ip := strings.TrimSpace(req.StaticIP); ip != "" {
		if net.ParseIP(ip) == nil {
			return api.InvalidParameter("静态地址不是合法的 IP：" + ip)
		}
	}
	// 初始密码只做最基本的长度与字符限制：它会被写进来宾凭据并加密保存，
	// 强度由使用者自己负责——拦住"带换行的密码"就够了（那会破坏注入脚本）。
	if p := req.InitialPassword; p != "" {
		if len(p) < 6 || len(p) > 64 {
			return api.InvalidParameter("初始密码长度需在 6 到 64 之间")
		}
		if strings.ContainsAny(p, "\n\r") {
			return api.InvalidParameter("初始密码不能包含换行")
		}
	}

	// CPU 拓扑：要填就填全，且乘积必须等于 vCPU。
	//
	// 只填一部分是没有意义的——来宾里看到的核数会与配额对不上，而这种不一致
	// 要等到装完系统才能发现。
	if req.CPUSockets > 0 || req.CPUCores > 0 || req.CPUThreads > 0 {
		if req.CPUSockets <= 0 || req.CPUCores <= 0 || req.CPUThreads <= 0 {
			return api.InvalidParameter("CPU 拓扑要填就填全：路数 / 每路核数 / 每核线程数")
		}
		if req.CPUSockets*req.CPUCores*req.CPUThreads != req.VCPU {
			return api.InvalidParameter(
				"CPU 拓扑之积（" + strconv.Itoa(req.CPUSockets) + "×" + strconv.Itoa(req.CPUCores) + "×" +
					strconv.Itoa(req.CPUThreads) + "）必须等于 vCPU 数量 " + strconv.Itoa(req.VCPU))
		}
	}
	if req.NICCount < 0 || req.NICCount > defaultInterfaceLimit {
		return api.InvalidParameter(
			"网口数量需在 0 到 " + strconv.Itoa(defaultInterfaceLimit) + " 之间")
	}
	return nil
}

// maxDiskGB 是单块磁盘的上限，与矩阵里 disk_gb 的 Max 保持一致。
const maxDiskGB = 2000

// allowedValue 报告取值是否在矩阵的可选值里。
//
// 复用矩阵而不是另写一份候选：数据盘的格式与驱动与系统盘是**同一套可选值**，
// 抄一份之后，某天新增一种格式时数据盘那边会静默拒绝它。
func allowedValue(key, value string) bool {
	f, ok := editFieldByKey(key)
	if !ok {
		return false
	}
	for i := range f.Options {
		if f.Options[i].Value == value {
			return true
		}
	}
	return false
}

// validateHostname 按 RFC 1123 校验主机名。
//
// 不校验而直接下发的后果是节点的初始化脚本拿到一个含空格或下划线的名字，
// 写进 /etc/hostname 后系统起不来——而报错在好几步之后才出现。
func validateHostname(name string) error {
	if len(name) > 63 {
		return api.InvalidParameter("主机名不能超过 63 个字符")
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(name)-1:
		default:
			return api.InvalidParameter("主机名只能包含字母、数字与连字符，且不能以连字符开头或结尾")
		}
	}
	return nil
}

// initialUsername 给出初始凭据的默认用户名。
//
// 按系统大类推断（Windows 用 Administrator，其余用 root）。它只是一个
// **起点**：真实的用户名取决于镜像，用户随后可以在详情页改。
func initialUsername(osType string) string {
	if osType == "windows" {
		return "Administrator"
	}
	return "root"
}

// credentialPurpose 标记这条凭据是创建时注入的初始密码。
//
// 与控制台密码（model.CredentialVNC）分开：它们的用途与展示位置都不同，
// 混在同一条里会让"重置登录密码"作用到一个不明确的用户上。
const credentialPurpose = "initial"
