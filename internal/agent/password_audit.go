package agent

// OpSecurityPasswordAudit 检查账号口令是否属于弱口令或已知泄露口令。
//
// 它必须在节点侧执行：判定需要一份清单（或以 k-anonymity 方式查询外部服务），
// 而控制面只有 argon2 哈希，且**不应为了这项检查去联网**。
const OpSecurityPasswordAudit OpKind = "security.password_audit"

// PasswordAuditDataKey 是检查结果的键。
const PasswordAuditDataKey = "password_audit"

// PasswordHit 是一个命中项。
type PasswordHit struct {
	// Username 而不是 UserID：节点只知道系统账号名，控制面负责把它映射回
	// 面板里的用户。
	Username string
	Reason   string
}

// PasswordAuditInfo 是一次口令检查的结果。
type PasswordAuditInfo struct {
	// Checked 是本次实际检查的账号数。
	Checked int
	Hits    []PasswordHit
	Message string
	// Unavailable 非空表示节点没有可用的判定依据（例如没有清单）。
	Unavailable string
}
