package nodeagent

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseDomainOVFMeta(t *testing.T) {
	xml := `<domain><name>demo</name><vcpu>4</vcpu><memory unit='MiB'>2048</memory></domain>`
	meta := parseDomainOVFMeta(xml)
	if meta.Name != "demo" || meta.VCPU != 4 || meta.MemoryMB != 2048 {
		t.Fatalf("解析结果不对: %+v", meta)
	}
}

func TestBuildOVF(t *testing.T) {
	ovf := buildOVF(domainOVFMeta{Name: "demo", VCPU: 2, MemoryMB: 1024}, 20<<30)
	for _, want := range []string{
		"disk.vmdk", `ovf:capacity="20"`, "<rasd:VirtualQuantity>2</rasd:VirtualQuantity>",
		"<Name>demo</Name>", "vmdisk1",
	} {
		if !strings.Contains(ovf, want) {
			t.Errorf("OVF 缺少 %q:\n%s", want, ovf)
		}
	}
}

func TestWriteOVAContainsDiskOVFAndManifest(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.vmdk")
	if err := os.WriteFile(disk, []byte("fake-vmdk"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "demo.ova")
	if err := writeOVA(context.Background(), out, buildOVF(domainOVFMeta{Name: "demo"}, 1<<30), disk); err != nil {
		t.Fatalf("writeOVA: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name] = string(b)
	}
	for _, name := range []string{"disk.vmdk", "system.ovf", "manifest.mf"} {
		if _, ok := got[name]; !ok {
			t.Errorf("OVA 缺少 %s，实际 %v", name, keysOf(got))
		}
	}
	if !strings.Contains(got["manifest.mf"], "SHA1(disk.vmdk)=") {
		t.Errorf("清单缺少磁盘摘要:\n%s", got["manifest.mf"])
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
