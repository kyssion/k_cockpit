package auth

import (
	"context"
	"errors"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// UserByID 按 ID 读取用户。
//
// 存在的理由：认证中间件注入的用户对象是**本次请求开始时的快照**。当某个
// 流程在本请求内改动了用户记录、又需要读到新值时（典型是「生成 TOTP 密钥
// 后立刻校验」，两步可能落在同一个请求里），用旧快照会表现为「刚做的事
// 好像没生效」——而用户显然刚点过。
func (s *Service) UserByID(ctx context.Context, id int64) (*model.User, error) {
	var user model.User
	err := s.db.WithContext(ctx).First(&user, id).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 会话仍有效但用户已不存在：按未认证处理，让前端回到登录页，
		// 而不是给出一个用户无法处理的服务端错误。
		return nil, errUnauthenticated
	case err != nil:
		log.Printf("[auth] 读取用户失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}
	return &user, nil
}

// UpdateUser 更新用户记录的指定字段。
//
// 只接受「字段 → 值」而不是整个 User 结构：整结构更新会让调用方**无意中
// 覆盖它没有读过的字段**（典型的是一次并发改名把密码哈希写回旧值），
// 而那种问题只表现为"某个设置莫名其妙变了"，很难追到是哪次调用。
func (s *Service) UpdateUser(ctx context.Context, userID int64, updates map[string]any) error {
	if len(updates) == 0 {
		return nil
	}
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", userID).Updates(updates).Error; err != nil {
		log.Printf("[auth] 更新用户失败 id=%d: %v", userID, err)
		return api.Internal()
	}
	return nil
}

// UsernameTaken 报告用户名是否已被他人占用。excludeID 用于改名的场景。
//
// 唯一性由数据库约束兜底，这个方法只是为了让上层能给出**可以说清楚的错误**
// 而不是一句唯一约束冲突。调用方不应据此对外区分"被占用"与"不存在"以外的
// 信息——那是用户名枚举的入口（f-1-06 R-005）。
func (s *Service) UsernameTaken(ctx context.Context, name string, excludeID int64) (bool, error) {
	var n int64
	q := s.db.WithContext(ctx).Model(&model.User{}).Where("username = ?", name)
	if excludeID > 0 {
		q = q.Where("id <> ?", excludeID)
	}
	if err := q.Count(&n).Error; err != nil {
		log.Printf("[auth] 查询用户名占用失败: %v", err)
		return false, api.Internal()
	}
	return n > 0, nil
}
