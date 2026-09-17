// Package apikey 实现 API 凭证与一次性动作令牌（F-1-10）。
//
// 有一条必须被**明示**的取舍贯穿本包，PRD 也专门点了它：
//
//	**API Key 调用不触发交互式二次验证。**
//
// 换句话说，一个泄漏的 Key 等同于**绕过所有高风险操作的验证**——删除虚拟
// 机、解锁业务软锁、开放防火墙端口……那些在浏览器里都需要过一次验证码的
// 操作，用 Key 调用时直接执行。
//
// 这不是疏忽，而是一个必要的取舍：二次验证要求**有人在那一端输入**，而
// Key 的使用场景恰恰是「没有人在那一端」。一个需要人工确认的 API 只能被
// 脚本用它做不到的方式调用。
//
// 因此本包把代价放在别处补偿：
//
//   - 明文只返回一次，库里只存哈希；
//   - 可以绑定来源 IP——Key 泄漏后唯一还能挡住攻击者的东西；
//   - 可以设固定到期；
//   - 记录最后使用时间，异常使用看得出来；
//   - 撤销不删记录，事后能查「什么时候被谁撤掉的」。
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/model"
)

// Service 提供 API 凭证能力。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{db: db, audit: recorder, now: time.Now}
}

// KeyPrefix 是明文凭证的固定前缀。
//
// 它让凭据**一眼可辨**：日志里、配置里、代码里出现 `kc_` 开头的一串，
// 任何人事先就知道那是什么，而不必去猜。这也是它出现在这么多 SDK 里的
// 原因——泄漏检测工具同样靠它。
const KeyPrefix = "kc_"

// 生成参数。
const (
	// keyEntropyBytes 决定密钥长度。32 字节 = 256 位，穷举不可行。
	keyEntropyBytes = 32
	// prefixLen 是展示用前缀的长度（含 `kc_`）。太短会撞车，太长则接近
	// 泄漏完整密钥的信息量。
	prefixLen = 12
	// actionTokenTTL 是一次性令牌的有效期。
	//
	// 只给 5 分钟：它从签发出现在 URL 里，到浏览器发起下载通常只有几秒。
	// 有效期越长，出现在浏览器历史与日志里的那个令牌就越危险。
	actionTokenTTL = 5 * time.Minute
)

