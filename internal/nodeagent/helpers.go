// helpers.go：执行器共用的小工具。
package nodeagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"time"

	"k_cockpit/internal/agent"
)

// failResult 构造一次「送达但失败」的结果（Result 语义，不是 error）。
func failResult(msg string) *agent.Result {
	return &agent.Result{Success: false, Message: msg}
}

// osRemove / osStat / osWriteFile / contextWithTimeout / timeSecond 是
// nodeagent 内部使用的小助手（集中在 helpers，避免各文件重复 import）。
func osRemove(path string) error { return os.Remove(path) }

func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

func osWriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func osReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func osMkdirAllErr(path string) error { return os.MkdirAll(path, 0o755) }

func osRename(src, dst string) error { return os.Rename(src, dst) }

func osRemoveAll(path string) error { return os.RemoveAll(path) }

func osReadFileBytes(path string) ([]byte, error) { return os.ReadFile(path) }

func osReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }

func osAppendFile(path string, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(line)
	return err
}

func sha256OfFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// osLink 建立硬链接（秒传落地的首选方式）。
func osLink(oldPath, newPath string) error { return os.Link(oldPath, newPath) }

// copyFileSync 复制文件并 fsync（硬链接不可用时的回退）。
//
// 必须 fsync：秒传完成后调用方立刻就会返回"已上传"，而记录一旦写下，
// 用户就可能去读这个文件。不落盘的复制在断电/崩溃后会变成一个**长度为 0
// 的"成功"文件**——那比失败难查得多。用 O_EXCL 建目标：绝不覆盖已有文件。
func copyFileSync(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	return out.Close()
}
