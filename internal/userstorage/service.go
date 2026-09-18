// Package userstorage 实现用户存储空间、文件管理与分片上传
// （F-5-03 / F-5-04 / F-5-05）。
//
// 三处贯穿本包的设计：
//
//  1. **路径一律相对于用户存储根**。存绝对路径意味着存储根一旦变更
//     （换盘、迁移、重建用户空间），库里所有记录都会指向一个不存在的位置，
//     而它们看起来完全正常。
//
//  2. **秒传的查重范围限定在同一用户内**。按摘要全局查重会让用户 A 通过
//     「上传 → 秒传成功」推断出用户 B 是否存在某个文件——内容读不到，
//     但「它存在」这条信息已经泄漏了。
//
//  3. **配额要在两处检查**：授予上传之前（否则用户传完 40GB 才被告知超限）
//     和**完成之时**（否则「开始传时还没超」就成了一个永久有效的通行证）。
package userstorage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供用户存储能力。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	// quota 用于检查配额。为 nil 时不做检查（仅测试环境）。
	quota quotaChecker
	// chunks 是分片暂存区；为 nil 时**不接受分片字节**（只跑状态机）。
	chunks *ChunkStore
	// agent 用于把拼好的文件交给节点；为 nil 时跳过下发。
	agent agentClient
	now   func() time.Time
}

// agentClient 是本包对 agent 的最小依赖。
type agentClient interface {
	Execute(ctx context.Context, op agent.Operation) (*agent.Result, error)
}

// WithChunks 挂上分片暂存区。
//
// 用链式设置而不是塞进构造函数：现有的调用点（尤其是测试）不需要为了一个
// 可选能力改动签名——而"不挂暂存区"本身是一个有意义的状态（只跑状态机）。
func (s *Service) WithChunks(c *ChunkStore) *Service {
	s.chunks = c
	return s
}

// WithAgent 挂上节点客户端。
func (s *Service) WithAgent(a agentClient) *Service {
	s.agent = a
	return s
}

// quotaChecker 是本包对配额服务的**最小依赖**。
//
// 声明成接口而不是直接持有 quota.Service：本包的测试只需要一个能返回
// 超限/放行的替身，而引入整个配额服务的构造依赖（它又要数据库、设置……）
// 会让每个用例都背上一串无关的装配。
type quotaChecker interface {
	Check(ctx context.Context, userID, nodeID int64, estimatedBytes int64) error
}

// NewService 构造用户存储服务。
func NewService(db *gorm.DB, recorder *audit.Recorder, checker quotaChecker) *Service {
	return &Service{db: db, audit: recorder, quota: checker, now: time.Now}
}

// 会话有效期。取 24 小时：足够一次跨夜的续传，又不会让废弃的分片
// 在节点上停留太久（它们占着真实空间）。
const sessionTTL = 24 * time.Hour

// StorageView 是用户存储空间的对外视图。
type StorageView struct {
	NodeID int64 `json:"node_id"`
	// Enabled 表示已开通。未开通时下面的字段无意义，界面应先引导开通。
	Enabled bool `json:"enabled"`
	// RelRoot 是存储根的**相对标识**，供界面提示用；不暴露宿主机绝对路径。
	RelRoot string `json:"rel_root,omitempty"`
	// InitializedAt 为空表示已开通但节点还没建好目录。
	InitializedAt *time.Time `json:"initialized_at,omitempty"`
	ReadOnly      bool       `json:"read_only"`
}

