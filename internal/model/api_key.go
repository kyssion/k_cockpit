package model

import "time"

// UserAPIKey 对应 user_api_key 表：用户的 API 凭证（F-1-10）。
//
// **一个用户只有一个 Key**（uniq_user_api_key_user_id）。这是建库时的设计，
// 它有一个真实的取舍，值得写下来：
//
//	好处：只有一个凭据要管理。用户不需要记住「哪个 key 是给 CI 的、
//	      哪个是给监控的」，泄漏时也只有一个地方要撤销。
//	代价：无法按用途拆分。给三个自动化任务发的其实是同一个凭据，
//	      其中一个泄漏就得全部轮换。
//
// 换成一对多要动那条唯一索引，而它同时是「查 Key 归属」的加速路径——
// 因此这个取舍被保留，并在界面上如实说明。
type UserAPIKey struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64 `gorm:"not null;uniqueIndex:uniq_user_api_key_user_id"`

	// KeyPrefix 是明文的前几位，供界面识别「这是哪一个 Key」。
	//
	// 没有它的话，用户看到列表里的一个 Key 只能靠创建时间去猜；而有了
	// 它，`kc_3f2a…` 一眼就能和手上的凭据对上。**前缀不构成安全风险**：
	// 真正用于校验的是下面那个哈希，而前缀本身不足以还原出完整凭据。
	KeyPrefix string `gorm:"size:16;not null;index:idx_user_api_key_prefix"`

	// KeyHash 是**明文的 SHA-256**（十六进制）。
	//
	// 这里用 SHA-256 而不是用户口令那样的 argon2，理由有两条：
	//
	//  1. **Key 是高熵随机串**（32 字节）。argon2 的慢哈希是为了对抗低熵
	//     口令的暴力破解，而一个 256 位随机串不存在「猜得到」这回事。
	//  2. **它在每个请求上都要校验一次**。argon2 故意设计成每次要花几十
	//     毫秒，那会让每一次 API 调用都背上这个开销——一个自动化任务批量
	//     调用时，这一点会立刻变成瓶颈。
	//
	// 「用慢哈希存凭据」是一条好规则，但它的前提是凭据可能被猜出来。
	// 前提不成立时照搬规则只会付出代价而得不到收益。
	KeyHash string `gorm:"size:128;not null"`

	// AllowedIPs 是允许使用该 Key 的来源（CIDR，换行分隔）；为空表示不限。
	//
	// 这是 Key 泄漏之后**唯一还能挡住攻击者的东西**：即使凭据被拿走，
	// 从别的 IP 发来的请求仍会被拒绝。因此界面上会建议填。
	AllowedIPs *string `gorm:"type:text"`

	// ExpiresAt 为空表示永不过期。
	//
	// 允许「永不过期」是因为把 Key 用在长期运行的自动化上时，定期轮换会
	// 带来真实的运维成本；但界面会提示它意味着**一次泄漏永久有效**。
	ExpiresAt *time.Time

	// RevokedAt 记录撤销时刻。撤销**不删记录**——留着才能回答「这个 Key
	// 是什么时候被谁撤掉的」，而那正是出事之后要查的东西。
	RevokedAt *time.Time

	// LastUsedAt 是最近一次成功使用的时间。
	//
	// 它有两个实际用途：识别「这个 Key 还在用吗」（长期没用就可以撤掉），
	// 以及**发现异常**——一个本该只在 CI 里用的 Key 突然在凌晨被使用。
	LastUsedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (UserAPIKey) TableName() string { return "user_api_key" }

// IsUsable 报告该 Key 在给定时刻是否可用。
func (k *UserAPIKey) IsUsable(now time.Time) bool {
	if k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && now.After(*k.ExpiresAt) {
		return false
	}
	return true
}

// 令牌用途。目前只有一种，但**写成显式取值而不是留空**：用途决定令牌能被
// 换到什么，将来加第二种时，一个空字符串的旧令牌无法判断该不该放行。
const PurposeDownload = "download"

// AuthActionToken 对应 auth_action_token 表：一次性动作令牌。
//
// 它解决的是「浏览器之外的下载」：`<a href>` 带不上自定义请求头，因此
// 用 Cookie 认证的接口没法直接下给用户。做法是先用会话换一个短期令牌，
// 再把令牌放进 URL。
//
// 三条约束缺一不可：
//
//   - **一次性**（used_at）：令牌会出现在 URL 里，也就可能出现在浏览器
//     历史、日志、Referer 头里。允许重复使用等于把一份长期凭据泄漏到那些
//     地方。
//   - **短期**（expires_at）：同上，缩小泄漏窗口。
//   - **绑定用途**（purpose）：下载令牌不该能用来做别的事。
type AuthActionToken struct {
	ID     int64 `gorm:"primaryKey"`
	UserID int64 `gorm:"not null;index:idx_auth_action_token_user_purpose,priority:1"`

	Purpose string `gorm:"size:32;not null;index:idx_auth_action_token_user_purpose,priority:2"`

	// TokenHash 同样是明文的 SHA-256；明文只在签发时返回一次。
	TokenHash string `gorm:"size:128;not null;uniqueIndex:uniq_auth_action_token_token_hash"`

	ExpiresAt time.Time `gorm:"not null"`
	// UsedAt 非空表示已被换过。**一次性**是靠它实现的。
	UsedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (AuthActionToken) TableName() string { return "auth_action_token" }

// IsUsable 报告该令牌在给定时刻是否可用。
func (t *AuthActionToken) IsUsable(now time.Time) bool {
	return t.UsedAt == nil && now.Before(t.ExpiresAt)
}
