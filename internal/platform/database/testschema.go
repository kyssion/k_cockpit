package database

import (
	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// AllModels 列出全部表模型，按 model 文件逐一登记。
//
// 用途有二：E2E 冒烟栈建库（cmd/e2e-init）与少数需要全套表的测试。
// 新增模型时**必须**同步本清单与 internal/model/——漏掉的表现为冒烟栈
// 在运行期报「no such table」，而不是编译错误。
//
// 注意这与「服务启动不做自动迁移」的约定（AGENTS.md）不冲突：本函数
// **只在测试与冒烟工具里调用**，生产库一律走 migrations/ 的 SQL 迁移；
// 两套结构的一致性由 model_alignment_test 对齐检查兜底。
func AllModels() []any {
	return []any{
		&model.Alert{}, &model.AuditLog{}, &model.AuthActionToken{},
		&model.CPUAffinityPreset{}, &model.ComputeQuota{},
		&model.EmailVerification{},
		&model.FirewallPolicy{}, &model.FirewallRule{}, &model.FirewallVMPolicy{},
		&model.HostFirewallPolicy{}, &model.HostFirewallRule{},
		&model.HostStatsRecord{},
		&model.ImageImport{}, &model.InterfaceSecurityGroup{},
		&model.NetworkBridge{}, &model.NetworkCapture{}, &model.Node{},
		&model.PortForward{}, &model.PortMirror{}, &model.PortSecurityPolicy{},
		&model.PublicIP{}, &model.PublicIPBinding{},
		&model.RequestLog{}, &model.ResourceQuota{},
		&model.SchedulerEvent{},
		&model.SecurityGroup{}, &model.SecurityGroupRule{},
		&model.Session{}, &model.SessionSigningKey{}, &model.ShareMount{},
		&model.SiteMaintenance{}, &model.StaticIP{},
		&model.StorageFile{}, &model.StoragePool{}, &model.StorageVolume{},
		&model.SystemSetting{},
		&model.Task{}, &model.TaskStage{}, &model.Template{}, &model.TemplateExport{},
		&model.TrafficStatDaily{},
		&model.UploadSession{}, &model.User{}, &model.UserAPIKey{},
		&model.UserInvite{}, &model.UserStorage{},
		&model.VM{}, &model.VMCDROM{}, &model.VMCredential{}, &model.VMExport{},
		&model.VMInterface{}, &model.VMLock{}, &model.VMMigration{},
		&model.VMPassthrough{}, &model.VMRuntimeDaily{}, &model.VMSchedule{},
		&model.VMSnapshot{}, &model.VMStatsRecord{}, &model.VMTag{},
		&model.VpcACLRule{}, &model.VpcSwitch{},
	}
}

// BuildTestSchema 用 AutoMigrate 建出全套表（仅测试/冒烟，见 AllModels）。
func BuildTestSchema(db *gorm.DB) error {
	return db.AutoMigrate(AllModels()...)
}
