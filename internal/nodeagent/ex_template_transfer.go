// ex_template_transfer.go：模板包导出 / 导入（F-3-05）。
//
// 包格式固定为 tar.gz：
//
//	manifest.json   —— 清单（名称/版本/族/规格/磁盘文件名/磁盘 SHA256）
//	disk.qcow2      —— 模板磁盘
//
// 两个哈希：清单里的 disk_sha256 校验磁盘，TemplateExportInfo.SHA256
// 校验整个压缩包。导入前先算磁盘哈希，摘要不符直接拒绝——包是别人给的，
// 不能等解完再用。
package nodeagent

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

const (
	// templatePackageChunk 是单次取回的最大分片（与 VM 导出一致，8 MiB）。
	templatePackageChunk = 8 << 20
)

// templatePackageRoot 是模板包在节点上的根（用户存储根之下）。
//
// 它是函数而不是常量：用户存储根可以被控制面改写到存储池里，模板包必须
// 跟着一起走——留一份写在系统盘上的常量会得到两个互相看不见的目录。
func templatePackageRoot() string { return userdataRoot() + "/templates" }

// templatePackageManifest 是 manifest.json 的线上形状。
//
// 单独定义而不是直接序列化 agent.TemplateManifest：后者的字段名会随
// Go 重构变化，而包格式是对外契约，必须有稳定的 snake_case 字段。
type templatePackageManifest struct {
	Name       string `json:"name"`
	Version    int    `json:"version"`
	FamilyName string `json:"family_name,omitempty"`
	DiskFormat string `json:"disk_format"`
	DiskSizeGB int    `json:"disk_size_gb"`

	OSType    string `json:"os_type,omitempty"`
	OSVariant string `json:"os_variant,omitempty"`

	DefaultCPU      int    `json:"default_cpu,omitempty"`
	DefaultMemoryMB int    `json:"default_memory_mb,omitempty"`
	DefaultDiskBus  string `json:"default_disk_bus,omitempty"`
	DefaultNicModel string `json:"default_nic_model,omitempty"`
	DefaultFirmware string `json:"default_firmware,omitempty"`

	DiskFile   string `json:"disk_file"`
	DiskSHA256 string `json:"disk_sha256"`
	CreatedAt  string `json:"created_at,omitempty"`
}

func (m templatePackageManifest) toAgent() agent.TemplateManifest {
	return agent.TemplateManifest{
		Name: m.Name, Version: m.Version, FamilyName: m.FamilyName,
		DiskFormat: m.DiskFormat, DiskSizeGB: m.DiskSizeGB,
		OSType: m.OSType, OSVariant: m.OSVariant,
		DefaultCPU: m.DefaultCPU, DefaultMemoryMB: m.DefaultMemoryMB,
		DefaultDiskBus: m.DefaultDiskBus, DefaultNicModel: m.DefaultNicModel,
		DefaultFirmware: m.DefaultFirmware,
	}
}

// resolveUserdataPath 把「我的存储」相对路径解析成绝对路径并做包含性校验。
func resolveUserdataPath(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("必须是相对于用户存储根的路径")
	}
	// 切换存储根之前上传的文件仍在历史根里，读取类调用必须能找到它们。
	p := resolveExistingUserdataPath(rel)
	if !underUserdataRoot(p) {
		return "", fmt.Errorf("路径越出用户存储根: %s", rel)
	}
	return p, nil
}

// requireUnder 校验路径位于 root 之内（用于模板盘/模板包目录）。
func requireUnder(root, p string) error {
	clean := filepath.Clean(p)
	if clean != root && !strings.HasPrefix(clean, root+"/") {
		return fmt.Errorf("路径不在 %s 内: %s", root, p)
	}
	return nil
}

