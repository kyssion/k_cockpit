// ex_template_maintain.go：模板派生链维护与离线预处理。
package nodeagent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/nodeagent/sys"
)

// templateRewriteTimeout 给 rebase / convert 这类改写整盘的命令。
const templateRewriteTimeout = 6 * time.Hour

// vmTemplateRebase 实现 template.rebase：把当前模板的 backing 切到上级。
func vmTemplateRebase(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	parent := strParam(op.Params, "parent_disk_path")
	if parent == "" {
		return failResult("没有上级模板盘，无法 rebase"), nil
	}
	return templateReparent(ctx, report, op.Target, parent, 1, "已把 backing 切换到上级")
}

// vmTemplateFlatten 实现 template.flatten：把当前模板拉平到上级（无上级则拉成独立盘）。
func vmTemplateFlatten(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	parent := strParam(op.Params, "parent_disk_path")
	return templateReparent(ctx, report, op.Target, parent, 1, "已拉平模板盘")
}

// vmTemplatePromoteChild 实现 template.promote_child：把子模板改挂到当前模板的上级。
func vmTemplatePromoteChild(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "child_pick", Name: "定位子模板", Index: 1, Total: 2})
	children := stringSliceParam(op.Params, "child_disk_paths")
	if len(children) == 0 {
		return failResult("没有要提升的子模板盘"), nil
	}
	parent := strParam(op.Params, "parent_disk_path")
	var rewritten int64
	for _, child := range children {
		n, err := reparentOne(ctx, child, parent)
		if err != nil {
			return failResult(fmt.Sprintf("子模板 %s 改挂失败: %v", child, err)), nil
		}
		rewritten += n
	}
	report(Stage{Key: "backing_switch", Name: "改挂到上一级", Index: 2, Total: 2})
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.TemplateMaintainDataKey: agent.TemplateMaintainInfo{
			Affected: len(children), RewrittenBytes: rewritten,
			Message: "子模板已提升一级",
		}},
	}, nil
}

// vmTemplatePromoteDelete 实现 template.promote_delete：先把下游改挂到上级，再删当前这一代。
func vmTemplatePromoteDelete(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	report(Stage{Key: "children_scan", Name: "扫描下游模板", Index: 1, Total: 3})
	children := stringSliceParam(op.Params, "child_disk_paths")
	parent := strParam(op.Params, "parent_disk_path")
	var rewritten int64
	for _, child := range children {
		n, err := reparentOne(ctx, child, parent)
		if err != nil {
			return failResult(fmt.Sprintf("下游模板 %s 改挂失败: %v", child, err)), nil
		}
		rewritten += n
	}
	report(Stage{Key: "backing_reparent", Name: "把下游改挂到上级", Index: 2, Total: 3})

	if err := requireUnder(templatesDir, op.Target); err != nil {
		return failResult("拒绝删除模板目录之外的路径: " + op.Target), nil
	}
	report(Stage{Key: "template_remove", Name: "删除这一代", Index: 3, Total: 3})
	if err := os.Remove(op.Target); err != nil && !os.IsNotExist(err) {
		return failResult("删除当前模板盘失败: " + err.Error()), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.TemplateMaintainDataKey: agent.TemplateMaintainInfo{
			Affected: len(children) + 1, RewrittenBytes: rewritten,
			Message: "已删除这一代，下游模板已改挂到上级",
		}},
	}, nil
}

// templateReparent 把 target 的 backing 改到 parent；parent 为空则拉平成独立盘。
func templateReparent(
	ctx context.Context, report Reporter, target, parent string, affected int, msg string,
) (*agent.Result, error) {
	report(Stage{Key: "chain_read", Name: "读取派生链", Index: 1, Total: 3})
	if err := requireUnder(templatesDir, target); err != nil {
		return failResult("模板盘路径非法: " + target), nil
	}
	if _, err := os.Stat(target); err != nil {
		return failResult("模板盘不存在: " + target), nil
	}
	if parent != "" {
		if err := requireUnder(templatesDir, parent); err != nil {
			return failResult("上级模板盘路径非法: " + parent), nil
		}
		if _, err := os.Stat(parent); err != nil {
			return failResult("上级模板盘不存在: " + parent), nil
		}
	}

	report(Stage{Key: "diff_rewrite", Name: "把差异写回上级", Index: 2, Total: 3})
	rewritten, err := reparentOne(ctx, target, parent)
	if err != nil {
		return failResult("改写派生链失败: " + err.Error()), nil
	}

	report(Stage{Key: "chain_verify", Name: "校验链完整", Index: 3, Total: 3})
	backing, err := qemuImgBacking(ctx, target)
	if err != nil {
		return failResult("校验派生链失败: " + err.Error()), nil
	}
	if parent == "" && backing != "" {
		return failResult("拉平后仍依赖父盘: " + backing), nil
	}
	if parent != "" && backing != parent {
		return failResult(fmt.Sprintf("backing 未切到目标（当前 %s，期望 %s）", backing, parent)), nil
	}
	return &agent.Result{
		Success: true,
		Data: map[string]any{agent.TemplateMaintainDataKey: agent.TemplateMaintainInfo{
			Affected: affected, RewrittenBytes: rewritten, Message: msg,
		}},
	}, nil
}

