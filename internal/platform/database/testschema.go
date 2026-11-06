package database

import (
	"fmt"
	"log"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// AllModels 列出全部表模型，按 model 文件逐一登记。
//
// 用途有三：本地开发建库（cmd/dev-init）、E2E 冒烟栈建库
// （cmd/e2e-init）与少数需要全套表的测试。
// 新增模型时**必须**同步本清单与 internal/model/——漏掉的表现为开发库
// 在运行期报「no such table」，而不是编译错误。
//
// 注意这与「服务启动不做自动迁移」的约定（AGENTS.md）不冲突：本函数
// **只在开发与测试工具里调用**，生产库一律走 migrations/ 的 SQL 迁移；
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
	if err := db.AutoMigrate(AllModels()...); err != nil {
		return err
	}
	return rebuildConditionalIndexes(db)
}

// conditionalIndex 是一条「定义里带 WHERE 条件」的唯一索引。
type conditionalIndex struct {
	model any
	name  string
}

// conditionalIndexes 列出必须按模型**重建**的条件唯一索引。
//
// 为什么需要它：AutoMigrate 只创建「名字还不存在」的索引（HasIndex 命中就
// 跳过），因此把 `uniqueIndex:uniq_x` 改成
// `uniqueIndex:uniq_x,where:deleted_at IS NULL` 之后，**已有库里的旧索引会被
// 原样保留**——AutoMigrate 报成功，约束却仍是旧语义（删掉的记录永久占住
// 名字）。PostgreSQL 走 migrations/ 的显式迁移，没有这个问题；SQLite 的
// 表结构由模型派生，只能在这里补一次「删了重建」。
//
// 这次补它的直接原因是第 3 轮 DEF-R3-12：真机库里 28 条软删记录占着
// uniq_vpc_switch_node_name，同名重建必然失败。不重建的话，改完模型与迁移
// 在 SQLite 上仍然不生效，而 dev-init 看起来"成功"。
//
// 重建是安全的：新约束只会**更宽松**（排除软删行），旧库里能通过旧约束的
// 数据必然也能通过新约束。登记在这里的索引必须同时出现在 migrations/ 里，
// 否则 index_alignment_test 会拦下来。
var conditionalIndexes = []conditionalIndex{
	{model: &model.VpcSwitch{}, name: "uniq_vpc_switch_node_name"},
	{model: &model.VpcSwitch{}, name: "uniq_vpc_switch_node_vlan"},
	// 告警的三列唯一索引（第 3 轮 DEF-R4-01）：真机上原有的是单列
	// `(resource_id)`，而正确形状是 `(kind, resource_type, resource_id)`。
	// AutoMigrate 只看索引名是否存在，**不会**因为列变了而重建——不在这里
	// 登记的话，改完模型与迁移在 SQLite 上仍然不生效（缺陷照旧）。
	{model: &model.Alert{}, name: "uniq_alert_kind_resource"},
	// 模板同名唯一索引（第 3 轮 DEF-R3-06）：模板是软删除，旧索引不排除
	// 已删行 → 删掉同名模板后重新导入必然撞约束，而预览（GORM 自动过滤
	// 软删行）却说"可以导入"。两处判定必须落在同一个口径上。
	{model: &model.Template{}, name: "uniq_template_node_name"},
}

// rebuildConditionalIndexes 删掉再按模型建回这些索引；不存在时跳过。
func rebuildConditionalIndexes(db *gorm.DB) error {
	m := db.Migrator()
	for _, idx := range conditionalIndexes {
		if !m.HasIndex(idx.model, idx.name) {
			continue // 新建的库：AutoMigrate 已按模型（含条件）建好
		}
		if err := m.DropIndex(idx.model, idx.name); err != nil {
			return fmt.Errorf("重建索引 %s 失败（删除阶段）: %w", idx.name, err)
		}
		if err := m.CreateIndex(idx.model, idx.name); err != nil {
			return fmt.Errorf("重建索引 %s 失败（创建阶段）: %w", idx.name, err)
		}
		log.Printf("[database] 已按模型重建条件唯一索引 %s", idx.name)
	}
	return nil
}
