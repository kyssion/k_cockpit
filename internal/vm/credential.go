package vm

import (
	"context"
	"errors"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
)

// InitialCredentialView 是详情页展示的初始登录凭据。
//
// Username 来自创建时的系统类型推断（root / Administrator），真实用户名取决于
// 镜像——它在来宾自动化里可改，改完之后这里展示的就是「最新的那份凭据」。
type InitialCredentialView struct {
	Has       bool   `json:"has"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// InitialCredentialOf 返回一台虚拟机的登录凭据（G-30）。
//
// 三道边界缺一不可：
//  1. **归属校验**（load）：404 而不是 403，不确认资源是否存在；
//  2. **审计留痕**：明文出站必须可追溯。审计记「谁读了哪台机器的凭据」，
//     绝不记密码本身——审计表是排查时要看的东西，不能同时是泄漏源；
//  3. **来源区分**：只返回初始凭据行（username 非 _vnc），控制台密码是
//     协议认证用的，展示它没有意义且有额外风险。
//
// 刻意**不套二次验证**（428）：初始密码是创建者自己设置的，读它不改变系统
// 状态；归属与审计已经覆盖了滥用面。套上 428 只会让演示与日常使用变重，
// 而威胁模型并没有因此变好。
func (s *Service) InitialCredentialOf(
	ctx context.Context, vmID int64, v authz.Viewer, operatorName, clientIP string,
) (*InitialCredentialView, error) {
	vm, err := s.load(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	var row model.VMCredential
	err = s.db.WithContext(ctx).
		Where("vm_id = ? AND username <> ?", vm.ID, model.CredentialVNC).
		Order("id").First(&row).Error
	if err != nil {
		// 没有记录不是错误：创建时没给初始密码的机器本来就没有。前端据此
		// 显示「未设置」引导区，而不是一个读不懂的报错。
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &InitialCredentialView{Has: false}, nil
		}
		log.Printf("[vm] 查询初始凭据失败 vm=%d: %v", vm.ID, err)
		return nil, api.Internal()
	}

	plain, err := cryptoutil.Open(s.encKey, row.PasswordEnc)
	if err != nil {
		// 解密失败通常是密钥轮换后未迁移密文。给用户的必须是可执行的
		// 提示：重设一次密码即可——凭据展示的意义就是让人能登进机器。
		// 用 ValidationFailed 而不是 Internal 是刻意的：它带着「怎么办」，
		// 而笼统的 500 会让用户反复点这个按钮。
		log.Printf("[vm] 解密初始凭据失败 vm=%d: %v", vm.ID, err)
		return nil, api.ValidationFailed("凭据密文无法解密（服务密钥可能已更换），请通过「重置密码」重新设置")
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:  "vm.credential.read",
		Success: true, ClientIP: clientIP,
	})

	return &InitialCredentialView{
		Has:       true,
		Username:  derefStr(row.Username),
		Password:  plain,
		CreatedAt: row.CreatedAt.Format("2006-01-02 15:04:05"),
	}, nil
}
