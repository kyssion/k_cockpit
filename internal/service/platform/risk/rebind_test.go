package risk

import "testing"

// 重新绑定验证器必须在清单里。
//
// 它看起来像「加一种验证方式」，实际是换锁：BeginTOTPSetup 一执行，旧密钥
// 立刻失效。若不过一次二次验证，拿到会话 Cookie 的人就能把第二因子换成
// 自己的验证器，随后所有删除类操作对他都是「已验证」。这条用例守住的是
// 清单本身——判定一旦被删掉，接口层就再没有别的拦截点。
func TestTOTPRebindIsProtected(t *testing.T) {
	if !Protected(ActionTOTPRebind) {
		t.Fatal("重新绑定验证器必须受二次验证保护")
	}

	var found bool
	for _, e := range Policy() {
		if e.Action != ActionTOTPRebind {
			continue
		}
		found = true
		if e.Label == "" || e.Reason == "" {
			t.Errorf("清单项缺少展示文案: %+v", e)
		}
	}
	if !found {
		t.Fatal("ActionTOTPRebind 不在 Policy() 返回的清单里，前端拿不到它")
	}
}
