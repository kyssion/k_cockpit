// Package sys 封装 agent 侧的系统探测：把 lsblk / virsh / ip / ovs-vsctl /
// /proc 这些异构来源收敛成结构化数据。
//
// 设计约束：
//   - **命令输出只进解析器，不进业务**：每个解析器吃 []byte 出结构体，
//     用真机脱敏后的 fixture 做单元测试（testdata/）——探测命令的输出格式
//     稳定，解析错误在这里暴露，而不是在执行器里悄悄产生零值；
//   - 命令路径不写死：依赖 PATH 解析（agent 以 root systemd 运行，PATH 由
//     unit 给出），发行版差异收敛在各 helper 内部；
//   - 全部只读；写操作（P2+）另起文件，不与探测混放。
package sys

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cmdTimeout 是单条探测命令的上限。
//
// 探测命令都是毫秒级（lsblk/virsh/ovs-vsctl）；给 10s 是对慢磁盘上的
// libvirt 读操作的宽容。超时的表现是该次探测失败（Result 失败），而不是
// 把 agent 卡住。
const cmdTimeout = 10 * time.Second

// Run 执行一条探测命令并返回标准输出。失败时把 stderr 附进错误——
// virsh/define 这类命令的失败原因只在 stderr 里（"unsupported configuration
// ..."），不带上的话节点侧只剩一个空泛的 exit status。
func Run(ctx context.Context, name string, args ...string) (string, error) {
	return RunTimeout(ctx, cmdTimeout, name, args...)
}

// RunTimeout 是 Run 的可控超时版本，给 qemu-img convert / blockcopy 这类
// 分钟级甚至小时级的操作使用。
//
// timeout <= 0 表示不额外加超时，完全由调用方 ctx 控制（任务取消时命令
// 会随 ctx 一起被终止）。
func RunTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("%s: %s", name, msg)
		}
		return "", fmt.Errorf("%s %v: %w", name, args, err)
	}
	return string(out), nil
}

// RunOK 执行命令，只关心成功与否（不取输出）。
func RunOK(ctx context.Context, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Run() == nil
}

// Run0Timeout 是 Run0 的可控超时版本（见 RunTimeout）。
func Run0Timeout(ctx context.Context, timeout time.Duration, name string, args ...string) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var stderr strings.Builder
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %s", name, msg)
		}
		return err
	}
	return nil
}

// jsonUnmarshalImpl 是 jsonUnmarshal 的实现（独立函数便于替换/打桩）。
func jsonUnmarshalImpl(b []byte, v any) error { return json.Unmarshal(b, v) }

// osReadFile 读小文件为字符串。
func osReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Run0 执行命令只返回错误（不看输出）——给「成功与否即结论」的写命令用。
func Run0(ctx context.Context, name string, args ...string) error {
	return Run0Timeout(ctx, cmdTimeout, name, args...)
}
