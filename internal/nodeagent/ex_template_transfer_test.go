package nodeagent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplatePackageRoundTrip(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	content := []byte("fake qcow2 content for package round-trip")
	if err := os.WriteFile(disk, content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := sha256File(context.Background(), disk)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(templatePackageManifest{
		Name: "ubuntu-24.04", Version: 2, DiskFormat: "qcow2",
		DiskSizeGB: 20, DiskFile: "disk.qcow2", DiskSHA256: hash,
	})
	pkg := filepath.Join(dir, "pkg.tar.gz")
	if err := writeTemplatePackage(context.Background(), pkg, manifest, disk); err != nil {
		t.Fatalf("writeTemplatePackage: %v", err)
	}

	scanned, err := scanTemplatePackage(context.Background(), pkg)
	if err != nil {
		t.Fatalf("scanTemplatePackage: %v", err)
	}
	if scanned.Manifest.Name != "ubuntu-24.04" || scanned.Manifest.Version != 2 {
		t.Errorf("清单字段不对: %+v", scanned.Manifest)
	}
	if scanned.DiskSize != int64(len(content)) {
		t.Errorf("磁盘长度 %d，期望 %d", scanned.DiskSize, len(content))
	}
	if scanned.DiskSHA != hash {
		t.Errorf("磁盘摘要不一致: %s != %s", scanned.DiskSHA, hash)
	}
}

func TestScanTemplatePackageRejectsMissingManifest(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(disk, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(dir, "bad.tar.gz")
	if err := writeTemplatePackage(context.Background(), pkg, []byte(`{}`), disk); err != nil {
		t.Fatal(err)
	}
	if _, err := scanTemplatePackage(context.Background(), pkg); err == nil {
		t.Fatal("缺少名称的清单应被拒绝")
	}
}

func TestResolveUserdataPathGuardsTraversal(t *testing.T) {
	for _, rel := range []string{"", "/etc/passwd", "../etc/passwd", "a/../../etc/passwd"} {
		if _, err := resolveUserdataPath(rel); err == nil {
			t.Errorf("路径 %q 应被拒绝", rel)
		}
	}
	p, err := resolveUserdataPath("alice/disk.qcow2")
	if err != nil {
		t.Fatalf("合法路径被拒绝: %v", err)
	}
	if !strings.HasPrefix(p, userdataRoot+"/") {
		t.Errorf("路径不在用户存储根: %s", p)
	}
}

func TestRequireUnderRejectsSiblingPrefix(t *testing.T) {
	if err := requireUnder(templatesDir, templatesDir+"/x.qcow2"); err != nil {
		t.Errorf("目录内路径应通过: %v", err)
	}
	if err := requireUnder(templatesDir, templatesDir+"-evil/x.qcow2"); err == nil {
		t.Error("同前缀目录不应通过")
	}
}

func TestStringSliceParam(t *testing.T) {
	got := stringSliceParam(map[string]any{"steps": []any{"a", "b", 1}}, "steps")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("解析结果不对: %#v", got)
	}
	if got := stringSliceParam(map[string]any{"steps": []string{"x"}}, "steps"); len(got) != 1 {
		t.Errorf("[]string 应直接返回: %#v", got)
	}
	if got := stringSliceParam(map[string]any{}, "steps"); got != nil {
		t.Errorf("缺省应为 nil: %#v", got)
	}
}

func TestParamInt64(t *testing.T) {
	cases := []struct {
		v    any
		want int64
		ok   bool
	}{
		{float64(42), 42, true},
		{int(7), 7, true},
		{int64(9), 9, true},
		{"11", 11, true},
		{"x", 0, false},
	}
	for _, c := range cases {
		got, ok := paramInt64(map[string]any{"n": c.v}, "n")
		if got != c.want || ok != c.ok {
			t.Errorf("v=%#v got=%d ok=%v want=%d/%v", c.v, got, ok, c.want, c.ok)
		}
	}
}