// sha256File 流式计算文件摘要，支持 ctx 取消。
func sha256File(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// vmTemplateExport 实现 template.export。
func vmTemplateExport(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	name := strParam(op.Params, "name")
	diskPath := strParam(op.Params, "disk_path")
	format := strParam(op.Params, "disk_format")
	if format == "" {
		format = "qcow2"
	}
	version, _ := paramInt(op.Params, "version")
	if version <= 0 {
		version = 1
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(diskPath), filepath.Ext(diskPath))
	}

	if err := requireUnder(templatesDir, diskPath); err != nil {
		return failResult("模板盘路径非法: " + err.Error()), nil
	}
	st, err := os.Stat(diskPath)
	if err != nil || st.IsDir() {
		return failResult("模板盘不可用: " + diskPath), nil
	}

	report(Stage{Key: "manifest_build", Name: "生成清单", Index: 1, Total: 3})
	report(Stage{Key: "digest_calc", Name: "计算摘要", Index: 2, Total: 3})
	diskHash, err := sha256File(ctx, diskPath)
	if err != nil {
		return failResult("计算模板盘摘要失败: " + err.Error()), nil
	}
	manifest := templatePackageManifest{
		Name: name, Version: version,
		FamilyName: strParam(op.Params, "family_name"),
		DiskFormat: format,
		DiskSizeGB: func() int { v, _ := paramInt(op.Params, "disk_size"); return v }(),
		OSType:     strParam(op.Params, "os_type"),
		OSVariant:  strParam(op.Params, "os_variant"),
		DiskFile:   "disk.qcow2",
		DiskSHA256: diskHash,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	manifest.DefaultCPU, _ = paramInt(op.Params, "default_cpu")
	manifest.DefaultMemoryMB, _ = paramInt(op.Params, "default_memory_mb")
	manifest.DefaultDiskBus = strParam(op.Params, "default_disk_bus")
	manifest.DefaultNicModel = strParam(op.Params, "default_nic_model")
	manifest.DefaultFirmware = strParam(op.Params, "default_firmware")
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return failResult("序列化清单失败: " + err.Error()), nil
	}

	if err := osMkdirAllErr(templatePackageRoot()); err != nil {
		return failResult("创建模板包目录失败: " + err.Error()), nil
	}
	pkgName := sanitizeFileName(name) + "-v" + strconv.Itoa(version) + ".tar.gz"
	pkgPath := filepath.Join(templatePackageRoot(), pkgName)
	_ = os.Remove(pkgPath)

	report(Stage{Key: "archive_pack", Name: "打包为 tar.gz", Index: 3, Total: 3})
	if err := writeTemplatePackage(ctx, pkgPath, manifestJSON, diskPath); err != nil {
		_ = os.Remove(pkgPath)
		return failResult("打包模板失败: " + err.Error()), nil
	}
	pkgHash, err := sha256File(ctx, pkgPath)
	if err != nil {
		_ = os.Remove(pkgPath)
		return failResult("计算模板包摘要失败: " + err.Error()), nil
	}
	pkgStat, err := os.Stat(pkgPath)
	if err != nil {
		_ = os.Remove(pkgPath)
		return failResult("读取模板包大小失败: " + err.Error()), nil
	}

	relPath := "templates/" + pkgName
	return &agent.Result{
		Success: true,
		Message: "模板已导出",
		Data: map[string]any{agent.TemplateExportDataKey: agent.TemplateExportInfo{
			RelPath: relPath, Filename: pkgName, SizeBytes: pkgStat.Size(),
			SHA256: pkgHash, Manifest: manifest.toAgent(),
		}},
	}, nil
}

// writeTemplatePackage 写 tar.gz（manifest 在前、磁盘在后）。
func writeTemplatePackage(ctx context.Context, dst string, manifest []byte, diskPath string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	hasher := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(f, hasher))
	tw := tar.NewWriter(gw)

	if err := tw.WriteHeader(&tar.Header{
		Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest)),
		ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	if _, err := tw.Write(manifest); err != nil {
		return err
	}

	in, err := os.Open(diskPath)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "disk.qcow2", Mode: 0o644, Size: st.Size(),
		ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	buf := make([]byte, 4<<20)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := tw.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

// vmTemplateExportFetch 实现 template.export.fetch（分片）。
func vmTemplateExportFetch(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		rel = op.Target
	}
	p, err := resolveUserdataPath(rel)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if err := requireUnder(templatePackageRoot(), p); err != nil {
		return failResult("不是模板包路径: " + rel), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return failResult("模板包不存在: " + err.Error()), nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return failResult("读取模板包失败: " + err.Error()), nil
	}

	offset, _ := paramInt64(op.Params, "offset")
	length, _ := paramInt64(op.Params, "length")
	if length <= 0 || length > templatePackageChunk {
		length = templatePackageChunk
	}
	if offset < 0 || offset >= st.Size() {
		return &agent.Result{
			Success: true,
			Message: "已到文件末尾",
			Data: map[string]any{
				agent.ExportContentKey: agent.ExportContent{MIME: "application/gzip"},
				"total_size":           float64(st.Size()),
			},
		}, nil
	}
	if remain := st.Size() - offset; length > remain {
		length = remain
	}
	report(Stage{Key: "file_check", Name: "确认文件仍在保留期内", Index: 1, Total: 2})
	report(Stage{Key: "stream_back", Name: "回传文件内容", Index: 2, Total: 2})

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return failResult("定位模板包失败: " + err.Error()), nil
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return failResult("读取模板包失败: " + err.Error()), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{
			agent.ExportContentKey: agent.ExportContent{Data: buf[:n], MIME: "application/gzip"},
			"total_size":           float64(st.Size()),
		},
	}, nil
}

