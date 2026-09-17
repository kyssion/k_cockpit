package userstorage_test

import (
	"context"
	"errors"

	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/userstorage"
)

// fakeQuota 是配额检查的替身，便于精确控制「什么时候超」。
type fakeQuota struct {
	allow bool
	calls int
}

func (f *fakeQuota) Check(context.Context, int64, int64, int64) error {
	f.calls++
	if f.allow {
		return nil
	}
	return api.ValidationFailed("存储配额不足")
}

func newTestEnv(t *testing.T, q *fakeQuota) (*userstorage.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "us.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.StorageFile{}, &model.UploadSession{}, &model.UserStorage{},
		&model.Node{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return userstorage.NewService(db, audit.NewRecorder(db), q), db
}

const (
	userA = int64(7)
	userB = int64(8)
)

func owner(u int64) authz.Viewer { return authz.Viewer{UserID: u} }

// seedFile 直接写一条就绪的文件记录。
func seedFile(t *testing.T, db *gorm.DB, userID int64, filename, digest string) *model.StorageFile {
	t.Helper()
	now := time.Now()
	row := model.StorageFile{
		NodeID: 1, UserID: &userID,
		RelPath: "share/" + filename, Category: model.FileCategoryShare,
		Filename: filename, SizeBytes: 1024, SHA256: &digest, UploadedAt: &now,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("写入文件失败: %v", err)
	}
	return &row
}

// --- 秒传的隐私边界 ---

// TestInstantOnlyMatchesOwnFiles 覆盖秒传最需要小心的一点。
//
// 按摘要**全局**查重会让用户 A 通过「上传 → 秒传成功」推断出用户 B 是否
// 存在某个文件：内容本身读不到，但「它存在」这条信息已经泄漏了——而对某些
// 文件名（一份未公开的合同、一个内部镜像）来说，存在性本身就是敏感信息。
func TestInstantOnlyMatchesOwnFiles(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()
	const digest = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	// B 有这个文件。
	seedFile(t, db, userB, "secret.iso", digest)

	// A 用同一个摘要发起上传 —— 不该秒传。
	view, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso",
		Filename: "guess.iso", TotalSize: 4096, ChunkSize: 1024, SHA256: digest,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("创建上传失败: %v", err)
	}
	if view.Instant {
		t.Fatal("秒传命中了别人的文件——这会让用户能探测别人是否存在某个内容")
	}

	// A 自己先有一个文件时，秒传才成立。
	seedFile(t, db, userA, "mine.iso", digest)
	view, err = svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso2",
		Filename: "copy.iso", TotalSize: 4096, ChunkSize: 1024, SHA256: digest,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("创建上传失败: %v", err)
	}
	if !view.Instant {
		t.Error("自己已有同一内容时应命中秒传")
	}
}

// --- 分片与断点续传 ---

func TestChunkBitmapResume(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	// 10 个分片，只传 2、5、7 三片。
	view, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare, RelDir: "share",
		Filename: "big.bin", TotalSize: 10 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("创建上传失败: %v", err)
	}
	if view.TotalChunks != 10 {
		t.Fatalf("总分片 = %d, 期望 10", view.TotalChunks)
	}

	for _, idx := range []int{2, 5, 7} {
		if _, err := svc.UploadChunk(ctx, userA, view.UploadID, idx, ""); err != nil {
			t.Fatalf("登记分片 %d 失败: %v", idx, err)
		}
	}

	got, err := svc.GetUpload(ctx, userA, view.UploadID)
	if err != nil {
		t.Fatalf("查询会话失败: %v", err)
	}

	// 缺失清单必须**精确到序号**：客户端要据此决定传哪几片，而「还差 7 片」
	// 这句话它无法据以行动，只能重传全部——那正是断点续传要避免的事。
	want := []int{0, 1, 3, 4, 6, 8, 9}
	if !equalInts(got.MissingChunks, want) {
		t.Errorf("缺失分片 = %v, 期望 %v", got.MissingChunks, want)
	}
}

