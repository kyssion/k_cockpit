package authz

import (
	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/auth"
)

// Viewer 是数据访问的视角，用于归属过滤（f-1-06 R-004）。
//
// 它必须在**查询构造处强制注入**，而不是让每个调用方自己记得加条件：
// 漏加一处就是一次越权，而这类遗漏在代码审查中很难被发现。
//
// 判定规则（f-1-06 §3.1）：
//   - `admin` 不受归属限制；
//   - `tenant` 只能访问 `owner_id` 等于自己的资源。
type Viewer struct {
	UserID  int64
	IsAdmin bool
}

// ViewerOf 从当前请求构造视角。
//
// 用户为 nil 时返回零值视角——它的 IsAdmin 为 false，会使查询过滤到
// `owner_id = 0`（不存在），即「什么都查不到」。这是**安全失败方向**：
// 未认证的请求不应该看到任何数据，而不是看到全部。
func ViewerOf(c *app.RequestContext) Viewer {
	user := auth.CurrentUser(c)
	if user == nil {
		return Viewer{}
	}
	return Viewer{UserID: user.ID, IsAdmin: user.IsAdmin()}
}
