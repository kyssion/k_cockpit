// ex_vm_export.go：虚拟机导出为 OVA 包。
//
// OVA 是 **未压缩 tar**，里面至少有一份 OVF 描述和一块磁盘。它不能由
// `qemu-img convert -O ova` 生成（qemu-img 没有 ova 输出格式）——这里先
// 把系统盘转成 streamOptimized VMDK，再写 OVF 与清单，最后打 tar。
package nodeagent

import (
	"archive/tar"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

const ovaDiskName = "disk.vmdk"

// domainOVFMeta 是 OVF 需要的最小域信息。
type domainOVFMeta struct {
	Name       string
	VCPU       int
	MemoryMB   int
	DiskSizeGB int
}

// parseDomainOVFMeta 从 dumpxml 中取名称、vCPU 与内存。
func parseDomainOVFMeta(xmlText string) domainOVFMeta {
	var doc struct {
		Name   string `xml:"name"`
		VCPU   int    `xml:"vcpu"`
		Memory struct {
			Unit  string `xml:"unit,attr"`
			Value int64  `xml:",chardata"`
		} `xml:"memory"`
	}
	meta := domainOVFMeta{Name: "vm", VCPU: 1, MemoryMB: 1024}
	if err := xml.Unmarshal([]byte(xmlText), &doc); err != nil {
		return meta
	}
	if strings.TrimSpace(doc.Name) != "" {
		meta.Name = strings.TrimSpace(doc.Name)
	}
	if doc.VCPU > 0 {
		meta.VCPU = doc.VCPU
	}
	if doc.Memory.Value > 0 {
		meta.MemoryMB = int(memoryToMB(doc.Memory.Value, doc.Memory.Unit))
	}
	return meta
}

// memoryToMB 把 libvirt 的 memory 值换算为 MB（默认单位是 KiB）。
func memoryToMB(v int64, unit string) int64 {
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case "mib", "mb":
		return v
	case "gib", "gb":
		return v * 1024
	case "tib", "tb":
		return v * 1024 * 1024
	default: // KiB 是 libvirt 的默认单位
		return v / 1024
	}
}

// buildOVF 渲染一份最小但可被常见虚拟化平台识别的 OVF。
func buildOVF(meta domainOVFMeta, diskBytes int64) string {
	if meta.Name == "" {
		meta.Name = "vm"
	}
	if meta.VCPU <= 0 {
		meta.VCPU = 1
	}
	if meta.MemoryMB <= 0 {
		meta.MemoryMB = 1024
	}
	if meta.DiskSizeGB <= 0 {
		const gib = int64(1024 * 1024 * 1024)
		meta.DiskSizeGB = int((diskBytes + gib - 1) / gib)
		if meta.DiskSizeGB <= 0 {
			meta.DiskSizeGB = 1
		}
	}
	id := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, meta.Name)
	if id == "" {
		id = "vm"
	}
	name := xmlEsc(meta.Name)
	return `<?xml version="1.0" encoding="UTF-8"?>
<Envelope ovf:version="1.0" xml:lang="en-US"
  xmlns="http://schemas.dmtf.org/ovf/envelope/1"
  xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
  xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
  xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData"
  xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <References>
    <File ovf:id="file1" ovf:href="` + ovaDiskName + `" ovf:size="` + strconv.FormatInt(diskBytes, 10) + `"/>
  </References>
  <DiskSection>
    <Info>Virtual disks</Info>
    <Disk ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:capacity="` + strconv.Itoa(meta.DiskSizeGB) + `" ovf:capacityAllocationUnits="byte * 2^30" ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
  </DiskSection>
  <NetworkSection>
    <Info>Logical networks</Info>
    <Network ovf:name="default"><Description>Default network</Description></Network>
  </NetworkSection>
  <VirtualSystem ovf:id="` + id + `">
    <Info>Exported from k_cockpit</Info>
    <Name>` + name + `</Name>
    <OperatingSystemSection ovf:id="100"><Info>Other</Info></OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware requirements</Info>
      <Item>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>` + strconv.Itoa(meta.VCPU) + `</rasd:VirtualQuantity>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
      </Item>
      <Item>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>` + strconv.Itoa(meta.MemoryMB) + `</rasd:VirtualQuantity>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
      </Item>
      <Item>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceType>17</rasd:ResourceType>
        <rasd:HostResource>ovf:/disk/vmdisk1</rasd:HostResource>
        <rasd:Parent>2</rasd:Parent>
      </Item>
      <Item>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:ResourceType>10</rasd:ResourceType>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Connection>default</rasd:Connection>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>
`
}

