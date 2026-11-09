package template_test

import (
	"context"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/template"
)

// readyTemplate 直接写一条可预处理的模板记录（created_by=7，属主视角可见）。
func readyTemplate(t *testing.T, db *gorm.DB, name string) *model.Template {
	t.Helper()
	disk := "/var/lib/k_cockpit/templates/" + name + ".qcow2"
	row := model.Template{
		NodeID: 1, Name: name, Status: model.TemplateReady, Version: 1,
		CreatedBy: ptr(int64(7)), CloneEnabled: true,
		Visibility: model.TemplatePrivate, DiskPath: &disk,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建模板失败: %v", err)
	}
	return &row
}

// TestPreprocessRejectsMalformedSSHKey 覆盖「公钥畸形时才在镜像改写阶段炸」。
//
// 实机现象：virt-customize 报 `failed to parse public key`，而那时任务已经
// 跑了半分钟、模板盘已被备份复制一遍。控制面完全有能力在**受理**时看出
// 这段内容不是一行公钥，因此畸形输入应当当场 422，并说清该怎么填。
func TestPreprocessRejectsMalformedSSHKey(t *testing.T) {
	svc, db := newTestEnv(t, model.VMStatusStopped)
	ctx := context.Background()
	tpl := readyTemplate(t, db, "tpl-key")
	viewer := authz.Viewer{UserID: 7}

	for _, bad := range []string{
		"ssh-rsa",                             // 只有类型（用户以为填了"算法"就够）
		"/home/alice/.ssh/id_ed25519.pub",     // 填了公钥文件的路径
		"-----BEGIN OPENSSH PRIVATE KEY-----", // 粘了私钥
		"banana AAAAB3NzaC1yc2E=",             // 未知算法
		"ssh-ed25519 not-base64!! alice@lab",  // 密钥体不是 base64
		"ssh-ed25519",                         // 缺密钥体
	} {
		_, err := svc.Preprocess(ctx, tpl.ID, template.PreprocessOptions{
			InjectSSHKey: true, SSHPublicKey: bad,
		}, true, viewer, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("畸形公钥 %q 应被拒绝", bad)
			continue
		}
		assertStatus(t, err, 422)
	}

	// 合法公钥必须通过——只测拒绝的用例拦不住一个"全都拒绝"的实现。
	if _, err := svc.Preprocess(ctx, tpl.ID, template.PreprocessOptions{
		InjectSSHKey: true,
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIL9v0Zx3RLb7bT0dQ0k9xWJ4mS3nZt2yqk8wT1oV6uHd alice@lab",
	}, true, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("合法公钥应被受理: %v", err)
	}
	// 不注入公钥时不该因为"没填公钥"被拒（空公钥只在选中该项时才是问题）。
	if _, err := svc.Preprocess(ctx, tpl.ID, template.PreprocessOptions{
		ResetMachineID: true,
	}, true, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("不注入公钥时应受理: %v", err)
	}
}
