// ex_template.go：模板制备与删除（template.prepare / template.delete）。
//
// 模板在节点上就是一块独立的系统盘副本；控制面只保存路径与元数据。
// 制备要求源虚拟机关机：运行中复制得到的是崩溃一致性快照，克隆机开机
// 可能要做 fsck，严重的会只读挂载。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// templatesDir 是模板盘的节点侧根目录。
const templatesDir = "/var/lib/k_cockpit/templates"

// templateDiskPath 由模板名推导模板盘路径。
//
// 拒绝路径分隔符与 ..：模板名来自用户输入，直接拼进路径会让"制备模板"
// 变成一个可以向任意位置写文件的操作。
func templateDiskPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("模板名不能为空")
	}
	if strings.ContainsAny(name, `/\`) || name == "." || name == ".." ||
		strings.Contains(name, "..") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("模板名包含非法字符: %q", name)
	}
	return filepath.Join(templatesDir, name+".qcow2"), nil
}

// vmTemplatePrepare 实现 template.prepare。
func vmTemplatePrepare(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "check_stopped", Name: "确认源虚拟机已停机", Index: 1, Total: 3})
	running, err := vmRunning(ctx, op.Target)
	if err != nil {
		return failResult("无法确认虚拟机状态: " + err.Error()), nil
	}
	if running {
		return failResult("源虚拟机正在运行：请先关机，运行中复制得到的是崩溃一致性快照"), nil
	}
	if snap, err := hasSnapshots(ctx, op.Target); err != nil {
		return failResult("无法确认快照状态，已中止: " + err.Error()), nil
	} else if snap {
		return failResult("源虚拟机存在快照：请先删除快照，复制当前盘会丢失快照链"), nil
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
		if d.Dev == "vda" || d.Dev == "sda" {
			source = d.SourceFile
			break
		}
	}
	if source == "" {
		return failResult("找不到系统盘（vda / sda）"), nil
	}
	if st, err := os.Stat(source); err != nil || st.IsDir() {
		return failResult("系统盘文件不可用: " + source), nil
	}

	name := strParam(op.Params, "name")
	dest, err := templateDiskPath(name)
	if err != nil {
		return failResult(err.Error()), nil
	}
	if _, err := os.Stat(dest); err == nil {
		return failResult("同名模板盘已存在: " + dest), nil
	}
	if err := osMkdirAllErr(templatesDir); err != nil {
		return failResult("创建模板目录失败: " + err.Error()), nil
	}

	report(Stage{Key: "copy_disk", Name: "复制系统盘", Index: 2, Total: 3})
	// convert 而不是 cp：它会把快照链/backing 读成一份独立的完整镜像，
	// 模板不该再依赖任何父盘。
	if _, err := sys.RunTimeout(ctx, qemuConvertTimeout,
		"qemu-img", "convert", "-O", "qcow2", source, dest); err != nil {
		_ = os.Remove(dest)
		return failResult("复制系统盘失败: " + err.Error()), nil
	}
	if err := ensureQemuReadable(dest); err != nil {
		_ = os.Remove(dest)
		return failResult("设置模板盘属主失败: " + err.Error()), nil
	}
	ensureAppArmorAllowsImages(templatesDir)

	report(Stage{Key: "verify", Name: "校验模板盘", Index: 3, Total: 3})
	size, err := sys.QemuImgSize(ctx, dest)
	if err != nil {
		_ = os.Remove(dest)
		return failResult("校验模板盘失败: " + err.Error()), nil
	}
	backing, err := qemuImgBacking(ctx, dest)
	if err != nil || backing != "" {
		_ = os.Remove(dest)
		return failResult("模板盘仍依赖父盘，制备结果不可用"), nil
	}
	const gib = int64(1024 * 1024 * 1024)
	return &agent.Result{
		Success: true,
		Message: "模板已制备",
		Data: map[string]any{agent.TemplateDataKey: agent.TemplateInfo{
			DiskPath: dest,
			SizeGB:   int((size + gib - 1) / gib),
			Format:   "qcow2",
		}},
	}, nil
}

// vmTemplateDelete 实现 template.delete：批量删除模板盘。
//
// 只允许删 templatesDir 下的文件：控制面传的是路径，而路径是字符串——
// 不设边界的话，一个拼错的参数就能删掉宿主机上的任意文件。
func vmTemplateDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "remove_files", Name: "删除模板盘", Index: 1, Total: 2})
	paths := templateDiskPaths(op.Params)
	if len(paths) == 0 {
		return failResult("缺少要删除的模板盘路径"), nil
	}

	var failed []string
	for _, p := range paths {
		clean := filepath.Clean(p)
		if clean != templatesDir && !strings.HasPrefix(clean, templatesDir+"/") {
			return failResult("拒绝删除模板目录之外的路径: " + clean), nil
		}
		if err := os.Remove(clean); err != nil && !os.IsNotExist(err) {
			failed = append(failed, fmt.Sprintf("%s: %v", clean, err))
			continue
		}
	}

	report(Stage{Key: "verify", Name: "确认文件已删除", Index: 2, Total: 2})
	if len(failed) > 0 {
		return failResult("部分模板盘删除失败: " + strings.Join(failed, "；")), nil
	}
	return &agent.Result{Success: true, Message: "模板盘已删除"}, nil
}

// templateDiskPaths 收集 disk_paths（优先）与单个 disk_path 参数。
func templateDiskPaths(params map[string]any) []string {
	var out []string
	if raw, ok := params["disk_paths"]; ok {
		switch v := raw.(type) {
		case []string:
			out = append(out, v...)
		case []any:
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
		}
	}
	if len(out) == 0 {
		if s, ok := params["disk_path"].(string); ok && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
