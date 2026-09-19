// Package emergency 实现服务器侧应急脚本（F-9-06）。
//
// 它是**面板不可用时的带外入口**：重置管理员密码、清除 2FA、清除邮箱绑定。
// 在宿主机本地执行，不经 Web。
//
// 三条决定贯穿本包：
//
//  1. **不假装有门禁。** 能在这台机器上以这个身份运行它的人，本来就能直接
//     改数据库。在脚本上再套一层「请输入管理密码」只是表演，而且它会给运维
//     一种"这个工具受保护"的错觉——真正的边界是**宿主机的登录权限**，
//     这一点要在文档里说清，而不是用一层无效的校验去暗示别的。
//
//  2. **必须自己写审计。** 它绕过整个 Web 层，因此那一条链路上所有的审计
//     都不会发生。不在这里写的话，「谁在什么时候重置了管理员密码」就没有
//     任何地方能回答——而这恰恰是事后最需要回答的问题。来源记为
//     `model.SourceEmergency`，以便与面板发出的操作区分开。
//
//  3. **不依赖任何后台组件。** 它能在服务完全没起来、甚至起不来的时候工作
//     ——那正是它存在的理由。因此它只开数据库，不构造队列、采集器或任何
//     需要运行循环的东西。
package emergency

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// Tool 是应急操作的执行者。
type Tool struct {
	db    *gorm.DB
	audit *audit.Recorder
	now   func() time.Time
}

// New 构造工具。
func New(db *gorm.DB, recorder *audit.Recorder) *Tool {
	return &Tool{db: db, audit: recorder, now: time.Now}
}

// AdminView 是一个管理员账号的摘要。
//
// **不返回密码哈希与 TOTP 密钥**：这个工具的输出是给人看的（通常会被贴到
// 工单或聊天里），而那些字段没有任何理由离开数据库。
type AdminView struct {
	Username string `json:"username"`
	Status   string `json:"status"`
	// Has2FA 表示该账号启用了两步验证。
	Has2FA bool   `json:"has_2fa"`
	Email  string `json:"email,omitempty"`
	// EmailVerified 表示邮箱是否已验证。未验证的邮箱在"用邮箱找回"的
	// 流程里是没用的——这一点容易被忽略。
	EmailVerified bool `json:"email_verified"`
}

// ListAdmins 列出全部管理员账号。
//
// 它是这个工具**最先要用到的**子命令：另外几个都要指定用户名，而在面板
// 不可用时，人未必记得住管理员叫什么。
func (t *Tool) ListAdmins(ctx context.Context) ([]AdminView, error) {
	var users []model.User
	if err := t.db.WithContext(ctx).
		Where("role = ?", model.RoleAdmin).
		Order("id").
		Find(&users).Error; err != nil {
		log.Printf("[emergency] 查询管理员失败: %v", err)
		return nil, api.Internal()
	}

	out := make([]AdminView, 0, len(users))
	for i := range users {
		u := &users[i]
		v := AdminView{
			Username: u.Username, Status: u.Status,
			Has2FA: u.TotpEnabled,
		}
		if u.Email != nil {
			v.Email = *u.Email
		}
		v.EmailVerified = u.EmailVerifiedAt != nil
		out = append(out, v)
	}
	return out, nil
}

// ResetPassword 重置指定用户的密码。
//
// 与「用户自助改密码」的差别只有一处，但很关键：这里**不需要旧密码**
// ——那正是它的用途（旧密码不知道了）。
//
// 它更新 security_updated_at，按 R-010 这会作废该用户的**全部既有会话**。
// 这是必需的而不是副作用：重置密码最常见的动机是"账号可能被盗"，如果
// 攻击者那个会话还活着，这次重置就只挡住了他不知道的凭据，而他手上那个
// 仍然有效。
func (t *Tool) ResetPassword(ctx context.Context, username, newPassword string) (*model.User, error) {
	user, err := t.find(ctx, username)
	if err != nil {
		return nil, err
	}
	if len(newPassword) < minPasswordLen {
		return nil, api.InvalidParameter(fmt.Sprintf(
			"密码至少 %d 位。应急重置同样要过长度下限——一个临时设成 \"123456\" "+
				"的管理员账号，往往会在重置之后一直留着", minPasswordLen))
	}

	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		log.Printf("[emergency] 生成密码哈希失败: %v", err)
		return nil, api.Internal()
	}

	// force_password_change：应急重置出来的密码是**临时的**（可能通过工单、
	// 聊天或口头传递），因此要求登录后立即改掉。不设这个标记的话，那个
	// 临时密码会一直有效。
	if err := t.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{
			"password_hash":         hash,
			"security_updated_at":   t.now(),
			"force_password_change": true,
		}).Error; err != nil {
		log.Printf("[emergency] 重置密码失败: %v", err)
		return nil, api.Internal()
	}

	t.record(ctx, user, "emergency.password.reset", map[string]any{
		"username": user.Username,
		// **不记密码本身。** 审计里出现凭据等于把它复制到了另一个地方。
		"note":                  "全部既有会话已失效（R-010），并要求下次登录后改密",
		"force_password_change": true,
	})
	return user, nil
}

