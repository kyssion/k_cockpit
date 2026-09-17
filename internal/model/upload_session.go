package model

import (
	"strings"
	"time"
)

// 上传会话的状态。
const (
	// UploadPending 会话已创建，等待分片。
	UploadPending = "pending"
	// UploadCompleted 全部分片到齐并已合并。
	UploadCompleted = "completed"
	// UploadExpired 会话超时未完成。
	//
	// 它**不代表失败**：用户之后还可以重新发起一次上传。之所以要有一个
	// 明确的状态而不是直接删记录，是因为界面需要能区分「你要找的这次
	// 上传从没存在过」（404）与「它过期了，重传即可」——两者的处理方式
	// 完全不同，而用户看到的都只是一个「找不到」。
	UploadExpired = "expired"
)

// UploadSession 对应 upload_session 表：一次分片上传的状态机（F-5-04）。
//
// 这张表存在的原因是大文件：一个 40GB 的镜像不可能一次请求传完，而网络
// 中断是常态而不是异常。会话把「已经收到了哪些分片」变成**可持久化的
// 事实**，于是断点续传只是「问服务端我还要传哪几片」。
type UploadSession struct {
	// UploadID 是主键，由服务端生成。
	//
	// 不用自增 id 而用随机串：它会被客户端持有并在重连时带回来，是可被
	// 猜测的凭据。自增 id 让任何登录用户都能通过遍历别人的会话上传分片。
	UploadID string `gorm:"primaryKey;size:64"`

	// Target 说明这次上传最终要变成什么（storage_file / image_import 等）。
	//
	// 必须先声明而不是上传完再决定：目标是**配额核算的输入**，而配额要在
	// 授予上传之前检查——一个 40GB 的上传传到一半才被告知超配额，用户
	// 已经等了几十分钟。
	Target string `gorm:"size:32;not null"`

	// FileKey 是这次上传的目标标识（如目标相对路径），全局唯一。
	//
	// 唯一性防的是**两次上传写同一个目标**：并发时两边都以为自己在写，
	// 最后落盘的内容是两者交错的片段——而两边都会收到「成功」。
	FileKey string `gorm:"size:255;not null;uniqueIndex:uniq_upload_session_file_key"`

	OwnerID *int64 `gorm:"index:idx_upload_session_owner_id"`
	NodeID  *int64

	TotalSize int64 `gorm:"not null;default:0"`
	ChunkSize int   `gorm:"not null;default:0"`

	// ReceivedBitmap 用十六进制字符记录已收到的分片，**一位一个分片**。
	//
	// 一个 4MB 分片切 40GB 文件是 10240 片，用 hex 表示是 2560 字节——
	// 存成 JSON 数组要几十 KB，而它每收到一片就要重写一次。
	//
	// 顺序按分片序号从左到右，第 N 片的位在第 N 个十六进制位上。
	ReceivedBitmap *string `gorm:"type:text"`

	SHA256 *string `gorm:"size:64"`
	Status string  `gorm:"size:16;not null;default:pending"`

	// ExpiresAt 是会话的失效时刻。
	//
	// 必须有它：未完成的分片在节点上占着真实空间，而一个再也不会回来的
	// 客户端留下的分片会一直占着。过期之后那些分片由节点清理。
	ExpiresAt time.Time `gorm:"not null;index:idx_upload_session_expires_at"`

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (UploadSession) TableName() string { return "upload_session" }

// TotalChunks 返回总片数；参数不完整时返回 0。
func (s *UploadSession) TotalChunks() int {
	if s.ChunkSize <= 0 || s.TotalSize <= 0 {
		return 0
	}
	n := s.TotalSize / int64(s.ChunkSize)
	if s.TotalSize%int64(s.ChunkSize) != 0 {
		n++
	}
	return int(n)
}

// Received 报告第 index 片（从 0 开始）是否已收到。
func (s *UploadSession) Received(index int) bool {
	bitmap := s.bitmapString()
	if index < 0 || index >= len(bitmap)*4 {
		return false
	}
	// 一个十六进制位表示 4 个分片：先定位到字符，再定位到字符内的位。
	digit := bitmap[index/4]
	v := hexValue(digit)
	if v < 0 {
		return false
	}
	// 一个十六进制位表示 4 个分片，高位对应序号小的那片。
	return v&(1<<uint(3-index%4)) != 0
}

// MarkReceived 记录第 index 片已收到，返回是否发生了**变化**。
//
// 返回是否变化而不是直接写入，是因为调用方要据此决定要不要落库：
// 重复上传同一分片是**常态**（网络重试），每次都写一次库会让一个
// 上万片的上传产生上万次无意义的写入。
func (s *UploadSession) MarkReceived(index int) bool {
	if s.Received(index) {
		return false
	}
	total := s.TotalChunks()
	if total <= 0 || index < 0 || index >= total {
		return false
	}

	bits := []byte(s.bitmapString())
	needDigits := (total + 3) / 4
	if len(bits) < needDigits {
		padded := make([]byte, needDigits)
		for i := range padded {
			padded[i] = '0'
		}
		copy(padded, bits)
		bits = padded
	}
	v := hexValue(bits[index/4])
	if v < 0 {
		v = 0
	}
	v |= 1 << uint(3-index%4)
	bits[index/4] = hexDigit(v)

	out := string(bits)
	s.ReceivedBitmap = &out
	return true
}

// MissingChunks 返回尚未收到的分片序号，供客户端续传。
//
// 返回**序号列表**而不是只给一个「还差 N 片」：客户端要据此决定传哪几片，
// 而「还差 3 片」这句话它无法据以行动，只能重传全部——那正是断点续传要
// 避免的事。
func (s *UploadSession) MissingChunks() []int {
	total := s.TotalChunks()
	out := make([]int, 0, total)
	for i := 0; i < total; i++ {
		if !s.Received(i) {
			out = append(out, i)
		}
	}
	return out
}

// IsComplete 报告全部分片是否都已收到。
func (s *UploadSession) IsComplete() bool {
	total := s.TotalChunks()
	if total == 0 {
		return false
	}
	for i := 0; i < total; i++ {
		if !s.Received(i) {
			return false
		}
	}
	return true
}

// Expired 报告会话在给定时刻是否已过期。
func (s *UploadSession) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// bitmapString 取当前 bitmap 的规范化字符串（清理空白与非法字符）。
func (s *UploadSession) bitmapString() string {
	if s.ReceivedBitmap == nil {
		return ""
	}
	return strings.TrimSpace(*s.ReceivedBitmap)
}

func hexValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

func hexDigit(v int) byte {
	if v < 10 {
		return byte('0' + v)
	}
	return byte('a' + v - 10)
}
