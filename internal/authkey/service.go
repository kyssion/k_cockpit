// Package authkey 管理会话令牌的签名密钥（F-1-09：轮换）。
//
// 语义是**更换密钥即让全部旧令牌失效**（见 auth/token.go 的 R-012）。
// 这听起来很重，但它正是"轮换"要的效果：怀疑密钥泄漏时，需要的是一次
// 立刻生效的全员登出，而不是一个"等旧令牌自然过期"的宽限期——那样在这
// 段时间里泄漏出去的令牌仍然可用。
//
// 因此这里：
//   - 只保留当前在用的一把密钥；
//   - 轮换 = 生成新密钥并替换，旧密钥不保留；
//   - 自动轮换的间隔由设置项给出，0 表示不自动轮换。
package authkey

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/audit"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
)

// Service 管理签名密钥。
type Service struct {
	db *gorm.DB
	// encKey 用于加解密存放的密钥。它由配置的根密钥派生。
	encKey []byte
	// bootstrap 是首次启用的密钥：数据库里还没有时用它，避免"换过一次
	// 配置"就取不到历史密钥。
	bootstrap string
	audit     *audit.Recorder
}

// NewService 构造服务。
func NewService(db *gorm.DB, encKey []byte, bootstrap string, recorder *audit.Recorder) *Service {
	return &Service{db: db, encKey: encKey, bootstrap: bootstrap, audit: recorder}
}

// Status 是当前密钥的状态。
type Status struct {
	KeyID     string `json:"key_id"`
	RotatedAt string `json:"rotated_at"`
	// AgeDays 是距离上次轮换的天数，界面用它说明"是不是该换了"。
	AgeDays int `json:"age_days"`
	// AutoRotateDays 为 0 表示未开启自动轮换。
	AutoRotateDays int `json:"auto_rotate_days"`
}

// Load 取当前在用的密钥（明文）。没有时按 bootstrap 创建一条。
func (s *Service) Load(ctx context.Context) (keyID string, secret []byte, err error) {
	var row model.SessionSigningKey
	err = s.db.WithContext(ctx).Where("active = ?", true).
		Order("id DESC").First(&row).Error
	switch {
	case err == nil:
		plain, err := cryptoutil.Open(s.encKey, row.SecretEnc)
		if err != nil {
			// 解不开通常意味着根密钥变了。按内部错误处理：让用户去改一个
			// 已经不存在的密码只会浪费他的时间。
			log.Printf("[authkey] 解密签名密钥失败: %v", err)
			return "", nil, errors.New("无法读取签名密钥")
		}
		return row.KeyID, []byte(plain), nil

	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.seed(ctx)

	default:
		log.Printf("[authkey] 查询签名密钥失败: %v", err)
		return "", nil, err
	}
}

// seed 用配置里的初始密钥建一条记录。
func (s *Service) seed(ctx context.Context) (string, []byte, error) {
	if len(s.bootstrap) < 32 {
		return "", nil, errors.New("初始签名密钥长度不足")
	}
	sealed, err := cryptoutil.Seal(s.encKey, s.bootstrap)
	if err != nil {
		return "", nil, err
	}
	now := time.Now()
	row := model.SessionSigningKey{
		KeyID:     newKeyID(),
		SecretEnc: sealed,
		Active:    true,
		CreatedAt: now,
		RotatedAt: now,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[authkey] 写入初始签名密钥失败: %v", err)
		return "", nil, err
	}
	return row.KeyID, []byte(s.bootstrap), nil
}

// Rotate 生成新密钥并让旧令牌全部失效。
func (s *Service) Rotate(ctx context.Context, operatorID int64, operatorName, clientIP string) (*Status, error) {
	plain, err := randomSecret()
	if err != nil {
		return nil, err
	}
	sealed, err := cryptoutil.Seal(s.encKey, plain)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	row := model.SessionSigningKey{
		KeyID:     newKeyID(),
		SecretEnc: sealed,
		Active:    true,
		CreatedAt: now,
		RotatedAt: now,
		RotatedBy: &operatorID,
	}
	// 先写新的、再停用旧的。
	//
	// 顺序不能反：先停用会让这一瞬间没有可用密钥，而任何一个并发请求都会
	// 拿到"无法读取签名密钥"——那是一次自己造成的中止。
	if err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Model(&model.SessionSigningKey{}).
			Where("active = ? AND id <> ?", true, row.ID).
			Updates(map[string]any{"active": false, "rotated_at": now}).Error
	}); err != nil {
		log.Printf("[authkey] 轮换签名密钥失败: %v", err)
		return nil, err
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: operatorID, OperatorName: operatorName,
			ResourceType: "auth_key", ResourceID: row.ID, ResourceName: row.KeyID,
			Action:  "auth_key.rotate",
			Params:  map[string]any{"note": "轮换后全部旧会话令牌失效"},
			Success: true, ClientIP: clientIP,
		})
	}
	return &Status{KeyID: row.KeyID, RotatedAt: now.Format("2006-01-02T15:04:05Z07:00")}, nil
}

// Current 返回当前密钥的状态（不含密钥本身）。
func (s *Service) Current(ctx context.Context) (*Status, error) {
	var row model.SessionSigningKey
	if err := s.db.WithContext(ctx).Where("active = ?", true).
		Order("id DESC").First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		log.Printf("[authkey] 查询签名密钥失败: %v", err)
		return nil, err
	}
	st := &Status{
		KeyID:     row.KeyID,
		RotatedAt: row.RotatedAt.Format("2006-01-02T15:04:05Z07:00"),
		AgeDays:   int(time.Since(row.RotatedAt).Hours() / 24),
	}
	return st, nil
}

// RotateIfDue 在超过自动轮换间隔时轮换一次；未开启或没到期时什么都不做。
func (s *Service) RotateIfDue(ctx context.Context, maxAgeDays int) (bool, error) {
	if maxAgeDays <= 0 {
		return false, nil
	}
	st, err := s.Current(ctx)
	if err != nil {
		return false, err
	}
	if st == nil || st.AgeDays < maxAgeDays {
		return false, nil
	}
	if _, err := s.Rotate(ctx, 0, "scheduler", ""); err != nil {
		return false, err
	}
	return true, nil
}

func randomSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newKeyID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("20060102150405")
	}
	return hex.EncodeToString(buf)
}
