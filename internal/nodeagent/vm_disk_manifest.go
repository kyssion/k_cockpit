// vm_disk_manifest.go：虚拟机磁盘的落盘清单（创建时记、删除时用）。
//
// 为什么需要这份清单：节点的删除路径原本只从域定义（`virsh dumpxml`）里
// 收集磁盘。僵尸机（域定义被外部 `virsh undefine` 掉，进程/文件仍在）恰恰
// 是「XML 里读不到磁盘」的形态——此时磁盘清单为空，删除任务报**成功**，
// 而 `/var/lib/k_cockpit/images/<name>.qcow2` 留在宿主机上（实机复现）。
// 控制面不持有磁盘清单（`GET /vms/:id/disks` 也是实时问节点），任务参数里
// 只有 `vm_name` 与 `disk_action`，因此"这台机器的盘在哪"只能由节点自己记住。
//
// 清单是**兜底信息**而不是创建的必要产物：写失败只记日志、不阻断创建——
// 一台创建成功但少了清单的机器仍然可用，只是删除时会退到第 2 层兜底。
package nodeagent

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// vmMetaDir 是「虚拟机磁盘清单」的落盘目录。
//
// 与 imagesDir / trashDir 同风格声明成变量：测试要指到临时目录
// （往真实的 /var/lib 下写在开发机上既不可写也不该留痕）。
var vmMetaDir = "/var/lib/k_cockpit/vmmeta"

// diskFormatExtensions 是系统盘/数据盘可能使用的文件扩展名（与创建路径一致：
// 默认 qcow2，允许 raw / img）。兜底删除只按这些**精确文件名**逐个尝试，
// 绝不使用通配符——见 dataDiskResidueFiles 的说明。
var diskFormatExtensions = []string{"qcow2", "raw", "img"}

// dataDiskDevicePattern 匹配数据盘文件名 `<vm>-<dev>.<ext>` 里的 dev 段
// （vdb / sda / vdaa …）。只用于**识别并提示**，不用于删除。
var dataDiskDevicePattern = regexp.MustCompile(`^[vs]d[a-z]{1,3}$`)

// vmDiskManifest 是一台虚拟机创建时落盘的磁盘清单。
//
// 字段保持最小：名字（自校验，防止文件被改名后误用）、磁盘绝对路径、
// 创建时间（排障时判断清单的新旧）。磁盘路径是**节点侧绝对路径**——清单
// 只对同一台宿主机有意义，与回收站里的域 XML 同一个理由。
type vmDiskManifest struct {
	Name      string   `json:"name"`
	Disks     []string `json:"disks"`
	CreatedAt int64    `json:"created_at"`
}

// diskManifestPath 返回某台虚拟机的清单路径；名字不合法时返回空串。
//
// 名字直接参与路径拼接，因此拒绝任何可能越出 vmMetaDir 的输入（斜杠、
// 上跳、空名）。用 sanitizeFileName 做「改动即拒绝」的校验而不是静默清洗：
// 清洗会让两个不同的名字落到同一个文件上，清单就张冠李戴了。
func diskManifestPath(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || sanitizeFileName(name) != name {
		return ""
	}
	return filepath.Join(vmMetaDir, name+".json")
}

// writeDiskManifest 把磁盘清单写入 `<vmMetaDir>/<name>.json`。
//
// 原子替换（临时文件 + rename）：清单是删除路径的唯一依据，读到半截 JSON
// 会退到兜底分支——那正是我们要避免的结果。调用方（vmCreate）只记日志，
// 不因失败而回滚创建。
func writeDiskManifest(name string, disks []string) error {
	path := diskManifestPath(name)
	if path == "" {
		return fmt.Errorf("虚拟机名不合法，无法记录磁盘清单: %q", name)
	}
	if err := os.MkdirAll(vmMetaDir, 0o755); err != nil {
		return err
	}
	clean := make([]string, 0, len(disks))
	for _, d := range disks {
		if d != "" {
			clean = append(clean, d)
		}
	}
	body, err := jsonMarshal(vmDiskManifest{
		Name:      name,
		Disks:     clean,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if werr := osWriteFile(tmp, append(body, '\n'), 0o600); werr != nil {
		_ = osRemove(tmp)
		return werr
	}
	if rerr := osRename(tmp, path); rerr != nil {
		_ = osRemove(tmp)
		return rerr
	}
	return nil
}

// readDiskManifest 读取磁盘清单。第二个返回值表示「拿到了一份可用的清单」：
// 文件不存在、内容损坏、名字对不上、磁盘为空都算失败——此时调用方必须走
// 兜底分支，而不是当成"这台机器没有磁盘"。
func readDiskManifest(name string) (vmDiskManifest, bool) {
	path := diskManifestPath(name)
	if path == "" {
		log.Printf("[nodeagent] 磁盘清单路径不可用（名字不合法）vm=%q", name)
		return vmDiskManifest{}, false
	}
	raw, err := osReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// 读不到的原因要留痕：权限问题与"从没写过清单"在排障上是
			// 完全不同的事（前者说明清单机制失效，后者只是旧机器）。
			log.Printf("[nodeagent] 读取磁盘清单失败 vm=%s: %v", name, err)
		}
		return vmDiskManifest{}, false
	}
	var m vmDiskManifest
	if jerr := jsonUnmarshalTo([]byte(raw), &m); jerr != nil {
		log.Printf("[nodeagent] 磁盘清单内容损坏 vm=%s path=%s: %v", name, path, jerr)
		return vmDiskManifest{}, false
	}
	if m.Name != name || len(m.Disks) == 0 {
		log.Printf("[nodeagent] 磁盘清单不可用 vm=%s path=%s（name=%q disks=%d）",
			name, path, m.Name, len(m.Disks))
		return vmDiskManifest{}, false
	}
	return m, true
}

