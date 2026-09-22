// Package invite 管理邀请注册（F-1-10）。
//
// 为什么是邀请而不是开放注册：这个面板能接管宿主机上的全部虚拟机，开放
// 注册等于把入口交给任何人。邀请让"谁能进来"成为一次**由管理员发出、有期限、
// 可追溯**的动作。
//
// 与「新建用户」的区别：新建用户时密码由管理员设、且要走强制改密；邀请则由
// 受邀人自己设密码，管理员从头到尾不知道它。
package invite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// Service 提供邀请能力。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	// mail 发送邀请邮件。为 nil 时只返回链接，由管理员自己转交。
	mail func(ctx context.Context, to, link, role string) error
	// createUser 由 useradmin 提供：真正创建账号并带上配额。
	createUser func(ctx context.Context, email, username, password, role string,
		quotaEnabled bool, quotaBytes int64) error
	// siteURL 用于拼邀请链接。
	siteURL func(ctx context.Context) string
}

// NewService 构造服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{db: db, audit: recorder}
}

// SetMailer 装配邮件发送；不装配时创建邀请只返回链接。
func (s *Service) SetMailer(fn func(ctx context.Context, to, link, role string) error) { s.mail = fn }

// SetUserCreator 装配账号创建（由 useradmin 提供）。
func (s *Service) SetUserCreator(fn func(ctx context.Context, email, username, password, role string,
	quotaEnabled bool, quotaBytes int64) error) {
	s.createUser = fn
}

// SetSiteURL 装配站点地址（用于拼链接）。
func (s *Service) SetSiteURL(fn func(ctx context.Context) string) { s.siteURL = fn }

// CreateRequest 是创建邀请的请求。
type CreateRequest struct {
	Email        string
	Role         string
	QuotaEnabled bool
	QuotaBytes   int64
	Remark       string
	// TTLHours 是有效期（小时），<=0 时取默认。
	TTLHours int
}

// defaultTTLHours 是邀请的默认有效期。
//
// 三天而不是更久：邀请链接躺在收件箱里，时间越长被人翻到的机会越大；而三天
// 足够大多数人点开。过期可以重发，代价很低。
const defaultTTLHours = 72

// minPasswordLen 是受邀人自设密码的下限。
const minPasswordLen = 12

// View 是邀请的对外视图（**不含令牌**）。
type View struct {
	ID           int64  `json:"id"`
	Email        string `json:"email"`
	Role         string `json:"role"`
	QuotaEnabled bool   `json:"quota_enabled"`
	QuotaBytes   int64  `json:"quota_bytes"`
	Remark       string `json:"remark,omitempty"`
	ExpiresAt    string `json:"expires_at"`
	// Status 取值 pending / accepted / revoked / expired。
	Status string `json:"status"`
	// Link 仅在创建与重发时返回一次，之后不再可读。
	Link string `json:"link,omitempty"`
}

// Create 创建一条邀请。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, operatorID int64, operatorName, clientIP string,
) (*View, error) {
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, api.InvalidParameter("请填写受邀人的邮箱")
	}
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = model.RoleTenant
	}
	if role != model.RoleTenant && role != model.RoleAdmin {
		return nil, api.InvalidParameter("角色必须是 tenant 或 admin")
	}

	// 已存在的账号不能再邀请：那不是"邀请"，是改角色。
	var existing model.User
	if err := s.db.WithContext(ctx).Where("lower(email) = ?", email).
		Select("id").First(&existing).Error; err == nil {
		return nil, api.InvalidParameter("该邮箱已有账号；要调整权限请直接编辑用户")
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	ttl := req.TTLHours
	if ttl <= 0 {
		ttl = defaultTTLHours
	}
	row := model.UserInvite{
		Email:        email,
		Role:         role,
		TokenHash:    hashToken(token),
		QuotaBytes:   req.QuotaBytes,
		QuotaEnabled: req.QuotaEnabled,
		ExpiresAt:    time.Now().Add(time.Duration(ttl) * time.Hour),
		CreatedBy:    &operatorID,
		CreatedAt:    time.Now(),
	}
	if r := strings.TrimSpace(req.Remark); r != "" {
		row.Remark = &r
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[invite] 创建邀请失败: %v", err)
		return nil, api.Internal()
	}

	link := s.linkFor(ctx, token)
	// 邮件失败**不影响邀请创建**：链接已经产生，管理员可以自己转发。让一次
	// 邮件发送失败把已经建好的邀请判为失败，用户会以为要重来一次，而那条
	// 链接其实已经发出了。
	if s.mail != nil {
		if err := s.mail(ctx, email, link, role); err != nil {
			log.Printf("[invite] 发送邀请邮件失败: %v", err)
		}
	}

	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		ResourceType: "user_invite", ResourceID: row.ID, ResourceName: email,
		Action:  "invite.create",
		Params:  map[string]any{"role": role, "ttl_hours": ttl},
		Success: true, ClientIP: clientIP,
	})

	view := toView(&row, time.Now())
	view.Link = link
	return &view, nil
}

