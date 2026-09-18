package userstorage_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k_cockpit/internal/userstorage"
)

const validID = "a1b2c3d4e5f60718"

func newStore(t *testing.T) *userstorage.ChunkStore {
	t.Helper()
	store, err := userstorage.NewChunkStore(filepath.Join(t.TempDir(), "uploads"))
	if err != nil {
		t.Fatalf("创建暂存区失败: %v", err)
	}
	return store
}

// TestPutReturnsDigest 覆盖分片摘要的计算。
func TestPutReturnsDigest(t *testing.T) {
	store := newStore(t)
	data := []byte("hello chunk")

	got, err := store.Put(validID, 0, data)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	sum := sha256.Sum256(data)
	if got != hex.EncodeToString(sum[:]) {
		t.Error("返回的摘要不是该分片内容的 sha256")
	}
	if !store.Has(validID, 0) {
		t.Error("分片应已落盘")
	}
}

// TestRejectsMaliciousUploadID 是暂存区**最重要的一条**。
//
// uploadID 是客户端带回来的，而它会被拼进文件路径。不校验的话，一个
// `../../etc/x` 形状的 id 就能让服务端在任意位置创建文件。
//
// 用白名单（只允许十六进制）而不是黑名单（拒绝 `..` 和 `/`）：后者要穷举
// 所有可能的逃逸写法，而前者根本不给它表达的余地。
func TestRejectsMaliciousUploadID(t *testing.T) {
	store := newStore(t)

	for _, bad := range []string{
		"../../etc/passwd",
		"..%2f..%2fetc",
		"abc/def",
		"a\\b",
		"",
		"short",
		strings.Repeat("z", 70),
		"ABCDEF0123456789", // 大写不被白名单接受
	} {
		if _, err := store.Put(bad, 0, []byte("x")); err == nil {
			t.Errorf("非法 uploadID %q 应被拒绝", bad)
		}
		// Has / Discard 也不该因为非法 id 而误操作真实路径。
		store.Discard(bad)
		if store.Has(bad, 0) {
			t.Errorf("非法 id %q 不该有分片", bad)
		}
	}

	// 确认没有在暂存区之外创建任何东西。
	if _, err := os.Stat(filepath.Join(store.Root(), "..", "etc")); err == nil {
		t.Fatal("发生了路径逃逸")
	}
}

// TestChunksAreSeparateFiles 覆盖一处会影响配额统计的设计。
//
// **每一片独立成一个文件**，而不是按偏移写进同一个大文件。后一种做法需要
// 事先把文件撑到完整大小（稀疏文件），而一个 40GB 的空洞会立刻被磁盘配额
// 统计算进去——用户会看到配额在传完第 1MB 时就被占满，而实际只用了 1MB。
func TestChunksAreSeparateFiles(t *testing.T) {
	store := newStore(t)

	// 模拟"总共 40GB、只传了第 0 片"的情形。
	data := bytes.Repeat([]byte("x"), 1024)
	if _, err := store.Put(validID, 0, data); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 实际占用必须接近 1KB，而不是 40GB。
	size := store.SizeOf(validID)
	if size > 64*1024 {
		t.Errorf("已占用 %d 字节——像是预分配了完整大小", size)
	}
	if size < 1024 {
		t.Errorf("已占用 %d 字节，应至少包含那一片", size)
	}
}

// TestAssembleInOrder 覆盖按序拼接。
func TestAssembleInOrder(t *testing.T) {
	store := newStore(t)

	// **乱序写入**（网络下这是常态）。
	parts := map[int][]byte{2: []byte("cc"), 0: []byte("aa"), 1: []byte("bb")}
	for idx, data := range parts {
		if _, err := store.Put(validID, idx, data); err != nil {
			t.Fatalf("保存分片 %d 失败: %v", idx, err)
		}
	}

	var buf bytes.Buffer
	sum, err := store.Assemble(validID, 3, &buf)
	if err != nil {
		t.Fatalf("拼接失败: %v", err)
	}
	if buf.String() != "aabbcc" {
		t.Errorf("拼接结果 = %q, 期望 aabbcc", buf.String())
	}

	want := sha256.Sum256([]byte("aabbcc"))
	if sum != hex.EncodeToString(want[:]) {
		t.Error("拼接返回的摘要不对——它会被用来与节点回传的值比对")
	}
}

// TestAssembleFailsOnMissingChunk 覆盖「缺片不静默跳过」。
//
// 静默跳过会产出一份比预期短的文件，而它看起来完全正常，直到被拿去装系统。
func TestAssembleFailsOnMissingChunk(t *testing.T) {
	store := newStore(t)

	if _, err := store.Put(validID, 0, []byte("aa")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	// 缺第 1 片。
	if _, err := store.Put(validID, 2, []byte("cc")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	var buf bytes.Buffer
	_, err := store.Assemble(validID, 3, &buf)
	if err == nil {
		t.Fatal("缺片时必须报错，而不是产出一份更短的文件")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("应指出缺的是哪一片: %v", err)
	}
}

// TestOverwriteChunkIsSafe 覆盖重传。
//
// 网络重试是常态，重传同一片必须得到正确结果（而不是重复追加）。
func TestOverwriteChunkIsSafe(t *testing.T) {
	store := newStore(t)

	if _, err := store.Put(validID, 0, []byte("old")); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if _, err := store.Put(validID, 0, []byte("new")); err != nil {
		t.Fatalf("重传失败: %v", err)
	}

	var buf bytes.Buffer
	if _, err := store.Assemble(validID, 1, &buf); err != nil {
		t.Fatalf("拼接失败: %v", err)
	}
	if buf.String() != "new" {
		t.Errorf("拼接结果 = %q, 期望 new（重传应覆盖，不是追加）", buf.String())
	}
}

// TestDiscardRemovesEverything 覆盖清理。
//
// 分片占的是真实磁盘空间，而一个再也不会回来的客户端留下的分片会一直占着
// ——直到有人发现磁盘满了，而那时已经很难查到是谁留下的。
func TestDiscardRemovesEverything(t *testing.T) {
	store := newStore(t)

	for i := 0; i < 3; i++ {
		if _, err := store.Put(validID, i, []byte("data")); err != nil {
			t.Fatalf("保存失败: %v", err)
		}
	}
	if err := store.Discard(validID); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if store.SizeOf(validID) != 0 {
		t.Error("清理后仍占用空间")
	}
	// 重复清理不该报错（幂等）。
	if err := store.Discard(validID); err != nil {
		t.Errorf("重复清理应幂等: %v", err)
	}
}

// TestNoPartialChunkAfterFailure 覆盖「先写临时、再改名」的意义。
//
// 直接写目标文件时，如果进程在中途被杀，会留下一个**半截的分片**——而
// 如果那时 bitmap 已经标记它收到了，最终文件就会坏掉，且没有任何地方会发现。
func TestNoPartialChunkAfterFailure(t *testing.T) {
	store := newStore(t)

	// 正常写入后，暂存目录里不该留下 .tmp 文件。
	if _, err := store.Put(validID, 0, bytes.Repeat([]byte("x"), 4096)); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), validID))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("残留了临时文件 %s——改名应当是原子的", e.Name())
		}
	}
}