// TestDuplicateChunkIsNotAnError 覆盖网络重试这一常态。
//
// 把重复登记当成错误，会让客户端在一次正常的重试上收到失败，然后放弃整个
// 上传。
func TestDuplicateChunkIsNotAnError(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "dup.bin", TotalSize: 2 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")

	for i := 0; i < 3; i++ {
		if _, err := svc.UploadChunk(ctx, userA, view.UploadID, 0, ""); err != nil {
			t.Fatalf("第 %d 次登记同一分片失败: %v", i+1, err)
		}
	}
}

func TestCompleteRequiresAllChunks(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "partial.bin", TotalSize: 3 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")
	if _, err := svc.UploadChunk(ctx, userA, view.UploadID, 0, ""); err != nil {
		t.Fatalf("登记失败: %v", err)
	}

	_, err := svc.CompleteUpload(ctx, userA, view.UploadID, owner(userA), "alice", "")
	assertStatus(t, err, 422)
	// 只说「还差 N 片」用户无法行动，因此缺口序号要一并给出。
	if err != nil && !strings.Contains(err.Error(), "2 片") {
		t.Errorf("拒绝文案应给出缺口数量: %v", err)
	}
}

func TestCompleteRegistersReadyFile(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso",
		Filename: "ubuntu.iso", TotalSize: 2 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")
	for i := 0; i < 2; i++ {
		if _, err := svc.UploadChunk(ctx, userA, view.UploadID, i, ""); err != nil {
			t.Fatalf("登记分片失败: %v", err)
		}
	}

	file, err := svc.CompleteUpload(ctx, userA, view.UploadID, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("完成上传失败: %v", err)
	}
	if file.Category != model.FileCategoryISO {
		t.Errorf("类别 = %q, 期望 iso（会话的 target 应被继承）", file.Category)
	}
	if file.RelPath != "iso/ubuntu.iso" {
		t.Errorf("相对路径 = %q, 期望 iso/ubuntu.iso", file.RelPath)
	}
	if file.UploadedAt == "" {
		t.Error("完成后应带上传时间")
	}

	// 列表里应当出现。
	files, err := svc.ListFiles(ctx, userA, 1, "")
	if err != nil {
		t.Fatalf("查询文件失败: %v", err)
	}
	if len(files) != 1 || files[0].Filename != "ubuntu.iso" {
		t.Errorf("文件列表 = %+v, 期望一条 ubuntu.iso", files)
	}
}

// TestListHidesPendingUploads 覆盖一处会让用户怀疑下载坏了的细节。
//
// 受理中的上传在 storage_file 里也有一行记录，把「已登记但还没写完」的
// 文件列出来，用户会看到一个大小正常、点下载却没有内容的条目。
func TestListHidesPendingUploads(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "inflight.bin", TotalSize: 2 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")
	_ = view

	files, err := svc.ListFiles(ctx, userA, 1, "")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("内容未就绪的文件不应出现在列表里, 实际 %d 条", len(files))
	}
}

// --- 配额的两次检查 ---

// TestQuotaCheckedAtCreateAndComplete 覆盖「开始时还没超」不能成为永久通行证。
//
// 用户可以先开一个会话、慢慢传几个小时，期间其他操作把空间占满，最后落盘时
// 依然被放行。因此完成时必须**再检查一次**。
func TestQuotaCheckedAtCreateAndComplete(t *testing.T) {
	q := &fakeQuota{allow: true}
	svc, _ := newTestEnv(t, q)
	ctx := context.Background()

	view, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "quota.bin", TotalSize: 2 * 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("创建上传失败: %v", err)
	}
	if q.calls == 0 {
		t.Error("创建会话时应检查配额——否则用户传完 40GB 才被告知超限")
	}

	for i := 0; i < 2; i++ {
		if _, err := svc.UploadChunk(ctx, userA, view.UploadID, i, ""); err != nil {
			t.Fatalf("登记分片失败: %v", err)
		}
	}

	// 传输期间空间被占满。
	q.allow = false
	_, err = svc.CompleteUpload(ctx, userA, view.UploadID, owner(userA), "alice", "")
	assertStatus(t, err, 422)
}

// --- 过期 ---

