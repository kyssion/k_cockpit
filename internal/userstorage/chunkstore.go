package userstorage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"k_cockpit/internal/api"
)

// ChunkStore 是分片上传的**本地暂存区**。
//
// 为什么不直接写到节点的存储根：那是宿主机上的目录，而控制面与节点之间
// 隔着网络。真实实现里字节最终要落到节点上（由 agent 完成），因此这里先把
// 分片收在控制面本地，完成时再整份交给节点。
//
// **每一片独立成一个文件**，而不是按偏移写进同一个大文件。后一种做法需要
// 事先把文件撑到完整大小（稀疏文件），而一个 40GB 的空洞会立刻被磁盘配额
// 统计算进去——用户会看到自己的配额在传完第 1MB 时就被占满，而实际只用了
// 1MB。分片独立成文件既没有这个问题，也天然支持乱序到达与断点续传。
type ChunkStore struct {
	root string
}

// NewChunkStore 构造暂存区并确保目录存在。
func NewChunkStore(root string) (*ChunkStore, error) {
	if root == "" {
		root = filepath.Join("data", "uploads")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("创建上传暂存目录失败: %w", err)
	}
	return &ChunkStore{root: root}, nil
}

// Root 返回暂存区根目录（供日志与排查使用）。
func (c *ChunkStore) Root() string { return c.root }

// uploadIDRe 限定 uploadID 的形状。
//
// **这是必须的一道校验**：uploadID 是客户端带回来的，而它会被拼进文件路径。
// 不校验的话，一个 `../../etc` 形状的 id 就能让服务端在任意位置创建文件。
// 白名单（只允许十六进制）比黑名单（拒绝 `..` 和 `/`）可靠得多——后者要
// 穷举所有可能的逃逸写法，而前者根本不给它表达的余地。
var uploadIDRe = regexp.MustCompile(`^[a-f0-9]{8,64}$`)

// Put 保存一个分片，返回该分片内容的 sha256。
//
// **写入用「先写临时文件、再改名」**：直接写目标文件时，如果进程在中途被杀
// （重启、OOM），会留下一个**半截的分片**——而 bitmap 里没有它，客户端以为
// 该片没传，重传时覆盖掉即可。但如果目标文件已经存在而写入失败，一个半截
// 文件加上一个"已收到"的标记就会让最终文件坏掉，且没有任何地方会发现。
// 改名是原子的，因此磁盘上要么没有这一片，要么是完整的一片。
func (c *ChunkStore) Put(uploadID string, index int, data []byte) (string, error) {
	if err := c.validateID(uploadID); err != nil {
		return "", err
	}
	if index < 0 {
		return "", api.InvalidParameter("分片序号不能为负")
	}

	dir := filepath.Join(c.root, uploadID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("[userstorage] 创建分片目录失败: %v", err)
		return "", api.Internal()
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	final := filepath.Join(dir, strconv.Itoa(index)+".part")
	tmp := final + ".tmp"

	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[userstorage] 写入分片失败: %v", err)
		return "", api.Internal()
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		log.Printf("[userstorage] 提交分片失败: %v", err)
		return "", api.Internal()
	}
	return digest, nil
}

// Has 报告某个分片是否已经落盘。
func (c *ChunkStore) Has(uploadID string, index int) bool {
	if err := c.validateID(uploadID); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(c.root, uploadID, strconv.Itoa(index)+".part"))
	return err == nil
}

// Assemble 把所有分片按序号拼成一份完整内容，写到 dst。
//
// **流式拼接**，不把整个文件读进内存：一个 40GB 的镜像不可能放进内存，
// 而把它读进内存再写出去的做法在测试里（几 KB）看不出问题，只有在真实
// 文件上才会 OOM。返回拼接后的 sha256。
func (c *ChunkStore) Assemble(uploadID string, totalChunks int, dst io.Writer) (string, error) {
	if err := c.validateID(uploadID); err != nil {
		return "", err
	}
	if totalChunks <= 0 {
		return "", api.InvalidParameter("分片数必须大于 0")
	}

	hasher := sha256.New()
	out := io.MultiWriter(dst, hasher)

	for i := 0; i < totalChunks; i++ {
		path := filepath.Join(c.root, uploadID, strconv.Itoa(i)+".part")
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// 缺片是一个**明确的错误**而不是"跳过"：静默跳过会产出一份
				// 比预期短的文件，而它看起来完全正常，直到被拿去装系统。
				return "", api.ValidationFailed(
					"分片 " + strconv.Itoa(i) + " 缺失，无法拼接")
			}
			log.Printf("[userstorage] 打开分片失败: %v", err)
			return "", api.Internal()
		}
		_, copyErr := io.Copy(out, f)
		closeErr := f.Close()
		if copyErr != nil || closeErr != nil {
			log.Printf("[userstorage] 拼接分片 %d 失败: %v / %v", i, copyErr, closeErr)
			return "", api.Internal()
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// Discard 丢弃一次上传的全部分片。
//
// 在**失败路径与过期清理**上都要调用它：分片占的是真实磁盘空间，而一个
// 再也不会回来的客户端留下的分片会一直占着——直到有人发现磁盘满了，
// 而那时已经很难查到是谁留下的。
func (c *ChunkStore) Discard(uploadID string) error {
	if err := c.validateID(uploadID); err != nil {
		// 形状不合法时**什么都不做**：路径不合法意味着它从来不是我们创建的
		// 目录，去删它反而是危险的。
		return nil
	}
	dir := filepath.Join(c.root, uploadID)
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("[userstorage] 清理分片目录失败 %s: %v", uploadID, err)
		return err
	}
	return nil
}

// SizeOf 返回某次上传已占用的字节数（供排查与配额核算参考）。
func (c *ChunkStore) SizeOf(uploadID string) int64 {
	if err := c.validateID(uploadID); err != nil {
		return 0
	}
	entries, err := os.ReadDir(filepath.Join(c.root, uploadID))
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// validateID 校验 uploadID 的形状（见 uploadIDRe 的说明）。
func (c *ChunkStore) validateID(id string) error {
	if !uploadIDRe.MatchString(id) {
		return api.InvalidParameter("上传标识不合法")
	}
	return nil
}
