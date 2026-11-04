package nodeagent

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"testing"
)

const testOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1">
  <References>
    <File ovf:href="disk.vmdk" ovf:size="1073741824"/>
  </References>
  <DiskSection>
    <Disk ovf:capacity="10737418240" ovf:fileRef="disk.vmdk"/>
  </DiskSection>
  <VirtualSystem>
    <OperatingSystemSection ovf:osType="ubuntu64Guest">
      <Description>Ubuntu 24.04</Description>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Item><ResourceType>3</ResourceType><VirtualQuantity>4</VirtualQuantity></Item>
      <Item><ResourceType>4</ResourceType><VirtualQuantity>8192</VirtualQuantity></Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

func writeTestOVA(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.ova")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	entries := []struct {
		name string
		body []byte
	}{
		{"test.ovf", []byte(testOVF)},
		{"disk.vmdk", make([]byte, 1024)},
		{"small.vmdk", make([]byte, 10)},
	}
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseOVA(t *testing.T) {
	ova := writeTestOVA(t)
	preview, err := parseOVA(context.Background(), ova)
	if err != nil {
		t.Fatalf("parseOVA: %v", err)
	}
	if preview.VCPU != 4 || preview.MemoryMB != 8192 || preview.DiskGB != 10 {
		t.Errorf("解析结果不对: %+v", preview)
	}
	if preview.OSType != "linux" || preview.OSVariant != "ubuntu64guest" {
		t.Errorf("OS 解析不对: %+v", preview)
	}
	if preview.Sources["vcpu"] != "ovf" || preview.Sources["memory_mb"] != "ovf" {
		t.Errorf("来源标记不对: %+v", preview.Sources)
	}
}

func TestLargestOVADiskName(t *testing.T) {
	ova := writeTestOVA(t)
	name, err := largestOVADiskName(context.Background(), ova)
	if err != nil {
		t.Fatal(err)
	}
	if name != "disk.vmdk" {
		t.Errorf("最大磁盘应为 disk.vmdk，得到 %q", name)
	}
}

func TestHasAnySuffix(t *testing.T) {
	if !hasAnySuffix("a/b/disk.QCOW2", imageDiskExts) {
		t.Error("大小写不敏感匹配失败")
	}
	if hasAnySuffix("a/b/readme.txt", imageDiskExts) {
		t.Error("非磁盘后缀不应命中")
	}
}
