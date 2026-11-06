// ex_image.go：镜像解析与导入（F-2-13）。
//
// 解析只读 OVA 里的 OVF 描述，不接触磁盘；导入才做格式转换与落盘。
// 两者分开是产品要求：用户必须在按下"创建"之前看到将创建成什么。
package nodeagent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// imageDiskExts 是 OVA 内可能承载系统盘的扩展名。
var imageDiskExts = []string{".vmdk", ".qcow2", ".raw", ".img"}

// ovfEnvelope 是 OVF 描述里我们需要的部分。
type ovfEnvelope struct {
	References []struct {
		Href string `xml:"href,attr"`
		Size int64  `xml:"size,attr"`
	} `xml:"References>File"`

	DiskSection struct {
		Disks []struct {
			Capacity int64 `xml:"capacity,attr"`
			// CapacityAllocationUnits 是容量的**单位**（第 3 轮 DEF-R3-07）。
			//
			// OVF 允许 capacity 用字节以外的单位表达，例如
			// `capacity="8" capacityAllocationUnits="byte * 2^30"`——那就是 8 GiB。
			// 只看 capacity 会把它当成 8 **字节** → 向上取整成 1 GB，正是真机上
			// 观测到的 `disk_gb=1`。
			CapacityAllocationUnits string `xml:"capacityAllocationUnits,attr"`
			FileRef                 string `xml:"fileRef,attr"`
		} `xml:"Disk"`
	} `xml:"DiskSection"`

	VirtualSystem struct {
		OperatingSystemSection struct {
			OSType      string `xml:"osType,attr"`
			Description string `xml:"Description"`
		} `xml:"OperatingSystemSection"`
		VirtualHardwareSection struct {
			Items []struct {
				ResourceType    int    `xml:"ResourceType"`
				VirtualQuantity int64  `xml:"VirtualQuantity"`
				ElementName     string `xml:"ElementName"`
			} `xml:"Item"`
		} `xml:"VirtualHardwareSection"`
	} `xml:"VirtualSystem"`
}

// ovfCapacityBytes 把 OVF 的 capacity + capacityAllocationUnits 换算成字节。
//
// 单位缺省是 **byte**（OVF 规范）。规范里常用的写法还有
// `byte * 2^30`（GiB）、`byte * 2^20`（MiB）、`megabyte`/`gigabyte` 等。
// 不认识单位时按字节处理并**照原值使用**（宁可小也不要凭空放大），
// 由调用方的"最大盘文件大小"兜底逻辑兜住明显不合理的值。
func ovfCapacityBytes(capacity int64, units string) int64 {
	if capacity <= 0 {
		return 0
	}
	// 把所有空白去掉再比对：规范里的写法是 `byte * 2^30`，但生成工具
	// 也可能写出 `byte*2^30` / `BYTE  *  2^30`。空格的有无不是语义差异。
	u := strings.ToLower(strings.Join(strings.Fields(units), ""))
	mult := int64(1)
	switch u {
	case "", "byte", "bytes":
		mult = 1
	case "kilobyte", "kilobytes", "byte*2^10":
		mult = 1 << 10
	case "megabyte", "megabytes", "byte*2^20":
		mult = 1 << 20
	case "gigabyte", "gigabytes", "byte*2^30":
		mult = 1 << 30
	case "terabyte", "terabytes", "byte*2^40":
		mult = 1 << 40
	default:
		mult = 1
	}
	return capacity * mult
}

// vmImageParse 实现 image.parse。
func vmImageParse(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		rel = op.Target
	}
	p, err := resolveUserdataPath(rel)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if _, err := os.Stat(p); err != nil {
		return failResult("镜像文件不存在: " + err.Error()), nil
	}

	report(Stage{Key: "archive_open", Name: "打开 OVA 包", Index: 1, Total: 2})
	preview, err := parseOVA(ctx, p)
	if err != nil {
		return failResult("解析 OVA 失败: " + err.Error()), nil
	}
	report(Stage{Key: "ovf_read", Name: "读取 OVF 描述", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Message: "已解析 OVA 描述",
		Data:    map[string]any{agent.ImageParseDataKey: preview},
	}, nil
}