// removeDiskManifest 删除清单文件（不存在视为成功）。
//
// 只在「磁盘已按 delete 语义处理完、任务已成功」之后调用；keep（回收站）
// 必须保留清单，否则恢复回来的机器再删除时又回到"XML 读不到"的死角。
func removeDiskManifest(name string) {
	path := diskManifestPath(name)
	if path == "" {
		return
	}
	if err := osRemove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("[nodeagent] 删除磁盘清单失败 vm=%s: %v", name, err)
	}
}

// disksForDomainMissing 在域定义里读不到任何磁盘时，给出「该删哪些文件」。
//
// 第 1 层（主路径）：读创建时落盘的清单，返回清单里的全部绝对路径。
// 第 2 层（兜底）：清单不存在/损坏（旧机器）时，按**精确文件名**逐个探测
// `<imagesDir>/<name>.{qcow2,raw,img}`，只返回真实存在的那些。
//
// 返回值 note 非空表示「有需要人工确认的残留」：同目录里存在
// `<name>-<dev>.<ext>` 形态的文件。这些文件**绝不自动删除**——VM 名本身
// 可能长成 `foo-vdb`，删掉 `foo-vdb.qcow2` 就等于删掉了另一台机器的系统盘。
// 交给用户判断是唯一安全的做法。
func disksForDomainMissing(name string) (paths []string, note string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ""
	}
	if m, ok := readDiskManifest(name); ok {
		// 清单是节点自己写的，但删除是不可逆动作：仍按「只允许镜像目录内
		// 的绝对路径」过滤一次。清单被篡改/损坏时，宁可回到精确名兜底，
		// 也不能让一个 JSON 文件变成删除任意路径的能力。
		out := make([]string, 0, len(m.Disks))
		for _, p := range m.Disks {
			if !insideImagesDir(p) {
				log.Printf("[nodeagent] 磁盘清单含镜像目录之外的路径，已忽略 vm=%s path=%s", name, p)
				continue
			}
			out = append(out, p)
		}
		if len(out) > 0 {
			return out, ""
		}
	}

	// 兜底：只按精确文件名试，不用 Glob / 前缀匹配。
	out := make([]string, 0, len(diskFormatExtensions))
	for _, ext := range diskFormatExtensions {
		p := filepath.Join(imagesDir, name+"."+ext)
		st, err := osStat(p)
		if err != nil || st.IsDir() {
			continue
		}
		out = append(out, p)
	}
	if orphans := dataDiskResidueFiles(name); len(orphans) > 0 {
		note = "数据盘可能残留，需要人工确认（未自动删除）: " + strings.Join(orphans, "、")
		log.Printf("[nodeagent] 虚拟机 %s 的删除可能留下数据盘: %s", name, strings.Join(orphans, "、"))
	}
	return out, note
}

// insideImagesDir 报告路径是否是 imagesDir 内的绝对路径。
//
// 用于给磁盘清单做最后一道边界：清单只应包含创建时落在镜像目录里的盘。
func insideImagesDir(p string) bool {
	if p == "" || !filepath.IsAbs(p) {
		return false
	}
	rel, err := filepath.Rel(imagesDir, p)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return false
	}
	return true
}

// dataDiskResidueFiles 列出镜像目录里形如 `<vm>-<dev>.<ext>` 的文件（仅提示用）。
//
// 这些文件有可能是这台机器的数据盘，也可能是另一台名字更长（`<vm>-vdb`）
// 的机器的系统盘——命名上无法区分，所以这里只**报告**不删除。
func dataDiskResidueFiles(name string) []string {
	entries, err := osReadDir(imagesDir)
	if err != nil {
		return nil
	}
	prefix := name + "-"
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fn := e.Name()
		if !strings.HasPrefix(fn, prefix) {
			continue
		}
		rest := strings.TrimPrefix(fn, prefix)
		dev := strings.TrimSuffix(rest, filepath.Ext(rest))
		ext := strings.TrimPrefix(filepath.Ext(rest), ".")
		if !contains(diskFormatExtensions, ext) || !dataDiskDevicePattern.MatchString(dev) {
			continue
		}
		out = append(out, filepath.Join(imagesDir, fn))
	}
	return out
}
