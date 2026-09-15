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