// parseOVA 读取包内 OVF 并返回预览。
func parseOVA(ctx context.Context, path string) (agent.ImagePreview, error) {
	out := agent.ImagePreview{Sources: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()

	tr, closer, err := newTarReader(f)
	if err != nil {
		return out, err
	}
	defer closer()

	var ovf []byte
	var largestDisk int64
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		lower := strings.ToLower(hdr.Name)
		switch {
		case strings.HasSuffix(lower, ".ovf"):
			if len(ovf) == 0 {
				ovf, err = io.ReadAll(io.LimitReader(tr, 8<<20))
				if err != nil {
					return out, fmt.Errorf("读取 OVF 失败: %w", err)
				}
			}
		case hasAnySuffix(lower, imageDiskExts):
			if hdr.Size > largestDisk {
				largestDisk = hdr.Size
			}
		}
	}
	if len(ovf) == 0 {
		return out, fmt.Errorf("包内没有 .ovf 描述")
	}

	var env ovfEnvelope
	if err := xml.Unmarshal(ovf, &env); err != nil {
		return out, fmt.Errorf("OVF XML 解析失败: %w", err)
	}
	for _, item := range env.VirtualSystem.VirtualHardwareSection.Items {
		switch item.ResourceType {
		case 3: // Processor
			if item.VirtualQuantity > 0 {
				out.VCPU = int(item.VirtualQuantity)
				out.Sources["vcpu"] = "ovf"
			}
		case 4: // Memory (MiB)
			if item.VirtualQuantity > 0 {
				out.MemoryMB = int(item.VirtualQuantity)
				out.Sources["memory_mb"] = "ovf"
			}
		}
	}

	var capacity int64
	for _, d := range env.DiskSection.Disks {
		if b := ovfCapacityBytes(d.Capacity, d.CapacityAllocationUnits); b > capacity {
			capacity = b
		}
	}
	if capacity == 0 {
		capacity = largestDisk
	}
	if capacity > 0 {
		out.DiskGB = int((capacity + (1 << 30) - 1) >> 30)
		out.Sources["disk_gb"] = "ovf"
	}

	osType := strings.ToLower(env.VirtualSystem.OperatingSystemSection.OSType)
	desc := strings.ToLower(env.VirtualSystem.OperatingSystemSection.Description)
	switch {
	case strings.Contains(osType, "windows") || strings.Contains(desc, "windows"):
		out.OSType = "windows"
		out.OSVariant = osType
	case strings.Contains(osType, "ubuntu") || strings.Contains(desc, "ubuntu"):
		out.OSType = "linux"
		out.OSVariant = osType
	default:
		out.OSType = "linux"
		if osType != "" {
			out.OSVariant = osType
		}
	}
	if out.OSType != "" {
		out.Sources["os_type"] = "ovf"
	}
	if out.VCPU == 0 || out.MemoryMB == 0 {
		return out, fmt.Errorf("OVF 未提供可识别的 CPU / 内存配置")
	}
	return out, nil
}

