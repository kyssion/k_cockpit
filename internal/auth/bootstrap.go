package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log"
	"regexp"
	"strings"
	"sync"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
)

// MinPasswordLength 是新建管理员的最小密码长度。
//
// 管理员密码可完全接管面板，长度下限高于普通用户；更完整的密码策略
// （强度校验、泄露检测）属 F-10-05，后续接入。
const MinPasswordLength = 12

// usernamePattern 限定用户名可用字符，避免出现难以输入或易混淆的形式。
var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,64}$`)

// errInvalidBootstrapToken 是初始化令牌校验失败的统一错误。
//
// 不区分「令牌为空」「令牌错误」「系统已初始化」的对外文案细节——
// 后者的含义在响应里已通过 409 表达，不需要再从文案推断。
var errInvalidBootstrapToken = api.PermissionDenied("初始化令牌无效")

// Bootstrap 负责首个管理员的创建，解决「系统尚无账号、而创建账号本身
// 需要权限」的自举问题（见 docs/06-decisions/0008-first-admin-bootstrap.md）。
//
// 设计要点：**没有默认凭据**。服务启动时若检测到系统中没有管理员，生成
// 一次性令牌并交由调用方打印到控制台日志——能读到日志即等价于拥有服务器
// 访问权，这正是「谁有权初始化」的合理判据。
type Bootstrap struct {
	db    *gorm.DB
	audit *audit.Recorder

	mu    sync.Mutex
	token string // 空表示当前不需要初始化
}

// NewBootstrap 检测系统状态，并在未初始化时生成一次性令牌。
//
// 返回的令牌由调用方打印到启动日志：本包不自行打日志，避免令牌出现在
// 非预期的输出位置。token 为空字符串表示系统已初始化。
func NewBootstrap(db *gorm.DB, recorder *audit.Recorder) (*Bootstrap, string, error) {
	b := &Bootstrap{db: db, audit: recorder}

	initialized, err := b.Initialized(context.Background())
	if err != nil {
		return nil, "", err
	}
	if initialized {
		return b, "", nil
	}

	token, err := newBootstrapToken()
	if err != nil {
		return nil, "", err
	}
	b.token = token
	return b, token, nil
}

// Initialized 报告系统中是否已存在管理员。
//
// **每次实时查库**，不缓存也不依赖启动时的判定：这是安全边界，缓存失效
// 或标记位出错会让初始化接口重新开放。调用方亦不得据此做长期缓存。
func (b *Bootstrap) Initialized(ctx context.Context) (bool, error) {
	var count int64
	err := b.db.WithContext(ctx).Model(&model.User{}).
		Where("role = ? AND deleted_at IS NULL", model.RoleAdmin).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// CreateAdmin 使用一次性令牌创建首个管理员。
//
// 成功后令牌立即失效；系统一旦存在管理员，本方法无条件拒绝。
func (b *Bootstrap) CreateAdmin(
	ctx context.Context, token, username, password, clientIP string,
) (*model.User, error) {
	if err := validateNewAdmin(username, password); err != nil {
		return nil, err
	}

	// 加锁：两个并发请求都通过了令牌校验时，必须有一个失败，
	// 否则会创建出两个管理员。
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.token == "" || !constantTimeEqual(token, b.token) {
		b.record(ctx, username, clientIP, false, "初始化令牌无效")
		return nil, errInvalidBootstrapToken
	}

	initialized, err := b.Initialized(ctx)
	if err != nil {
		log.Printf("[bootstrap] 查询初始化状态失败: %v", err)
		return nil, api.Unavailable("初始化服务暂时不可用")
	}
	if initialized {
		b.record(ctx, username, clientIP, false, "系统已初始化")
		return nil, api.Conflict("系统已完成初始化")
	}

	hash, err := HashPassword(password)
	if err != nil {
		log.Printf("[bootstrap] 生成密码哈希失败: %v", err)
		return nil, api.Internal()
	}

	user := model.User{
		Username:     username,
		PasswordHash: hash,
		Role:         model.RoleAdmin,
		Status:       model.UserStatusActive,
	}
	if err := b.db.WithContext(ctx).Create(&user).Error; err != nil {
		log.Printf("[bootstrap] 创建管理员失败: %v", err)
		return nil, api.Internal()
	}

	// 令牌一次性：创建成功即作废，同一令牌无法再创建第二个管理员。
	b.token = ""

	b.record(ctx, username, clientIP, true, "")
	return &user, nil
}

// validateNewAdmin 校验首个管理员的用户名与密码。
func validateNewAdmin(username, password string) error {
	if !usernamePattern.MatchString(username) {
		return api.InvalidParameter("用户名需为 3-64 位的字母、数字、下划线或连字符")
	}
	if len([]rune(password)) < MinPasswordLength {
		return api.InvalidParameter("密码长度不得少于 12 个字符")
	}
	if strings.Contains(strings.ToLower(password), strings.ToLower(username)) {
		return api.InvalidParameter("密码不能包含用户名")
	}
	return nil
}

func (b *Bootstrap) record(ctx context.Context, username, clientIP string, success bool, reason string) {
	if b.audit == nil {
		return
	}
	// 参数与令牌一律不入审计：令牌等同于初始化权限。
	b.audit.Record(ctx, audit.Entry{
		Source:       model.SourceWeb,
		ResourceType: "user",
		ResourceName: username,
		Action:       "system.bootstrap_admin",
		Success:      success,
		Error:        reason,
		ClientIP:     clientIP,
	})
}

// constantTimeEqual 用常量时间比较令牌，避免被计时推断。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func newBootstrapToken() (string, error) {
	var buf [24]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", errors.New("生成初始化令牌失败: " + err.Error())
	}
	return hex.EncodeToString(buf[:]), nil
}