// List 列出邀请。
func (s *Service) List(ctx context.Context) ([]View, error) {
	var rows []model.UserInvite
	if err := s.db.WithContext(ctx).Order("created_at DESC").Limit(200).Find(&rows).Error; err != nil {
		log.Printf("[invite] 查询邀请失败: %v", err)
		return nil, api.Internal()
	}
	now := time.Now()
	out := make([]View, 0, len(rows))
	for i := range rows {
		out = append(out, toView(&rows[i], now))
	}
	return out, nil
}

// Revoke 撤销一条邀请。
func (s *Service) Revoke(
	ctx context.Context, id int64, operatorID int64, operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.AcceptedAt != nil {
		return nil, api.Conflict("该邀请已被使用，不能撤销——要收回权限请直接封禁那个账号")
	}
	now := time.Now()
	if err := s.db.WithContext(ctx).Model(&model.UserInvite{}).
		Where("id = ?", id).Updates(map[string]any{"revoked_at": now}).Error; err != nil {
		log.Printf("[invite] 撤销邀请失败: %v", err)
		return nil, api.Internal()
	}
	row.RevokedAt = &now
	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		ResourceType: "user_invite", ResourceID: id, ResourceName: row.Email,
		Action: "invite.revoke", Success: true, ClientIP: clientIP,
	})
	view := toView(row, now)
	return &view, nil
}

// Resend 重发：换新令牌、延长有效期。
//
// 为什么是"换新"而不是"再发一次同样的链接"：旧链接可能已经泄漏（转发、聊天
// 记录），重发时把它作废掉，成本为零而收益明确。
func (s *Service) Resend(
	ctx context.Context, id int64, operatorID int64, operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if row.AcceptedAt != nil {
		return nil, api.Conflict("该邀请已被使用")
	}
	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	expires := now.Add(defaultTTLHours * time.Hour)
	if err := s.db.WithContext(ctx).Model(&model.UserInvite{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"token_hash": hashToken(token),
			"expires_at": expires,
			"revoked_at": nil,
		}).Error; err != nil {
		log.Printf("[invite] 重发邀请失败: %v", err)
		return nil, api.Internal()
	}
	row.TokenHash = hashToken(token)
	row.ExpiresAt = expires
	row.RevokedAt = nil

	link := s.linkFor(ctx, token)
	if s.mail != nil {
		if err := s.mail(ctx, row.Email, link, row.Role); err != nil {
			log.Printf("[invite] 重发邀请邮件失败: %v", err)
		}
	}
	s.record(ctx, audit.Entry{
		OperatorID: operatorID, OperatorName: operatorName,
		ResourceType: "user_invite", ResourceID: id, ResourceName: row.Email,
		Action: "invite.resend", Success: true, ClientIP: clientIP,
	})
	view := toView(row, now)
	view.Link = link
	return &view, nil
}

