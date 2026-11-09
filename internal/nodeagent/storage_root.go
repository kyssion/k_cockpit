// storage_root.go：把「我的存储」的节点侧根目录变成**可收敛的配置**。
//
// 背景（实机抓出）：根目录此前是写死的 /var/lib/k_cockpit/userdata，落在
// 系统盘上。节点上明明建了 15G 的数据池，上传的 600MB 镜像却写进系统盘——
// 池完全没被用上，而"系统盘被镜像写满"会直接拖垮节点。
//
// 收敛方式与架构里的其它"期望态"一致：控制面算出期望路径（默认存储池的
// <mount>/userdata），通过 storage.root.set 下发；agent 校验后生效并落盘，
// 重启后由 LoadUserdataRoot 读回。没有存储池时保持历史默认值。
package nodeagent

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"k_cockpit/internal/agent"
)

// agentStateDir 是 agent 的状态目录（与身份、证书同处），由 cmd/agent
// 在启动时通过 SetStateDir 指定。
var agentStateDir = "/var/lib/k_cockpit-agent"

// SetStateDir 指定状态目录；空值忽略（保持默认）。
func SetStateDir(dir string) {
	if strings.TrimSpace(dir) != "" {
		agentStateDir = dir
	}
}

func storageRootFile() string { return filepath.Join(agentStateDir, "storage-root") }

// LoadUserdataRoot 在启动时读回上次下发的存储根。
//
// 读不到或校验不通过时保持默认值并记日志：**不因为一份坏配置就不启动**，
// 那会让一台本来还能干活的节点彻底掉线。下一次控制面收敛会自动纠正。
func LoadUserdataRoot() {
	b, err := os.ReadFile(storageRootFile())
	if err != nil {
		return
	}
	p := strings.TrimSpace(string(b))
	if p == "" {
		return
	}
	if err := applyUserdataRoot(p); err != nil {
		log.Printf("[k_cockpit-agent] 存储根配置不可用，继续使用默认 %s: %v",
			defaultUserdataRoot, err)
		return
	}
	log.Printf("[k_cockpit-agent] 用户存储根已加载: %s", p)
}

// applyUserdataRoot 校验并生效（不落盘）。
//
// 校验的重点是"控制面让我们往哪里写"：即使控制面被攻破，也不能把
// 「我的存储」指到 /etc 或 /root——因此只接受 agent 自己管理的池挂载点
// 之下，以及历史默认根。
func applyUserdataRoot(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("存储根必须是绝对路径: %q", path)
	}
	clean := filepath.Clean(path)
	if !allowedUserdataRoot(clean) {
		return fmt.Errorf("存储根必须位于 %s 之下或为 %s: %q",
			poolMountRoot, defaultUserdataRoot, clean)
	}
	if err := os.MkdirAll(clean, 0o755); err != nil {
		return fmt.Errorf("创建存储根失败: %w", err)
	}
	// qemu 以 libvirt-qemu 身份打开 ISO/磁盘，目录必须对它有读权限。
	ensureAppArmorAllowsImages(clean)
	userdataRootValue.Store(clean)
	return nil
}

// userdataAllowedRoots 是允许承载用户存储的根目录白名单。
//
// 默认是 agent 自己管理的池挂载点根与历史默认根；测试会替换它指向临时
// 目录（否则在开发机上没法验证"切换 + 落盘"这条路径）。
var userdataAllowedRoots = []string{poolMountRoot, defaultUserdataRoot}

// allowedUserdataRoot 判断路径是否落在允许的根下。
func allowedUserdataRoot(clean string) bool {
	for _, root := range userdataAllowedRoots {
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return true
		}
	}
	return false
}

// storageRootSet 实现 storage.root.set。
//
// 参数 path 为空表示"没有默认存储池"：回到历史默认根（系统盘）。
// 幂等：值没变时只回读一次当前值，不重复 mkdir。
func storageRootSet(ctx context.Context, op agent.Operation, report Reporter) (*agent.Result, error) {
	path := strings.TrimSpace(strParam(op.Params, "path"))
	if path == "" {
		userdataRootValue.Store(defaultUserdataRoot)
		if err := os.Remove(storageRootFile()); err != nil && !os.IsNotExist(err) {
			log.Printf("[k_cockpit-agent] 清理存储根配置失败: %v", err)
		}
		return &agent.Result{Success: true,
			Data: map[string]any{"root": defaultUserdataRoot}}, nil
	}

	clean := filepath.Clean(path)
	if current := userdataRoot(); current != clean {
		report(Stage{Key: "storage_root", Name: "切换用户存储根", Index: 1, Total: 1})
		if err := applyUserdataRoot(clean); err != nil {
			return failResult("切换用户存储根失败: " + err.Error()), nil
		}
	}

	if err := os.MkdirAll(agentStateDir, 0o755); err != nil {
		return failResult("写入状态目录失败: " + err.Error()), nil
	}
	if err := os.WriteFile(storageRootFile(), []byte(clean+"\n"), 0o600); err != nil {
		return failResult("保存存储根配置失败: " + err.Error()), nil
	}
	return &agent.Result{Success: true,
		Data: map[string]any{"root": clean}}, nil
}
