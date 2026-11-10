package platform_test

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"k_cockpit/internal/model"
)

// TestTOTPRebindStaysGuardedWhilePending 守住「重绑开始之后仍要验证」。
//
// 实机缺陷（r18）：BeginTOTPSetup 一执行就把 totp_enabled 置为 false（旧
// 密钥按设计当场作废）。守卫当时只看 TOTPEnabled，于是**第一次**重绑要求
// 验证、**第二次**起就不要求了——任何拿着会话的人只要在受害者发起过一次
// 重绑之后再来一次，就能把自己的密钥写成待确认状态，然后用自己算出的码
// 确认。第二因子就这样在两步之内被换掉，而守卫全程没有报错。
//
// 现在守卫同时看「曾绑定过」（恢复码还在）：pending 也要过一次验证。首次
// 绑定（从未有过因子、也没有恢复码）不受影响。
func TestTOTPRebindStaysGuardedWhilePending(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "rebind", model.RoleTenant)
	cookie, secret := bindTOTP(t, h, "rebind")

	// 1) 已启用：不带 grant 必须 428。
	w := ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/totp/setup", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("已启用时重绑状态码 = %d，期望 428（body=%s）", w.Code, w.Body.String())
	}

	// 2) 带 grant：允许开始重绑，账号进入 pending。
	grant := issueRiskGrant(t, h, cookie, secret)
	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/totp/setup", nil,
		ut.Header{Key: "Cookie", Value: cookie},
		ut.Header{Key: "X-Risk-Grant", Value: grant})
	if w.Code != consts.StatusOK {
		t.Fatalf("带 grant 重绑状态码 = %d，期望 200（body=%s）", w.Code, w.Body.String())
	}

	// 3) pending 状态：再来一次仍然必须 428。
	//
	// 这一步是整条用例的重点。放宽成 200 的话，攻击者只需要等受害者点过
	// 一次「重新绑定」（或者自己制造一次），就能无验证地接管第二因子。
	w = ut.PerformRequest(h.Engine, "POST", "/api/v1/auth/totp/setup", nil,
		ut.Header{Key: "Cookie", Value: cookie})
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("pending 状态下重绑状态码 = %d，期望 428（body=%s）", w.Code, w.Body.String())
	}
}