// Preview 公开预览一条邀请：只说明"这条邀请是给谁的、什么角色"，不暴露配额。
func (s *Service) Preview(ctx context.Context, token string) (*View, error) {
	row, err := s.byToken(ctx, token)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !row.Usable(now) {
		return nil, api.NotFound("邀请无效或已过期")
	}
	view := toView(row, now)
	// 预览**不带配额**：那是管理员与受邀人之间的约定，不该被任何拿到链接的
	// 人看到。
	view.QuotaBytes = 0
	view.QuotaEnabled = false
	return &view, nil
}

// AcceptRequest 是接受邀请的请求。
type AcceptRequest struct {
	Token    string
	Username string
	Password string
	Email    string
}

// Accept 接受邀请并创建账号。
func (s *Service) Accept(ctx context.Context, req AcceptRequest) error {
	if s.createUser == nil {
		return api.Internal()
	}
	row, err := s.byToken(ctx, req.Token)
	if err != nil {
		return err
	}
	now := time.Now()
	if !row.Usable(now) {
		return api.NotFound("邀请无效或已过期")
	}

	username := strings.TrimSpace(req.Username)
	if username == "" {
		return api.InvalidParameter("请填写用户名")
	}
	if len(req.Password) < minPasswordLen {
		return api.InvalidParameter("密码长度不得少于 " + strconv.Itoa(minPasswordLen) + " 个字符")
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" {
		email = row.Email
	}

	// 先建账号、再标记邀请已用。
	//
	// 顺序不能反：先标记的话，建号失败会让这条邀请变成"已使用但没人进来"，
	// 而管理员看到的是"他已接受"——那比没接受更难解释。
	if err := s.createUser(ctx, email, username, req.Password, row.Role,
		row.QuotaEnabled, row.QuotaBytes); err != nil {
		return err
	}

	var created model.User
	_ = s.db.WithContext(ctx).Where("username = ?", username).
		Select("id").First(&created).Error
	if err := s.db.WithContext(ctx).Model(&model.UserInvite{}).
		Where("id = ?", row.ID).
		Updates(map[string]any{"accepted_at": now, "accepted_user_id": created.ID}).Error; err != nil {
		// 账号已经建好了：标记失败只记日志，用户此刻已经能登录。
		log.Printf("[invite] 标记邀请已使用失败 id=%d: %v", row.ID, err)
	}

	s.record(ctx, audit.Entry{
		ResourceType: "user_invite", ResourceID: row.ID, ResourceName: email,
		Action:  "invite.accept",
		Params:  map[string]any{"username": username},
		Success: true,
	})
	return nil
}

// --- 内部 ---

func (s *Service) load(ctx context.Context, id int64) (*model.UserInvite, error) {
	var row model.UserInvite
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("邀请不存在")
		}
		log.Printf("[invite] 查询邀请失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) byToken(ctx context.Context, token string) (*model.UserInvite, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, api.InvalidParameter("缺少邀请令牌")
	}
	var row model.UserInvite
	if err := s.db.WithContext(ctx).Where("token_hash = ?", hashToken(token)).
		First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("邀请无效或已过期")
		}
		log.Printf("[invite] 查询邀请失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

func (s *Service) linkFor(ctx context.Context, token string) string {
	base := ""
	if s.siteURL != nil {
		base = strings.TrimRight(s.siteURL(ctx), "/")
	}
	return base + "/invite/" + token
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit != nil {
		s.audit.Record(ctx, e)
	}
}

// 令牌只在创建与重发时出现一次，之后库里只有哈希。
func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func toView(u *model.UserInvite, now time.Time) View {
	view := View{
		ID: u.ID, Email: u.Email, Role: u.Role,
		QuotaBytes: u.QuotaBytes, QuotaEnabled: u.QuotaEnabled,
		ExpiresAt: u.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if u.Remark != nil {
		view.Remark = *u.Remark
	}
	switch {
	case u.AcceptedAt != nil:
		view.Status = "accepted"
	case u.RevokedAt != nil:
		view.Status = "revoked"
	case !u.ExpiresAt.After(now):
		view.Status = "expired"
	default:
		view.Status = "pending"
	}
	return view
}