// reparentOne 改一块盘的 backing，返回改写字节数（文件大小变化的绝对值）。
func reparentOne(ctx context.Context, target, parent string) (int64, error) {
	before, _ := os.Stat(target)
	if parent == "" {
		if err := flattenStandalone(ctx, target); err != nil {
			return 0, err
		}
	} else {
		// 安全模式（不带 -u）：qemu-img 会把旧 backing 的差异合并进目标，
		// 再切换 backing。用 -u 只改文件名会在数据层面留下空洞。
		if _, err := sys.RunTimeout(ctx, templateRewriteTimeout,
			"qemu-img", "rebase", "-b", parent, "-F", "qcow2", target); err != nil {
			return 0, err
		}
	}
	after, _ := os.Stat(target)
	var delta int64
	if before != nil && after != nil {
		delta = after.Size() - before.Size()
		if delta < 0 {
			delta = -delta
		}
	}
	return delta, nil
}

// flattenStandalone 把一块盘读成不带 backing 的独立 qcow2。
func flattenStandalone(ctx context.Context, target string) error {
	tmp := target + ".kc-flat-" + fmt.Sprint(time.Now().UnixNano())
	_ = os.Remove(tmp)
	if _, err := sys.RunTimeout(ctx, templateRewriteTimeout,
		"qemu-img", "convert", "-O", "qcow2", target, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	backing, err := qemuImgBacking(ctx, tmp)
	if err != nil || backing != "" {
		_ = os.Remove(tmp)
		return fmt.Errorf("拉平后的镜像仍依赖父盘")
	}
	if err := ensureQemuReadable(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// stringSliceParam 读取 []string 或 []any 形式的字符串列表。
func stringSliceParam(params map[string]any, key string) []string {
	raw, ok := params[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// vmTemplatePreprocess 实现 template.preprocess：用 virt-customize 离线改写镜像。
//
// 先备份再改：virt-customize 是原地改写，中途失败会把模板留在半改状态；
// 备份是唯一能保证"失败还能回到原样"的手段。
func vmTemplatePreprocess(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	target := op.Target
	if err := requireUnder(templatesDir, target); err != nil {
		return failResult("模板盘路径非法: " + target), nil
	}
	if _, err := os.Stat(target); err != nil {
		return failResult("模板盘不存在: " + target), nil
	}
	steps := stringSliceParam(op.Params, "steps")
	if len(steps) == 0 {
		return failResult("没有要执行的预处理项"), nil
	}
	sshKey := strings.TrimSpace(strParam(op.Params, "ssh_public_key"))
	for _, s := range steps {
		if s == "inject_ssh_key" && sshKey == "" {
			return failResult("已选择注入 SSH 公钥，但节点未收到公钥内容"), nil
		}
	}
	if _, err := exec.LookPath("virt-customize"); err != nil {
		return &agent.Result{
			Success: true,
			Data: map[string]any{agent.TemplatePreprocessDataKey: agent.TemplatePreprocessInfo{
				Unavailable: "节点缺少 virt-customize（libguestfs-tools），预处理未执行",
			}},
		}, nil
	}

	report(Stage{Key: "image_mount", Name: "挂载镜像到临时环境", Index: 1, Total: 3})
	backup := target + ".kc-preprocess-backup"
	_ = os.Remove(backup)
	before, _ := os.Stat(target)
	if err := copyFile(ctx, target, backup); err != nil {
		return failResult("备份模板盘失败: " + err.Error()), nil
	}
	restore := func() {
		_ = os.Remove(target)
		_ = os.Rename(backup, target)
	}

	args := []string{"-a", target}
	done := make([]string, 0, len(steps))
	for _, step := range steps {
		switch step {
		case "install_agent":
			args = append(args, "--install", "qemu-guest-agent")
			done = append(done, step)
		case "inject_ssh_key":
			keyFile, kerr := os.CreateTemp("", "kc-ssh-key-*.pub")
			if kerr != nil {
				restore()
				return failResult("写入 SSH 公钥临时文件失败: " + kerr.Error()), nil
			}
			if _, werr := keyFile.WriteString(sshKey + "\n"); werr != nil {
				_ = keyFile.Close()
				_ = os.Remove(keyFile.Name())
				restore()
				return failResult("写入 SSH 公钥失败: " + werr.Error()), nil
			}
			_ = keyFile.Close()
			// virt-customize 从文件读取公钥；defer 在函数返回后删除。
			defer os.Remove(keyFile.Name())
			args = append(args, "--ssh-inject", "root:"+keyFile.Name())
			done = append(done, step)
		case "reset_machine_id":
			args = append(args,
				"--run-command", "truncate -s 0 /etc/machine-id",
				"--run-command", "rm -f /var/lib/dbus/machine-id",
			)
			done = append(done, step)
		case "remove_cloud_init":
			args = append(args, "--run-command", "rm -rf /var/lib/cloud/*")
			done = append(done, step)
		default:
			restore()
			return failResult("不支持的预处理项: " + step), nil
		}
	}

	report(Stage{Key: "image_tune", Name: "改写镜像内容", Index: 2, Total: 3})
	if _, err := sys.RunTimeout(ctx, templateRewriteTimeout, "virt-customize", args...); err != nil {
		restore()
		return failResult("预处理失败，已还原原镜像: " + err.Error()), nil
	}
	report(Stage{Key: "image_verify", Name: "校验可启动", Index: 3, Total: 3})
	if _, err := sys.Run(ctx, "qemu-img", "check", target); err != nil {
		restore()
		return failResult("预处理后镜像校验失败，已还原: " + err.Error()), nil
	}
	_ = os.Remove(backup)
	after, _ := os.Stat(target)
	var delta int64
	if before != nil && after != nil {
		delta = after.Size() - before.Size()
	}
	return &agent.Result{
		Success: true,
		Message: "预处理完成",
		Data: map[string]any{agent.TemplatePreprocessDataKey: agent.TemplatePreprocessInfo{
			Steps: done, SizeDeltaBytes: delta, Message: "预处理完成",
		}},
	}, nil
}