// vmTemplateExportDelete 实现 template.export.delete。
func vmTemplateExportDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "file_remove", Name: "删除导出包", Index: 1, Total: 1})
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		rel = op.Target
	}
	p, err := resolveUserdataPath(rel)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if err := requireUnder(templatePackageRoot(), p); err != nil {
		return failResult("拒绝删除模板包目录之外的路径: " + p), nil
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return failResult("删除模板包失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true, Message: "模板包已删除"}, nil
}

// scannedTemplatePackage 是扫描模板包的结果。
type scannedTemplatePackage struct {
	Manifest  templatePackageManifest
	DiskSize  int64
	DiskSHA   string
	DiskInTar string
}

// scanTemplatePackage 读取清单并计算包内磁盘的摘要。
func scanTemplatePackage(ctx context.Context, pkgPath string) (*scannedTemplatePackage, error) {
	f, err := os.Open(pkgPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("不是有效的 tar.gz: %w", err)
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	out := &scannedTemplatePackage{}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case hdr.Name == "manifest.json":
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&out.Manifest); err != nil {
				return nil, fmt.Errorf("清单解析失败: %w", err)
			}
		case hdr.Name == out.Manifest.DiskFile || hdr.Name == "disk.qcow2":
			h := sha256.New()
			n, err := io.Copy(h, tr)
			if err != nil {
				return nil, err
			}
			out.DiskSize = n
			out.DiskSHA = hex.EncodeToString(h.Sum(nil))
			out.DiskInTar = hdr.Name
		}
	}
	if out.Manifest.Name == "" {
		return nil, fmt.Errorf("模板包缺少 manifest.json 或清单为空")
	}
	if out.DiskInTar == "" {
		return nil, fmt.Errorf("模板包内没有磁盘文件")
	}
	return out, nil
}

// vmTemplateImportPreview 实现 template.import.preview。
func vmTemplateImportPreview(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	pkgPath, err := resolveImportPackage(op)
	if err != nil {
		return failResult(err.Error()), nil
	}
	report(Stage{Key: "archive_open", Name: "打开模板包", Index: 1, Total: 3})
	report(Stage{Key: "manifest_read", Name: "读取清单", Index: 2, Total: 3})
	report(Stage{Key: "digest_verify", Name: "校验内容摘要", Index: 3, Total: 3})

	scanned, err := scanTemplatePackage(ctx, pkgPath)
	if err != nil {
		return failResult("读取模板包失败: " + err.Error()), nil
	}
	mismatch := scanned.Manifest.DiskSHA256 != "" && scanned.DiskSHA != scanned.Manifest.DiskSHA256
	msg := "模板包校验通过"
	if mismatch {
		msg = "包内容与清单里的摘要不符，包可能已损坏或被改动"
	}
	return &agent.Result{
		Success: true,
		Message: msg,
		Data: map[string]any{agent.TemplateImportDataKey: agent.TemplateImportInfo{
			Manifest:       scanned.Manifest.toAgent(),
			Format:         scanned.Manifest.DiskFormat,
			DigestMismatch: mismatch,
			Message:        msg,
		}},
	}, nil
}