// Clear2FA 清除用户的密码器与恢复码。
//
// **两者必须一起清。** 只清 TOTP 而留下恢复码，等于把"另一把还能开的钥匙"
// 留在抽屉里——而用户以为两步验证已经被摘掉了。反过来说，恢复码本身也是
// 一种第二因子，只清一个在安全上是自相矛盾的。
func (t *Tool) Clear2FA(ctx context.Context, username string) (*model.User, error) {
	user, err := t.find(ctx, username)
	if err != nil {
		return nil, err
	}

	if err := t.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{
			"totp_secret_enc":     nil,
			"totp_enabled":        false,
			"recovery_codes_hash": nil,
			"security_updated_at": t.now(),
		}).Error; err != nil {
		log.Printf("[emergency] 清除 2FA 失败: %v", err)
		return nil, api.Internal()
	}

	t.record(ctx, user, "emergency.2fa.clear", map[string]any{
		"username": user.Username,
		"note":     "密码器与恢复码一并清除；全部既有会话已失效（R-010）",
	})
	return user, nil
}

// ClearEmail 清除用户的邮箱绑定。
//
// 用途有两类：邮箱写错了导致找回流程走不通；或者邮箱已失效（离职、域名
// 停用），而那会让"用邮箱找回"这条路彻底堵死。
func (t *Tool) ClearEmail(ctx context.Context, username string) (*model.User, error) {
	user, err := t.find(ctx, username)
	if err != nil {
		return nil, err
	}

	// EmailVerifiedAt 必须一起清。留着一个"已验证"的时间戳而邮箱是空的，
	// 会让后续任何"按已验证邮箱查找"的逻辑拿到一个自相矛盾的状态。
	if err := t.db.WithContext(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{
			"email":               nil,
			"email_verified_at":   nil,
			"security_updated_at": t.now(),
		}).Error; err != nil {
		log.Printf("[emergency] 清除邮箱失败: %v", err)
		return nil, api.Internal()
	}

	prev := ""
	if user.Email != nil {
		prev = *user.Email
	}
	t.record(ctx, user, "emergency.email.clear", map[string]any{
		"username": user.Username,
		// 记下**原来绑的是什么**：事后追查"这个账号什么时候被改绑过"时，
		// 旧地址是唯一能对上的线索。
		"previous_email": prev,
		"note":           "邮箱与验证状态一并清除；全部既有会话已失效（R-010）",
	})
	return user, nil
}

// find 按用户名取用户。
func (t *Tool) find(ctx context.Context, username string) (*model.User, error) {
	var user model.User
	err := t.db.WithContext(ctx).Where("username = ?", username).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("用户不存在：" + username +
			"（用 list-admins 查看全部管理员账号）")
	case err != nil:
		log.Printf("[emergency] 查询用户失败: %v", err)
		return nil, api.Internal()
	}
	return &user, nil
}

// record 写一条审计。
//
// 来源固定为 emergency：这一条链路上没有 HTTP 请求，因此也就没有来源 IP。
// 事后要区分「这次重置是从面板点的还是从宿主机命令行敲的」时，这个字段是
// 唯一的依据——而它与「是谁做的」这个问题直接相关：能登上面板的人可能很多，
// 能登上宿主机的人通常很少。
func (t *Tool) record(ctx context.Context, user *model.User, action string, params map[string]any) {
	if t.audit == nil {
		return
	}
	t.audit.Record(ctx, audit.Entry{
		OperatorName: "本地应急脚本",
		Source:       model.SourceEmergency,
		ResourceType: "user",
		ResourceID:   user.ID,
		ResourceName: user.Username,
		Action:       action,
		Params:       params,
		Success:      true,
	})
}

// minPasswordLen 与登录侧的下限保持一致。
const minPasswordLen = 8

// GeneratePassword 生成一个随机临时密码。
//
// 用 base64url 而不是随便拼可读单词：这个密码是**临时**的（登录后即要求
// 改掉），可读性带来的收益很小，而一旦有人用字典词拼出「正确-horse-电池-订书钉」
// 这类口令，强度会远低于它看起来的样子。
//
// 24 字节 → 32 个字符，约 192 位熵。
func GeneratePassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机密码失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