// View 是 API 凭证的对外视图。
//
// **不含明文，也不含哈希**。明文只在创建时返回过一次；哈希即使泄漏也无
// 用处，把它回传只会让它出现在浏览器的开发者工具与前端缓存里。
type View struct {
	// Exists 为 false 表示该用户还没有生成过 Key。
	Exists bool `json:"exists"`
	// Prefix 是明文前几位，供识别。
	Prefix string `json:"prefix,omitempty"`
	// AllowedIPs 为空表示不限来源。
	AllowedIPs []string `json:"allowed_ips"`
	// ExpiresAt 为空表示永不过期。
	ExpiresAt  string `json:"expires_at,omitempty"`
	Expired    bool   `json:"expired"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	// Usable 是该 Key 当前是否可用（未撤销、未过期）。
	Usable bool `json:"usable"`
}

// Created 是创建结果：**唯一一次**携带明文的响应。
type Created struct {
	// PlainKey 是明文凭证。
	//
	// 它只在这个响应里出现一次——库里存的是哈希，服务端之后**再也拿不到
	// 它**。因此界面必须明确告知用户「现在就复制，关掉就没了」，而不是
	// 像别的表单那样可以随时回来再看。
	PlainKey string `json:"plain_key"`
	View
}

// Get 返回用户的凭证信息。
func (s *Service) Get(ctx context.Context, userID int64) (*View, error) {
	var key model.UserAPIKey
	err := s.db.WithContext(ctx).Where("user_id = ?", userID).First(&key).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &View{Exists: false, AllowedIPs: []string{}}, nil
	}
	if err != nil {
		log.Printf("[apikey] 查询凭证失败: %v", err)
		return nil, api.Internal()
	}
	return s.toView(&key), nil
}

// CreateRequest 是生成凭证的请求。
type CreateRequest struct {
	// AllowedIPs 为空表示不限来源。
	AllowedIPs []string
	// ExpiresInDays 为 0 表示永不过期。
	//
	// 允许永不过期是因为把它用在长期运行的自动化上时，定期轮换有真实的
	// 运维成本；但界面上会提示它意味着**一次泄漏永久有效**。
	ExpiresInDays int
}

// Create 生成凭证（或**轮换**已有的那个）。
//
// 已有凭证时**覆盖**而不是再建一条：唯一索引决定了一人一个，而「重新生成」
// 在语义上就是轮换——旧的立即失效。这一点必须在界面上说清楚，否则用户会
// 以为新 Key 和旧 Key 能并存，直到某个自动化任务突然开始报 401。
func (s *Service) Create(
	ctx context.Context, userID int64, req CreateRequest,
	operatorName, clientIP string,
) (*Created, error) {
	allowed, err := normalizeIPs(req.AllowedIPs)
	if err != nil {
		return nil, err
	}
	if req.ExpiresInDays < 0 {
		return nil, api.InvalidParameter("有效天数不能为负")
	}
	if req.ExpiresInDays > 3650 {
		return nil, api.InvalidParameter("有效天数最多 3650 天（约 10 年）")
	}

	plain, hash, err := generateKey()
	if err != nil {
		log.Printf("[apikey] 生成随机凭证失败: %v", err)
		return nil, api.Internal()
	}

	var expiresAt *time.Time
	if req.ExpiresInDays > 0 {
		t := s.now().AddDate(0, 0, req.ExpiresInDays)
		expiresAt = &t
	}
	var allowedPtr *string
	if len(allowed) > 0 {
		joined := strings.Join(allowed, "\n")
		allowedPtr = &joined
	}

	// 覆盖式轮换放在一个事务里：分两步写会出现「旧的已撤、新的没建好」
	// 的瞬间，而那段时间里所有自动化任务都会 401。
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).
			Delete(&model.UserAPIKey{}).Error; err != nil {
			return err
		}
		row := model.UserAPIKey{
			UserID: userID, KeyPrefix: plain[:prefixLen], KeyHash: hash,
			AllowedIPs: allowedPtr, ExpiresAt: expiresAt,
		}
		return tx.Create(&row).Error
	})
	if err != nil {
		log.Printf("[apikey] 写入凭证失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: userID, OperatorName: operatorName,
		ResourceType: "api_key", ResourceName: plain[:prefixLen] + "…",
		Action: "api_key.create",
		Params: map[string]any{
			"allowed_ips": allowed, "expires_in_days": req.ExpiresInDays,
			// 记录「轮换掉了旧的那个」——事后追查「我的 Key 什么时候失效的」
			// 时，唯一能回答的就是这条。
			"rotated": true,
		},
		Success: true, ClientIP: clientIP,
	})

	view, err := s.Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &Created{PlainKey: plain, View: *view}, nil
}

// Revoke 撤销凭证。
//
// **不删记录**，只写 revoked_at：留着才能回答「这个 Key 是什么时候被撤掉
// 的」，而那正是出事之后要查的东西。删掉它，用户看到的是「我从没生成过
// Key」——与事实不符，且无从追查。
func (s *Service) Revoke(
	ctx context.Context, userID int64, operatorName, clientIP string,
) error {
	res := s.db.WithContext(ctx).Model(&model.UserAPIKey{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", s.now())
	if res.Error != nil {
		log.Printf("[apikey] 撤销凭证失败: %v", res.Error)
		return api.Internal()
	}
	if res.RowsAffected == 0 {
		return api.NotFound("没有可撤销的 API 凭证")
	}

	s.record(ctx, audit.Entry{
		OperatorID: userID, OperatorName: operatorName,
		ResourceType: "api_key", Action: "api_key.revoke",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// Authenticate 校验一个明文凭证。
//
// 返回 nil 表示不通过。**不区分失败原因**：凭据不存在、已撤销、已过期、
// 来源 IP 不匹配——对外一律是同一句「凭证无效」。
//
// 区分它们会让攻击者能通过响应差异**枚举出有效的前缀**（「这个前缀存在
// 但过期了」已经泄漏了一条信息）；而对合法用户来说，「无效」与「过期」
// 的处理方式本来就一样：重新生成一个。
// 返回的是 auth.APIKeyPrincipal 而不是本包自己的类型：中间件按接口
// （auth.APIKeyAuth）调用它，类型必须**就是**接口里声明的那个。各定义一个
// 形状相同的结构体只会让两边需要一层手写的转换，而那层转换是纯粹的成本。
func (s *Service) Authenticate(ctx context.Context, plain, fromIP string) (*auth.APIKeyPrincipal, error) {
	plain = strings.TrimSpace(plain)
	if !strings.HasPrefix(plain, KeyPrefix) {
		return nil, nil
	}
	hash := hashKey(plain)

	var key model.UserAPIKey
	if err := s.db.WithContext(ctx).
		Where("key_hash = ?", hash).First(&key).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("[apikey] 查询凭证失败: %v", err)
			return nil, api.Internal()
		}
		return nil, nil
	}

	now := s.now()
	if !key.IsUsable(now) {
		return nil, nil
	}
	if !ipAllowed(key.AllowedIPs, fromIP) {
		return nil, nil
	}

	// 更新最后使用时间。
	//
	// 失败**不阻断认证**：它只是一个观测字段，而为了写它把一次完全合法的
	// 调用判为失败，代价与收益完全不成比例。
	if err := s.db.WithContext(ctx).Model(&model.UserAPIKey{}).
		Where("id = ?", key.ID).
		Update("last_used_at", now).Error; err != nil {
		log.Printf("[apikey] 更新最后使用时间失败: %v", err)
	}

	return &auth.APIKeyPrincipal{UserID: key.UserID, KeyID: key.ID, Prefix: key.KeyPrefix}, nil
}

// --- 一次性动作令牌 ---

// IssueActionToken 签发一个一次性令牌，返回明文。
//
// 它解决的是「浏览器之外的下载」：`<a href>` 带不上自定义请求头，因此
// 用 Cookie 认证的接口没法直接下给用户。做法是先用会话换一个短期令牌，
// 再把令牌放进 URL。
func (s *Service) IssueActionToken(
	ctx context.Context, userID int64, purpose string,
) (string, time.Time, error) {
	if purpose == "" {
		return "", time.Time{}, api.InvalidParameter("必须指定用途")
	}

	plain, hash, err := generateKey()
	if err != nil {
		log.Printf("[apikey] 生成令牌失败: %v", err)
		return "", time.Time{}, api.Internal()
	}
	expiresAt := s.now().Add(actionTokenTTL)

	row := model.AuthActionToken{
		UserID: userID, Purpose: purpose, TokenHash: hash, ExpiresAt: expiresAt,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[apikey] 写入令牌失败: %v", err)
		return "", time.Time{}, api.Internal()
	}
	return plain, expiresAt, nil
}

// ConsumeActionToken 校验并**消费**一个令牌（一次性）。
//
// 消费是在**同一个 UPDATE 语句**里完成的（`used_at IS NULL` 作为条件），
// 而不是「先查、再改」：后者在并发下会让同一个令牌被换出去两次——而
// 一次性正是这个令牌存在的全部理由。
func (s *Service) ConsumeActionToken(
	ctx context.Context, plain, purpose string,
) (int64, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return 0, api.Unauthenticated("令牌无效")
	}

	var row model.AuthActionToken
	err := s.db.WithContext(ctx).
		Where("token_hash = ? AND purpose = ?", hashKey(plain), purpose).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, api.Unauthenticated("令牌无效")
	}
	if err != nil {
		log.Printf("[apikey] 查询令牌失败: %v", err)
		return 0, api.Internal()
	}
	if !row.IsUsable(s.now()) {
		return 0, api.Unauthenticated("令牌已失效")
	}

	res := s.db.WithContext(ctx).Model(&model.AuthActionToken{}).
		Where("id = ? AND used_at IS NULL", row.ID).
		Update("used_at", s.now())
	if res.Error != nil {
		log.Printf("[apikey] 消费令牌失败: %v", res.Error)
		return 0, api.Internal()
	}
	// RowsAffected 为 0 说明另一个并发请求先一步换走了它。
	if res.RowsAffected == 0 {
		return 0, api.Unauthenticated("令牌已失效")
	}
	return row.UserID, nil
}

// --- 内部 ---

func (s *Service) toView(k *model.UserAPIKey) *View {
	now := s.now()
	view := &View{
		Exists: true, Prefix: k.KeyPrefix,
		AllowedIPs: splitLines(k.AllowedIPs),
		Expired:    k.ExpiresAt != nil && now.After(*k.ExpiresAt),
		Usable:     k.IsUsable(now),
		CreatedAt:  k.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if k.ExpiresAt != nil {
		view.ExpiresAt = k.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if k.RevokedAt != nil {
		view.RevokedAt = k.RevokedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if k.LastUsedAt != nil {
		view.LastUsedAt = k.LastUsedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

// generateKey 生成明文与它的哈希。
//
// 用 crypto/rand 而不是 math/rand：后者是可预测的，而一个可预测的 API
// 凭据等于没有凭据——攻击者只需要知道种子就能算出所有用户的 Key。
func generateKey() (plain, hash string, err error) {
	buf := make([]byte, keyEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = KeyPrefix + hex.EncodeToString(buf)
	return plain, hashKey(plain), nil
}

func hashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// normalizeIPs 校验并规范化来源 IP 列表。
func normalizeIPs(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, raw := range list {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if ip := net.ParseIP(value); ip != nil {
			// 单个地址补上掩码长度：留着裸地址会让比对逻辑需要处理两种
			// 写法，而两种写法在字符串比较时又互不相等。
			if ip.To4() != nil {
				value += "/32"
			} else {
				value += "/128"
			}
		} else if _, _, err := net.ParseCIDR(value); err != nil {
			return nil, api.InvalidParameter(
				"来源 IP 必须是地址或网段，例如 203.0.113.7 或 10.0.0.0/8")
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	// 排序让同一份配置每次渲染的顺序一致——顺序会变的列表会让人怀疑
	// 自己看错了，而来源白名单恰恰是出事时要逐条核对的东西。
	sort.Strings(out)
	return out, nil
}

// ipAllowed 判断来源是否被允许；列表为空表示不限。
func ipAllowed(allowed *string, fromIP string) bool {
	list := splitLines(allowed)
	if len(list) == 0 {
		return true
	}
	parsed := net.ParseIP(strings.TrimSpace(fromIP))
	if parsed == nil {
		// 拿不到来源地址时**拒绝**而不是放行：绑定 IP 的全部意义就是
		// 「只能从这里来」，而一个说不清来源的请求无法满足这个条件。
		return false
	}
	for _, cidr := range list {
		if _, network, err := net.ParseCIDR(cidr); err == nil && network.Contains(parsed) {
			return true
		}
	}
	return false
}

func splitLines(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return []string{}
	}
	fields := strings.FieldsFunc(*raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}