// vmTemplateImport 实现 template.import。
func vmTemplateImport(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	pkgPath, err := resolveImportPackage(op)
	if err != nil {
		return failResult(err.Error()), nil
	}
	report(Stage{Key: "archive_open", Name: "打开模板包", Index: 1, Total: 5})
	scanned, err := scanTemplatePackage(ctx, pkgPath)
	if err != nil {
		return failResult("读取模板包失败: " + err.Error()), nil
	}
	report(Stage{Key: "digest_verify", Name: "校验内容摘要", Index: 2, Total: 5})
	if scanned.Manifest.DiskSHA256 != "" && scanned.DiskSHA != scanned.Manifest.DiskSHA256 {
		return failResult("包内容与清单里的摘要不符，拒绝导入"), nil
	}

	name := scanned.Manifest.Name
	version := scanned.Manifest.Version
	if version <= 0 {
		version = 1
	}
	finalName := sanitizeFileName(name) + "-v" + strconv.Itoa(version) + ".qcow2"
	finalPath := filepath.Join(templatesDir, finalName)
	if _, err := os.Stat(finalPath); err == nil {
		return failResult("同名模板盘已存在: " + finalPath), nil
	}
	if err := osMkdirAllErr(templatesDir); err != nil {
		return failResult("创建模板目录失败: " + err.Error()), nil
	}

	report(Stage{Key: "disk_extract", Name: "解出磁盘镜像", Index: 3, Total: 5})
	tmp, err := os.CreateTemp(templatesDir, ".import-*.qcow2")
	if err != nil {
		return failResult("创建临时文件失败: " + err.Error()), nil
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpPath)
	fail := func(msg string) (*agent.Result, error) {
		_ = os.Remove(tmpPath)
		return failResult(msg), nil
	}

	if err := extractTemplateDisk(ctx, pkgPath, scanned.DiskInTar, tmpPath); err != nil {
		return fail("解出磁盘失败: " + err.Error())
	}
	report(Stage{Key: "disk_convert", Name: "转换磁盘格式", Index: 4, Total: 5})
	format := scanned.Manifest.DiskFormat
	if format == "" {
		format = "qcow2"
	}
	if format != "qcow2" {
		convertTo := tmpPath + ".qcow2"
		if _, err := sys.RunTimeout(ctx, qemuConvertTimeout,
			"qemu-img", "convert", "-O", "qcow2", tmpPath, convertTo); err != nil {
			_ = os.Remove(convertTo)
			return fail("转换磁盘格式失败: " + err.Error())
		}
		_ = os.Remove(tmpPath)
		tmpPath = convertTo
	}
	if err := ensureQemuReadable(tmpPath); err != nil {
		return fail("设置模板盘属主失败: " + err.Error())
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fail("落位模板盘失败: " + err.Error())
	}
	ensureAppArmorAllowsImages(templatesDir)

	report(Stage{Key: "template_register", Name: "登记为模板", Index: 5, Total: 5})
	size, err := sys.QemuImgSize(ctx, finalPath)
	if err != nil {
		_ = os.Remove(finalPath)
		return fail("读取模板盘容量失败: " + err.Error())
	}
	const gib = int64(1024 * 1024 * 1024)
	return &agent.Result{
		Success: true,
		Message: "模板已导入",
		Data: map[string]any{agent.TemplateImportDataKey: agent.TemplateImportInfo{
			Manifest: scanned.Manifest.toAgent(),
			DiskPath: finalPath,
			SizeGB:   int((size + gib - 1) / gib),
			Format:   "qcow2",
			Message:  "模板已导入",
		}},
	}, nil
}

// resolveImportPackage 解析导入来源的包路径。
func resolveImportPackage(op agent.Operation) (string, error) {
	rel := strParam(op.Params, "rel_path")
	if rel == "" {
		rel = op.Target
	}
	p, err := resolveUserdataPath(rel)
	if err != nil {
		return "", err
	}
	// 导入来源是「我的存储」里的任意模板包（用户上传位置不固定），
	// 只要在用户存储根内即可；导出产物才是固定落在 templates/ 下。
	if err := requireUnder(userdataRoot(), p); err != nil {
		return "", fmt.Errorf("不是用户存储路径: %s", rel)
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("模板包不存在: %w", err)
	}
	return p, nil
}

// extractTemplateDisk 从包中解出磁盘到指定路径（一次流式解包）。
func extractTemplateDisk(ctx context.Context, pkgPath, diskName, dst string) error {
	f, err := os.Open(pkgPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gr.Close()
	tr := tar.NewReader(gr)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("包内没有 %s", diskName)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != diskName {
			continue
		}
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			return err
		}
		return out.Close()
	}
}

// paramInt64 读取 JSON 数字为 int64（允许 float64 / int / string）。
func paramInt64(params map[string]any, key string) (int64, bool) {
	switch v := params[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