func TestExpiredSessionIsRejected(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "stale.bin", TotalSize: 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")

	if err := db.Model(&model.UploadSession{}).
		Where("upload_id = ?", view.UploadID).
		Update("expires_at", time.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}

	// 查状态要能区分「过期了」与「从没存在过」——两者的处理方式完全不同，
	// 而用户看到的都只是一个「找不到」。
	got, err := svc.GetUpload(ctx, userA, view.UploadID)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.Status != model.UploadExpired {
		t.Errorf("状态 = %q, 期望 expired", got.Status)
	}
	if len(got.MissingChunks) != 0 {
		t.Error("过期会话不应再给分片清单——那是在让客户端对着一个不可能完成的任务重试")
	}

	_, err = svc.UploadChunk(ctx, userA, view.UploadID, 0, "")
	assertStatus(t, err, 422)
}

// --- 路径边界 ---

func TestFilenameCannotCarryPath(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	// 文件名里的路径分隔符让「文件名」可以夹带目录层级，从而绕过对目录的
	// 那一层校验。
	for _, bad := range []string{"a/b.bin", `a\b.bin`, "..", ".", "a\x00b"} {
		_, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
			NodeID: 1, Category: model.FileCategoryShare,
			Filename: bad, TotalSize: 1024, ChunkSize: 1024,
		}, owner(userA), "alice", "")
		if err == nil {
			t.Errorf("文件名 %q 应被拒绝", bad)
		}
	}
}

func TestDirectoryCannotEscape(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	for _, bad := range []string{"../etc", "/etc", "a/../../b"} {
		_, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
			NodeID: 1, Category: model.FileCategoryShare, RelDir: bad,
			Filename: "x.bin", TotalSize: 1024, ChunkSize: 1024,
		}, owner(userA), "alice", "")
		assertStatus(t, err, 400)
	}
}

func TestTooManyChunksRejected(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	// 1 字节的分片会生成一个天文数字的 bitmap，而它每收到一片都要重写一次。
	_, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "x.bin", TotalSize: 200_000, ChunkSize: 1,
	}, owner(userA), "alice", "")
	assertStatus(t, err, 400)
}

// --- 越权 ---

func TestSessionBelongsToOwner(t *testing.T) {
	svc, _ := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	view, _ := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryShare,
		Filename: "private.bin", TotalSize: 1024, ChunkSize: 1024,
	}, owner(userA), "alice", "")

	// UploadID 会被客户端长期持有，不校验归属的话任何登录用户都能凭一个
	// 猜到的（或从日志里看到的）id 往别人的上传里塞分片。
	_, err := svc.UploadChunk(ctx, userB, view.UploadID, 0, "")
	assertStatus(t, err, 404)

	_, err = svc.GetUpload(ctx, userB, view.UploadID)
	assertStatus(t, err, 404)
}

func TestDeleteFileRejectsOthers(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	ctx := context.Background()

	file := seedFile(t, db, userB, "b-only.bin", strings.Repeat("a", 64))

	err := svc.DeleteFile(ctx, userA, 1, file.ID, owner(userA), "alice", "")
	assertStatus(t, err, 404)
}

// --- 文件哈希的位序 ---

// TestBitmapBitOrderIsStable 锁定 bitmap 的位序约定。
//
// 位序本身只要自洽就行，但它会被持久化：一旦某个版本用低位表示第 0 片、
// 另一个版本用高位，同一个会话在升级前后会给出**不同的缺失清单**——而
// 客户端只会照着清单重传，两边都以为自己在正常工作。
func TestBitmapBitOrderIsStable(t *testing.T) {
	session := &model.UploadSession{TotalSize: 8, ChunkSize: 1}
	for i := 0; i < 8; i++ {
		if session.Received(i) {
			t.Errorf("初始状态不应有已收到的分片（第 %d 片）", i)
		}
	}
	// 第 0 片对应第 1 个十六进制字符的最高位。
	if !session.MarkReceived(0) {
		t.Fatal("标记第 0 片失败")
	}
	if !session.Received(0) {
		t.Fatal("第 0 片未被记录")
	}
	if session.Received(1) {
		t.Fatal("第 1 片被误记")
	}
	if session.MarkReceived(0) {
		t.Error("重复标记应返回 false——调用方据此跳过落库")
	}
}

// --- 辅助 ---

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}