// FileView 是一个文件的对外视图。
type FileView struct {
	ID        int64  `json:"id"`
	Category  string `json:"category"`
	Filename  string `json:"filename"`
	RelPath   string `json:"rel_path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256,omitempty"`

	OsType    string `json:"os_type,omitempty"`
	OsVariant string `json:"os_variant,omitempty"`
	MinDiskGB int    `json:"min_disk_gb,omitempty"`

	UploadedAt string `json:"uploaded_at,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// Get 返回用户在指定节点上的存储空间。
func (s *Service) Get(ctx context.Context, userID, nodeID int64) (*StorageView, error) {
	us, err := s.loadStorage(ctx, userID, nodeID)
	if err != nil {
		return nil, err
	}
	view := &StorageView{NodeID: nodeID, Enabled: us.Enabled, ReadOnly: us.ReadOnly}
	if us.RootPath != nil {
		// 只回一个标识而不是绝对路径：界面需要的只是「空间在哪开通的」这个
		// 概念，而宿主机目录结构对用户没有用处，回传它只是把内部布局暴露出去。
		view.RelRoot = path.Base(*us.RootPath)
	}
	view.InitializedAt = us.InitializedAt
	return view, nil
}

// Ensure 开通存储空间（F-5-03：未开通时先「开通」并分配配额）。
//
// **配额为 0（不限）时也允许开通**：开通与限额是两件事，先有空间再有额度
// 是正常的操作顺序。强行要求先设额度才能开通，会让管理员在做任何事之前
// 必须先在另一个页面填一个数字。
func (s *Service) Ensure(
	ctx context.Context, userID, nodeID int64, v authz.Viewer, operatorName, clientIP string,
) (*StorageView, error) {
	if !v.IsAdmin && v.UserID != userID {
		// 404 而非 403：403 会确认「这个用户存在」。
		return nil, api.NotFound("用户不存在")
	}
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}

	us, err := s.loadStorage(ctx, userID, nodeID)
	if err != nil {
		return nil, err
	}
	if us.Enabled {
		// 已开通时直接返回，不重复写库也不重复记审计——与软锁、维护模式
		// 的幂等处理一致：一串同一分钟的记录会让事后追查说不清是哪一次
		// 真正生效的。
		view := &StorageView{NodeID: nodeID, Enabled: true, ReadOnly: us.ReadOnly}
		view.InitializedAt = us.InitializedAt
		if us.RootPath != nil {
			view.RelRoot = path.Base(*us.RootPath)
		}
		return view, nil
	}

	us.Enabled = true
	if err := s.db.WithContext(ctx).Save(us).Error; err != nil {
		log.Printf("[userstorage] 开通存储失败 user=%d node=%d: %v", userID, nodeID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: nodeID, ResourceType: "user_storage", ResourceID: us.ID,
		ResourceName: "user:" + strconv.FormatInt(userID, 10),
		Action:       "user_storage.ensure",
		Success:      true, ClientIP: clientIP,
	})

	return &StorageView{NodeID: nodeID, Enabled: true, ReadOnly: us.ReadOnly}, nil
}

// ListFiles 返回用户的文件列表（F-5-05）。
//
// **只返回内容已就绪的文件**：受理中的上传在 storage_file 里也有一行记录
// （见 CompleteUpload），把「已登记但还没写完」的文件列出来，用户会看到
// 一个大小正常、点下载却没有内容的条目——而他会先怀疑是下载坏了。
func (s *Service) ListFiles(
	ctx context.Context, userID, nodeID int64, category string,
) ([]FileView, error) {
	query := s.db.WithContext(ctx).Model(&model.StorageFile{}).
		Where("node_id = ? AND uploaded_at IS NOT NULL", nodeID).
		Where("user_id = ?", userID).
		Order("uploaded_at DESC")
	if category != "" {
		if !model.ValidFileCategory(category) {
			return nil, api.InvalidParameter("不支持的文件类别：" + category)
		}
		query = query.Where("category = ?", category)
	}

	var rows []model.StorageFile
	if err := query.Find(&rows).Error; err != nil {
		log.Printf("[userstorage] 查询文件失败: %v", err)
		return nil, api.Internal()
	}

	views := make([]FileView, 0, len(rows))
	for i := range rows {
		views = append(views, toFileView(&rows[i]))
	}
	return views, nil
}

// DeleteFile 删除一个文件（F-5-05：高风险）。
//
// **不需要二次验证**：删除的是用户自己的文件，而且它随时可以重新上传。
// 与其他「不可逆」操作的区别在于——这里的不可逆只对**这一次**成立，
// 内容本身在用户手上。
func (s *Service) DeleteFile(
	ctx context.Context, userID, nodeID, fileID int64, v authz.Viewer,
	operatorName, clientIP string,
) error {
	file, err := s.loadFile(ctx, fileID)
	if err != nil {
		return err
	}
	if file.NodeID != nodeID {
		return api.NotFound("文件不存在")
	}
	if !v.IsAdmin && (file.UserID == nil || *file.UserID != userID) {
		// 404 而非 403：403 会让租户通过枚举推断出别人有哪些文件。
		return api.NotFound("文件不存在")
	}

	if err := s.db.WithContext(ctx).Delete(&model.StorageFile{}, file.ID).Error; err != nil {
		log.Printf("[userstorage] 删除文件失败 id=%d: %v", file.ID, err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: file.NodeID, ResourceType: "storage_file", ResourceID: file.ID,
		ResourceName: file.Filename, Action: "storage_file.delete",
		Params:  map[string]any{"rel_path": file.RelPath, "size": file.SizeBytes},
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// --- 上传会话（F-5-04）---

// CreateUploadRequest 是创建上传会话的请求。
//
// Category 同时决定会话的 Target——见 CreateUpload 里的说明。
type CreateUploadRequest struct {
	NodeID   int64
	Category string
	// RelDir 是目标目录（相对存储根），例如 "iso"。
	RelDir    string
	Filename  string
	TotalSize int64
	ChunkSize int
	// SHA256 由客户端计算，用于秒传。留空表示不做秒传。
	SHA256 string
}

// UploadView 是上传会话的对外视图。
type UploadView struct {
	UploadID string `json:"upload_id"`
	Status   string `json:"status"`
	// Instant 为 true 表示命中秒传，文件已经就绪、不必再传。
	Instant bool `json:"instant"`
	// MissingChunks 是还需要上传的分片序号。
	//
	// 返回**序号列表**而不是「还差 N 片」：客户端要据此决定传哪几片，
	// 而「还差 3 片」这句话它无法据以行动，只能重传全部——那正是
	// 断点续传要避免的事。
	MissingChunks []int     `json:"missing_chunks,omitempty"`
	TotalChunks   int       `json:"total_chunks"`
	File          *FileView `json:"file,omitempty"`
}

// CreateUpload 创建上传会话；命中秒传时直接完成（F-5-04）。
func (s *Service) CreateUpload(
	ctx context.Context, userID int64, req CreateUploadRequest, v authz.Viewer,
	operatorName, clientIP string,
) (*UploadView, error) {
	if err := s.ensureNode(ctx, req.NodeID); err != nil {
		return nil, err
	}
	if !model.ValidFileCategory(req.Category) {
		return nil, api.InvalidParameter("不支持的文件类别：" + req.Category)
	}
	if req.TotalSize <= 0 {
		return nil, api.InvalidParameter("文件大小必须大于 0")
	}
	if req.ChunkSize <= 0 {
		return nil, api.InvalidParameter("必须指定分片大小")
	}
	// 分片数上限：一个 1 字节的分片会生成一个天文数字的 bitmap，
	// 而它每收到一片都要重写一次。
	const maxChunks = 100_000
	chunks := req.TotalSize / int64(req.ChunkSize)
	if req.TotalSize%int64(req.ChunkSize) != 0 {
		chunks++
	}
	if chunks > maxChunks {
		return nil, api.InvalidParameter(
			"分片数过多（" + strconv.FormatInt(chunks, 10) + "），请增大分片大小；" +
				"最多 " + strconv.Itoa(maxChunks) + " 片")
	}

	relPath, err := s.resolveFileRelPath(req.RelDir, req.Filename)
	if err != nil {
		return nil, err
	}

	// 配额在**授予上传之前**检查：一个 40GB 的上传传到一半才被告知超配额，
	// 用户已经等了几十分钟，而那时除了丢弃别无选择。
	if s.quota != nil {
		if err := s.quota.Check(ctx, userID, req.NodeID, req.TotalSize); err != nil {
			return nil, err
		}
	}

	// 秒传：摘要命中**自己已有的**文件时直接完成。
	if req.SHA256 != "" {
		if existing, err := s.FindByDigest(ctx, userID, req.NodeID, req.SHA256); err != nil {
			return nil, err
		} else if existing != nil {
			file, err := s.instantCopy(ctx, existing, relPath, req.Filename, req.Category, userID, req.NodeID)
			if err != nil {
				return nil, err
			}
			s.record(ctx, audit.Entry{
				OperatorID: v.UserID, OperatorName: operatorName,
				NodeID: req.NodeID, ResourceType: "storage_file", ResourceID: file.ID,
				ResourceName: req.Filename, Action: "storage_file.instant",
				Params:  map[string]any{"sha256": req.SHA256, "rel_path": relPath},
				Success: true, ClientIP: clientIP,
			})
			view := toFileView(file)
			return &UploadView{Status: model.UploadCompleted, Instant: true, File: &view}, nil
		}
	}

	session := model.UploadSession{
		UploadID: newUploadID(),
		// Target 承载**文件类别**（iso / share / disk），而不是「上传到哪个子系统」：
		// 这三类正是上传的目标（f-5-03），而 storage_file.category 在完成时
		// 需要一个来源——会话表里没有别的列能表达它。
		//
		// 复用已有列而不是加一列：这张表在设计时就为「上传最终要变成什么」
		// 留了位置，只是那句话被写成了 target。
		Target:    req.Category,
		FileKey:   strconv.FormatInt(req.NodeID, 10) + ":" + strconv.FormatInt(userID, 10) + ":" + relPath,
		OwnerID:   &userID,
		NodeID:    &req.NodeID,
		TotalSize: req.TotalSize,
		ChunkSize: req.ChunkSize,
		Status:    model.UploadPending,
		ExpiresAt: s.now().Add(sessionTTL),
		// 目标路径与文件名随会话一起记在 FileKey 里，完成时才写 storage_file
		// ——见 CompleteUpload 的说明。
	}
	if req.SHA256 != "" {
		session.SHA256 = &req.SHA256
	}
	if err := s.db.WithContext(ctx).Create(&session).Error; err != nil {
		if isDuplicateKey(err) {
			// 唯一约束落在 FileKey 上：同一个目标已经有会话了。
			return nil, api.Conflict(
				"该目标已有一个进行中的上传；请续传它，或先取消再重新发起")
		}
		log.Printf("[userstorage] 创建上传会话失败: %v", err)
		return nil, api.Internal()
	}

	return &UploadView{
		UploadID:      session.UploadID,
		Status:        session.Status,
		TotalChunks:   session.TotalChunks(),
		MissingChunks: session.MissingChunks(),
	}, nil
}

// GetUpload 返回上传会话状态，供断点续传查询。
func (s *Service) GetUpload(ctx context.Context, userID int64, uploadID string) (*UploadView, error) {
	session, err := s.loadSession(ctx, uploadID, userID)
	if err != nil {
		return nil, err
	}

	view := &UploadView{
		UploadID:    session.UploadID,
		Status:      session.Status,
		TotalChunks: session.TotalChunks(),
	}
	if session.Status == model.UploadPending {
		// 过期会话**保留状态但不给分片清单**：告诉客户端「重开一个」，
		// 而不是让它对着一个不可能完成的任务继续重试。
		if session.Expired(s.now()) {
			view.Status = model.UploadExpired
			return view, nil
		}
		view.MissingChunks = session.MissingChunks()
	}
	return view, nil
}

// UploadChunkData 接收一个分片的**字节**并落盘（F-5-04）。
//
// 它是真正让上传跑起来的那一步：此前的 UploadChunk 只登记序号，而字节
// 从来没到过任何地方。
//
// 顺序是**先落盘、再登记**。反过来的话，登记成功而落盘失败会留下一个
// 「bitmap 说收到了、磁盘上却没有」的缺口——而那个缺口在完成拼接时才会
// 暴露，那时用户已经以为传完了。
func (s *Service) UploadChunkData(
	ctx context.Context, userID int64, uploadID string, index int, data []byte, clientSHA string,
) (*UploadView, error) {
	if s.chunks == nil {
		return nil, api.ValidationFailed("服务端未启用分片接收")
	}
	session, err := s.loadSession(ctx, uploadID, userID)
	if err != nil {
		return nil, err
	}
	if session.Status == model.UploadCompleted {
		return nil, api.ValidationFailed("该上传已完成")
	}
	if session.Expired(s.now()) {
		if err := s.markExpired(ctx, session); err != nil {
			return nil, err
		}
		return nil, api.ValidationFailed("该上传已过期，请重新发起")
	}

	total := session.TotalChunks()
	if index < 0 || index >= total {
		return nil, api.InvalidParameter(
			"分片序号越界：应在 0 到 " + strconv.Itoa(total-1) + " 之间")
	}

	digest, err := s.chunks.Put(uploadID, index, data)
	if err != nil {
		return nil, err
	}
	// 客户端给了摘要就校验：不校验的话，一个传坏的分片会被当成好的收下，
	// 而最终文件在装系统时才失败——那时已经很难追到是哪个环节坏的。
	//
	// 不匹配时**不登记**：让客户端重传这一片。登记了再报错等于把坏数据
	// 留在了暂存区，而完成时的摘要比对会发现它，但那时用户已经白传了整份。
	if clientSHA != "" && !strings.EqualFold(clientSHA, digest) {
		return nil, api.ValidationFailed(
			"分片 " + strconv.Itoa(index) + " 的摘要不匹配，请重传该分片")
	}

	session.MarkReceived(index)
	updates := map[string]any{
		"received_bitmap": session.ReceivedBitmap,
		"expires_at":      s.now().Add(sessionTTL),
	}
	if err := s.db.WithContext(ctx).Model(&model.UploadSession{}).
		Where("upload_id = ?", session.UploadID).
		Updates(updates).Error; err != nil {
		log.Printf("[userstorage] 登记分片失败 %s: %v", uploadID, err)
		return nil, api.Internal()
	}

	return &UploadView{
		UploadID: session.UploadID, Status: session.Status,
		TotalChunks: total, MissingChunks: session.MissingChunks(),
	}, nil
}

// UploadChunk 登记一个分片已收到（F-5-04：缺失分片补传）。
//
// 重复登记同一分片**不算错误**：网络重试是常态，把它当成错误会让客户端在
// 一次正常的重试上收到失败，然后放弃整个上传。
func (s *Service) UploadChunk(
	ctx context.Context, userID int64, uploadID string, index int, chunkSHA256 string,
) (*UploadView, error) {
	session, err := s.loadSession(ctx, uploadID, userID)
	if err != nil {
		return nil, err
	}
	if session.Status == model.UploadCompleted {
		return nil, api.ValidationFailed("该上传已完成")
	}
	if session.Expired(s.now()) {
		if err := s.markExpired(ctx, session); err != nil {
			return nil, err
		}
		return nil, api.ValidationFailed("该上传已过期，请重新发起")
	}

	total := session.TotalChunks()
	if index < 0 || index >= total {
		return nil, api.InvalidParameter(
			"分片序号越界：应在 0 到 " + strconv.Itoa(total-1) + " 之间")
	}

	changed := session.MarkReceived(index)
	updates := map[string]any{}
	if changed {
		updates["received_bitmap"] = session.ReceivedBitmap
	}
	// 续期：还在传就说明这个会话是活的，不应在传输中途过期——
	// 一个传了 20 小时的大文件会在最后一刻被判定过期，那是最坏的结果。
	updates["expires_at"] = s.now().Add(sessionTTL)

	if err := s.db.WithContext(ctx).Model(&model.UploadSession{}).
		Where("upload_id = ?", session.UploadID).
		Updates(updates).Error; err != nil {
		log.Printf("[userstorage] 更新分片状态失败 %s: %v", uploadID, err)
		return nil, api.Internal()
	}

	view := &UploadView{
		UploadID:      session.UploadID,
		Status:        session.Status,
		TotalChunks:   total,
		MissingChunks: session.MissingChunks(),
	}
	return view, nil
}

// CompleteUpload 收尾一次上传（F-5-04）。
//
// **配额在这里**再检查一次**。只在创建会话时检查的话，「开始时还没超」
// 就成了一个永久有效的通行证——用户可以先开一个会话，慢慢传几个小时，
// 期间其他操作把空间占满，最后落盘时依然会被放行。
func (s *Service) CompleteUpload(
	ctx context.Context, userID int64, uploadID string, v authz.Viewer,
	operatorName, clientIP string,
) (*FileView, error) {
	session, err := s.loadSession(ctx, uploadID, userID)
	if err != nil {
		return nil, err
	}
	if session.Status == model.UploadCompleted {
		return nil, api.ValidationFailed("该上传已完成")
	}
	if session.Expired(s.now()) {
		if err := s.markExpired(ctx, session); err != nil {
			return nil, err
		}
		return nil, api.ValidationFailed("该上传已过期，请重新发起")
	}
	if !session.IsComplete() {
		missing := session.MissingChunks()
		// 只说「还差 N 片」用户无法行动，因此把缺口数量与前几个序号一起给出来。
		head := missing
		if len(head) > 5 {
			head = head[:5]
		}
		return nil, api.ValidationFailed(
			"分片尚未到齐，还差 " + strconv.Itoa(len(missing)) + " 片（前几个：" +
				joinInts(head) + "）")
	}

	if session.NodeID == nil || session.OwnerID == nil {
		return nil, api.Internal()
	}
	if s.quota != nil {
		if err := s.quota.Check(ctx, *session.OwnerID, *session.NodeID, session.TotalSize); err != nil {
			return nil, err
		}
	}

	relPath := fileRelPathFromKey(session.FileKey)
	if relPath == "" {
		log.Printf("[userstorage] 会话的目标路径不可解析: %s", session.FileKey)
		return nil, api.Internal()
	}

	// 拼接：把所有分片按序写进一个临时文件，并同时算出整份的摘要。
	//
	// **流式拼接**，不把整个文件读进内存——一个 40GB 的镜像不可能放进内存，
	// 而"读进内存再写出去"的写法在测试里（几 KB）看不出问题。
	assembledPath, assembledSum, err := s.assembleAndStage(ctx, session, relPath)
	if err != nil {
		return nil, err
	}
	defer func() {
		// 无论成败都删掉拼接出来的临时文件：它是一份**完整副本**，留着会
		// 让一次 40GB 的上传在磁盘上占 80GB。分片暂存区则由下面按成败分别处理。
		if rmErr := os.Remove(assembledPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			log.Printf("[userstorage] 清理拼接临时文件失败: %v", rmErr)
		}
	}()

	// 交给节点落盘。
	//
	// 契约要求节点回传的摘要与控制面算出的**必须一致**——不一致说明传输
	// 过程中出了问题，此时应当把这次上传判为失败并清理，而不是登记一条
	// 指向损坏内容的记录（用户会在装系统时才发现，而那时已经很难追查）。
	if s.agent != nil {
		result, err := s.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpStorageFileCommit,
			NodeID: *session.NodeID,
			Target: relPath,
			Params: map[string]any{
				"rel_path": relPath,
				"size":     session.TotalSize,
				"checksum": assembledSum,
				"staged":   assembledPath,
			},
		})
		if err != nil {
			return nil, api.Unavailable("节点不可达，文件未落盘（分片已保留，可重试）")
		}
		if !result.Success {
			return nil, api.ValidationFailed(result.Message)
		}
		if info, ok := result.Data[agent.StorageFileDataKey].(agent.StorageFileInfo); ok {
			if info.Checksum != "" && !strings.EqualFold(info.Checksum, assembledSum) {
				// 清理掉这一份坏内容，并保留分片供重试。
				if s.chunks != nil {
					_ = s.chunks.Discard(session.UploadID)
				}
				log.Printf("[userstorage] 摘要不一致 upload=%s 本地=%s 节点=%s",
					session.UploadID, assembledSum, info.Checksum)
				return nil, api.ValidationFailed(
					"文件校验失败：节点收到的内容与本地上传的不一致，请重新上传")
			}
		}
	}

	now := s.now()
	file := model.StorageFile{
		NodeID: *session.NodeID, UserID: session.OwnerID,
		RelPath: relPath, Category: session.Target,
		Filename: path.Base(relPath), SizeBytes: session.TotalSize,
		SHA256: session.SHA256, UploadedAt: &now,
	}
	if err := s.db.WithContext(ctx).Create(&file).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该位置已存在同名文件")
		}
		log.Printf("[userstorage] 登记文件失败: %v", err)
		return nil, api.Internal()
	}

	if err := s.db.WithContext(ctx).Model(&model.UploadSession{}).
		Where("upload_id = ?", session.UploadID).
		Update("status", model.UploadCompleted).Error; err != nil {
		log.Printf("[userstorage] 更新会话状态失败 %s: %v", session.UploadID, err)
		// 文件已经登记成功，会话状态没更新只影响后续查询——不因此判为失败，
		// 那会让用户重试一次完整的上传，而文件其实已经在库里了。
	}

	// 文件已经登记好了，分片暂存区就没用了。清理失败只记日志——
	// 它占的是磁盘空间，而"文件已经上传成功"这个事实不该被一次清理失败
	// 推翻。废弃的分片由过期清理兜底。
	if s.chunks != nil {
		if err := s.chunks.Discard(session.UploadID); err != nil {
			log.Printf("[userstorage] 上传完成后清理分片失败 %s: %v", session.UploadID, err)
		}
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: *session.NodeID, ResourceType: "storage_file", ResourceID: file.ID,
		ResourceName: file.Filename, Action: "storage_file.upload",
		Params: map[string]any{
			"rel_path": relPath, "size": file.SizeBytes,
			"upload_id": session.UploadID,
		},
		Success: true, ClientIP: clientIP,
	})

	view := toFileView(&file)
	return &view, nil
}

// FindByDigest 按内容摘要查找用户**自己**已有的文件（秒传）。
//
// **查重范围必须限定在同一用户内**。按摘要全局查会让用户 A 通过
// 「上传 → 秒传成功」推断出用户 B 是否存在某个文件：内容本身读不到，
// 但「它存在」这条信息已经泄漏了——而对某些文件名（一份未公开的合同、
// 一个内部镜像）来说，存在性本身就是敏感信息。
//
// 这也解释了为什么不做「跨用户共享同一份内容」的省空间优化：那需要在
// 文件与用户之间再建一层引用，而引用一旦存在，就必然有一个「谁引用了它」
// 的可见性判断要做对——收益是一份重复内容的磁盘空间，代价是一整类
// 可以出错的地方。
func (s *Service) FindByDigest(
	ctx context.Context, userID, nodeID int64, sha256 string,
) (*model.StorageFile, error) {
	digest := strings.ToLower(strings.TrimSpace(sha256))
	if digest == "" {
		return nil, nil
	}
	var file model.StorageFile
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ? AND sha256 = ? AND uploaded_at IS NOT NULL",
			nodeID, userID, digest).
		First(&file).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		log.Printf("[userstorage] 按摘要查询失败: %v", err)
		return nil, api.Internal()
	}
	return &file, nil
}

// instantCopy 在秒传命中时为目标路径登记一条新记录。
//
// **不复制内容**：磁盘上的文件按内容寻址，两条记录指向同一份数据。
// 但记录是**独立**的——用户删掉其中一条时，另一条必须仍然可用，因此
// 引用计数由节点侧维护（控制面看不到谁在引用同一份数据）。
func (s *Service) instantCopy(
	ctx context.Context, src *model.StorageFile, relPath, filename, category string,
	userID, nodeID int64,
) (*model.StorageFile, error) {
	now := s.now()
	file := model.StorageFile{
		NodeID: nodeID, UserID: &userID,
		RelPath: relPath, Category: category, Filename: filename,
		SizeBytes: src.SizeBytes, SHA256: src.SHA256,
		OsType: src.OsType, OsVariant: src.OsVariant, MinDiskGB: src.MinDiskGB,
		UploadedAt: &now,
	}
	if err := s.db.WithContext(ctx).Create(&file).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该位置已存在同名文件")
		}
		log.Printf("[userstorage] 秒传登记失败: %v", err)
		return nil, api.Internal()
	}
	return &file, nil
}

// assembleAndStage 把分片拼成一个临时文件，返回它的路径与整份摘要。
func (s *Service) assembleAndStage(
	ctx context.Context, session *model.UploadSession, relPath string,
) (string, string, error) {
	if s.chunks == nil {
		return "", "", api.ValidationFailed("服务端未启用分片接收")
	}
	tmpDir, err := os.MkdirTemp("", "kc-upload-")
	if err != nil {
		log.Printf("[userstorage] 创建临时目录失败: %v", err)
		return "", "", api.Internal()
	}
	dst := filepath.Join(tmpDir, filepath.Base(relPath))

	f, err := os.Create(dst)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		log.Printf("[userstorage] 创建临时文件失败: %v", err)
		return "", "", api.Internal()
	}
	sum, assembleErr := s.chunks.Assemble(session.UploadID, session.TotalChunks(), f)
	closeErr := f.Close()
	if assembleErr != nil || closeErr != nil {
		_ = os.RemoveAll(tmpDir)
		if assembleErr != nil {
			return "", "", assembleErr
		}
		log.Printf("[userstorage] 关闭临时文件失败: %v", closeErr)
		return "", "", api.Internal()
	}
	_ = ctx
	return dst, sum, nil
}

// --- 内部 ---

// resolveFileRelPath 把目标目录与文件名拼成存储根下的相对路径。
func (s *Service) resolveFileRelPath(dir, filename string) (string, error) {
	name := strings.TrimSpace(filename)
	if name == "" {
		return "", api.InvalidParameter("必须填写文件名")
	}
	// 文件名里的路径分隔符一律拒绝：它让「文件名」可以夹带目录层级，
	// 从而绕过对目录的那一层校验。
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		return "", api.InvalidParameter("文件名不能包含路径分隔符")
	}
	if strings.ContainsRune(name, 0) {
		return "", api.InvalidParameter("文件名不能包含空字符")
	}
	if len(name) > 255 {
		return "", api.InvalidParameter("文件名过长（最多 255 字节）")
	}

	cleanDir, err := s.resolveRelDir(dir)
	if err != nil {
		return "", err
	}
	if cleanDir == "" {
		return name, nil
	}
	return cleanDir + "/" + name, nil
}

// resolveRelDir 校验目标目录。
//
// 与目录共享（f-5-06）同一套边界：只接受相对路径，清理后不得越出存储根。
// 两处各写一份是因为它们的输入来源不同（这里是上传、那里是共享），但
// 边界规则必须一致——规则不一致时，弱的那一处就是整条防线。
func (s *Service) resolveRelDir(dir string) (string, error) {
	raw := strings.TrimSpace(dir)
	if raw == "" || raw == "." {
		return "", nil
	}
	if strings.ContainsRune(raw, 0) || strings.Contains(raw, "\\") {
		return "", api.InvalidParameter("目录名中不能包含反斜杠或空字符")
	}
	if strings.HasPrefix(raw, "/") {
		return "", api.InvalidParameter("目录请填写相对于存储根的路径，不要以 / 开头")
	}
	clean := path.Clean(raw)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "/" {
		return "", api.InvalidParameter("目录不能指向存储根之外")
	}
	return clean, nil
}

// loadStorage 读取（或在缺失时构造）用户存储记录。
func (s *Service) loadStorage(ctx context.Context, userID, nodeID int64) (*model.UserStorage, error) {
	var us model.UserStorage
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND node_id = ?", userID, nodeID).First(&us).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// 不在这里自动建记录：GET 一个不存在的东西应当是「没开通」，
		// 而不是顺手写一行——那会让「查了一下」变成一次写操作。
		return &model.UserStorage{UserID: userID, NodeID: nodeID}, nil
	}
	if err != nil {
		log.Printf("[userstorage] 查询用户存储失败: %v", err)
		return nil, api.Internal()
	}
	return &us, nil
}

func (s *Service) loadFile(ctx context.Context, id int64) (*model.StorageFile, error) {
	var file model.StorageFile
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&file).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("文件不存在")
		}
		log.Printf("[userstorage] 查询文件失败: %v", err)
		return nil, api.Internal()
	}
	return &file, nil
}

// loadSession 读取上传会话并校验归属。
//
// 归属校验是必须的：UploadID 会被客户端长期持有，不校验的话任何登录用户
// 都能凭一个猜到的（或从日志里看到的）id 往别人的上传里塞分片。
func (s *Service) loadSession(
	ctx context.Context, uploadID string, userID int64,
) (*model.UploadSession, error) {
	var session model.UploadSession
	if err := s.db.WithContext(ctx).
		Where("upload_id = ?", uploadID).First(&session).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("上传会话不存在")
		}
		log.Printf("[userstorage] 查询上传会话失败: %v", err)
		return nil, api.Internal()
	}
	// 404 而非 403：403 会确认「这个会话 id 存在」。
	if session.OwnerID == nil || *session.OwnerID != userID {
		return nil, api.NotFound("上传会话不存在")
	}
	return &session, nil
}

func (s *Service) markExpired(ctx context.Context, session *model.UploadSession) error {
	if err := s.db.WithContext(ctx).Model(&model.UploadSession{}).
		Where("upload_id = ?", session.UploadID).
		Update("status", model.UploadExpired).Error; err != nil {
		log.Printf("[userstorage] 标记会话过期失败 %s: %v", session.UploadID, err)
		return api.Internal()
	}
	return nil
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).
		Select("id", "enroll_state").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[userstorage] 查询节点失败: %v", err)
		return api.Internal()
	}
	if !n.IsEnrolled() {
		return api.ValidationFailed("节点尚未接入")
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

// newUploadID 生成随机的会话标识。
func newUploadID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 退回到时间戳：随机源不可用是极端情况，而这里更需要「有结果」
		// 而不是「安全」——UploadID 的归属由 OwnerID 校验兜底。
		return "u" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

// fileRelPathFromKey 从 FileKey 还原出目标相对路径。
//
// FileKey 的格式是 `<nodeID>:<userID>:<relPath>`——它被用作**唯一约束**，
// 因此不能只存 relPath（那样两个用户在同一个节点上就永远无法上传同名文件）。
// 节点与用户前缀让唯一性落在「同一个用户的同一个目标」上。
func fileRelPathFromKey(key string) string {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return ""
	}
	return parts[2]
}

func toFileView(f *model.StorageFile) FileView {
	view := FileView{
		ID: f.ID, Category: f.Category, Filename: f.Filename, RelPath: f.RelPath,
		SizeBytes: f.SizeBytes, MinDiskGB: f.MinDiskGB,
		CreatedAt: f.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if f.SHA256 != nil {
		view.SHA256 = *f.SHA256
	}
	if f.OsType != nil {
		view.OsType = *f.OsType
	}
	if f.OsVariant != nil {
		view.OsVariant = *f.OsVariant
	}
	if f.UploadedAt != nil {
		view.UploadedAt = f.UploadedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

func joinInts(list []int) string {
	parts := make([]string, 0, len(list))
	for _, n := range list {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ", ")
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