// vmImageImport 实现 image.import。
func vmImageImport(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "source_verify", Name: "校验源文件", Index: 1, Total: 4})
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		rel = op.Target
	}
	source, err := resolveUserdataPath(rel)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if st, err := os.Stat(source); err != nil || st.IsDir() {
		return failResult("源文件不可用: " + source), nil
	}

	name := strParam(op.Params, "name")
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
	}
	finalPath := filepath.Join(templatesDir, sanitizeFileName(name)+".qcow2")
	if _, err := os.Stat(finalPath); err == nil {
		return failResult("同名模板盘已存在: " + finalPath), nil
	}
	if err := osMkdirAllErr(templatesDir); err != nil {
		return failResult("创建模板目录失败: " + err.Error()), nil
	}

	tmp := filepath.Join(templatesDir, ".import-"+fmt.Sprint(time.Now().UnixNano())+".qcow2")
	_ = os.Remove(tmp)
	fail := func(msg string) (*agent.Result, error) {
		_ = os.Remove(tmp)
		return failResult(msg), nil
	}

	format := strings.ToLower(strParam(op.Params, "source_format"))
	var notes []string
	report(Stage{Key: "format_convert", Name: "转换格式", Index: 2, Total: 4})
	if format == "ova" {
		diskName, err := extractLargestOVADisk(ctx, source, tmp)
		if err != nil {
			return fail("从 OVA 解出磁盘失败: " + err.Error())
		}
		notes = append(notes, "已从 OVA 解出磁盘 "+diskName)
		if diskName != "" && !strings.HasSuffix(strings.ToLower(diskName), ".qcow2") {
			converted := tmp + ".qcow2"
			if _, err := sys.RunTimeout(ctx, qemuConvertTimeout,
				"qemu-img", "convert", "-O", "qcow2", tmp, converted); err != nil {
				_ = os.Remove(converted)
				return fail("转换磁盘格式失败: " + err.Error())
			}
			_ = os.Remove(tmp)
			tmp = converted
			notes = append(notes, "源格式已转换为 qcow2")
		}
	} else {
		if _, err := sys.RunTimeout(ctx, qemuConvertTimeout,
			"qemu-img", "convert", "-O", "qcow2", source, tmp); err != nil {
			return fail("转换磁盘格式失败: " + err.Error())
		}
		if format != "" && format != "qcow2" {
			notes = append(notes, "源格式 "+format+" 已转换为 qcow2")
		}
	}

	report(Stage{Key: "disk_place", Name: "放入存储池", Index: 3, Total: 4})
	if err := ensureQemuReadable(tmp); err != nil {
		return fail("设置镜像属主失败: " + err.Error())
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		return fail("落位镜像失败: " + err.Error())
	}
	ensureAppArmorAllowsImages(templatesDir)

	report(Stage{Key: "template_register", Name: "登记为模板", Index: 4, Total: 4})
	size, err := sys.QemuImgSize(ctx, finalPath)
	if err != nil {
		_ = os.Remove(finalPath)
		return fail("读取镜像容量失败: " + err.Error())
	}
	const gib = int64(1024 * 1024 * 1024)
	return &agent.Result{
		Success: true,
		Message: "镜像已导入",
		Data: map[string]any{agent.ImageImportDataKey: agent.ImageImportInfo{
			DiskPath: finalPath,
			SizeGB:   int((size + gib - 1) / gib),
			Format:   "qcow2",
			Notes:    notes,
		}},
	}, nil
}

// newTarReader 识别 OVA（未压缩 tar）与 tar.gz 两种载体。
func newTarReader(r io.Reader) (*tar.Reader, func(), error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(2)
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return nil, func() {}, err
		}
		return tar.NewReader(gr), func() { _ = gr.Close() }, nil
	}
	return tar.NewReader(br), func() {}, nil
}

// extractLargestOVADisk 把 OVA 里最大的磁盘条目解到 dst。
func extractLargestOVADisk(ctx context.Context, ovaPath, dst string) (string, error) {
	name, err := largestOVADiskName(ctx, ovaPath)
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("包内没有磁盘文件")
	}

	f, err := os.Open(ovaPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr, closer, err := newTarReader(f)
	if err != nil {
		return "", err
	}
	defer closer()

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("包内没有 %s", name)
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != name {
			continue
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			return "", err
		}
		return name, out.Close()
	}
}

// largestOVADiskName 返回包内最大的磁盘条目名。
func largestOVADiskName(ctx context.Context, ovaPath string) (string, error) {
	f, err := os.Open(ovaPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr, closer, err := newTarReader(f)
	if err != nil {
		return "", err
	}
	defer closer()

	var name string
	var size int64
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return name, nil
		}
		if err != nil {
			return "", err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hasAnySuffix(strings.ToLower(hdr.Name), imageDiskExts) && hdr.Size > size {
			name, size = hdr.Name, hdr.Size
		}
	}
}

func hasAnySuffix(s string, suffixes []string) bool {
	s = strings.ToLower(s)
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}
