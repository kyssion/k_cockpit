package agent

// 宿主机上与该用户有关的设置。
//
// 控制面**不持有到宿主机的登录通道**（迁移 0002 已删掉 ssh_host / ssh_user
// 那些列），因此这里只下发动作，不传任何凭据。
const (
	// OpUserSSHAccess 启用 / 禁用某个用户在宿主机上的 SSH 访问。
	//
	// 关闭时必须**一并结束在线会话**：只改配置而人还在里面，等于把"禁止访问"
	// 变成了一句只对下次登录生效的话。
	OpUserSSHAccess OpKind = "user.ssh.access"
)

// UserSSHAccessDataKey 是 SSH 访问设置结果的键。
const UserSSHAccessDataKey = "user_ssh_access"

// UserSSHAccessInfo 是 SSH 访问设置的结果。
type UserSSHAccessInfo struct {
	// Applied 表示节点侧已生效。
	Applied bool
	// KilledSessions 是被结束的在线会话数。
	//
	// 回显它是为了让界面能说清"改了配置，并且把已经进去的人请出去了"——
	// 否则用户无从判断这次操作是否真的生效。
	KilledSessions int
	Message        string
}