// writeOVA 把 VMDK、OVF 与 SHA1 清单写成未压缩 tar。
func writeOVA(ctx context.Context, outPath string, ovf string, diskPath string) error {
	out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(outPath)
		}
	}()

	tw := tar.NewWriter(out)
	defer func() { _ = tw.Close() }()

	disk, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer func() { _ = disk.Close() }()
	st, err := disk.Stat()
	if err != nil {
		return err
	}

	diskHash := sha1.New()
	if err := writeTarEntry(tw, ovaDiskName, st.Size(), 0o644,
		io.TeeReader(disk, diskHash)); err != nil {
		return err
	}
	ovfName := "system.ovf"
	ovfHash := sha1.Sum([]byte(ovf))
	if err := writeTarEntry(tw, ovfName, int64(len(ovf)), 0o644,
		strings.NewReader(ovf)); err != nil {
		return err
	}
	manifest := fmt.Sprintf("SHA1(%s)= %s\nSHA1(%s)= %s\n",
		ovaDiskName, hex.EncodeToString(diskHash.Sum(nil)),
		ovfName, hex.EncodeToString(ovfHash[:]))
	if err := writeTarEntry(tw, "manifest.mf", int64(len(manifest)), 0o644,
		strings.NewReader(manifest)); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	ok = true
	return nil
}

func writeTarEntry(tw *tar.Writer, name string, size int64, mode int64, r io.Reader) error {
	hdr := &tar.Header{
		Name: name, Size: size, Mode: mode,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if size == 0 {
		return nil
	}
	_, err := io.Copy(tw, r)
	return err
}

// vmExportOVA 把系统盘导出为标准 OVA。
func vmExportOVA(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	if running, err := vmRunning(ctx, op.Target); err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	} else if running {
		return failResult("导出 OVA 需要先关机：运行中的磁盘写入会让包内容不一致"), nil
	}

	xmlText, err := sys.DumpXML(ctx, op.Target, false)
	if err != nil {
		return failResult("读取域定义失败: " + err.Error()), nil
	}
	source := ""
	for _, d := range sys.ParseDomainDisks(xmlText) {
		if d.IsCDROM || d.IsFloppy {
			continue
		}
		if d.SourceFile != "" {
			source = d.SourceFile
			break
		}
	}
	if source == "" {
		return failResult("找不到系统盘"), nil
	}

	report(Stage{Key: "convert", Name: "转换为 VMDK", Index: 1, Total: 2})
	_ = osMkdirAllErr(exportDir)
	vmdk := filepath.Join(exportDir, op.Target+".ova-disk.vmdk")
	_ = osRemove(vmdk)
	// streamOptimized 是 OVF 对 VMDK 的常见要求；不带它时部分平台会拒绝。
	if _, err := sys.RunTimeout(ctx, qemuConvertTimeout, "qemu-img", "convert",
		"-O", "vmdk", "-o", "subformat=streamOptimized", source, vmdk); err != nil {
		_ = osRemove(vmdk)
		return failResult("转换为 VMDK 失败: " + err.Error()), nil
	}
	defer func() { _ = osRemove(vmdk) }()

	diskSize, err := sys.QemuImgSize(ctx, source)
	if err != nil {
		return failResult("读取磁盘容量失败: " + err.Error()), nil
	}
	meta := parseDomainOVFMeta(xmlText)
	meta.DiskSizeGB = int((diskSize + (1 << 30) - 1) / (1 << 30))
	// OVF 的 File size 是包内文件的实际大小；capacity 才用虚拟容量。
	fileSize := diskSize
	if st, err := os.Stat(vmdk); err == nil {
		fileSize = st.Size()
	}
	ovf := buildOVF(meta, fileSize)

	report(Stage{Key: "package", Name: "打包 OVA", Index: 2, Total: 2})
	out := filepath.Join(exportDir, op.Target+".ova")
	if err := writeOVA(ctx, out, ovf, vmdk); err != nil {
		return failResult("打包 OVA 失败: " + err.Error()), nil
	}
	st, _ := os.Stat(out)
	return &agent.Result{Success: true, Data: map[string]any{
		agent.ExportDataKey: agent.ExportInfo{
			FilePath: out, FileName: filepath.Base(out), SizeBytes: fileSizeOr0(st),
		},
	}}, nil
}
