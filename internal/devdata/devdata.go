// Package devdata 预置开发环境的演示数据（ADR-0007 mock-first）。
//
// 面板的大量能力以「节点上有资源」为前提：创建向导要求存储池 / 网络 /
// 镜像齐备，模板克隆要有现成模板。mock 运输层下节点运行态是假的，但
// 这些**元数据**是真实的库记录——没有它们，向导的每一步都是空态。
// Ensure 在开发环境启动时补齐这些数据；生产环境永不进入这条路径
// （main 里环境与运输层双重闸门）。
//
// 原则与 node.EnsureSimulated 一致：能走真实领域逻辑的走真实领域逻辑，
// 走不通的（上传、制备都是异步任务流）按真实流程完成后的**最终形态**
// 直接落行，幂等可重复调用。
package devdata

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
	"k_cockpit/internal/network/vswitch"
	"k_cockpit/internal/node"
)

// NodeNames 是预置的模拟节点名，与 node.EnsureSimulated 的调用方约定一致。
var NodeNames = []string{"dev-node-1", "dev-node-2", "dev-node-3"}

// isoSpec 是一台演示镜像的描述。 SizeBytes 取真实发行版的量级，让配额、
// 容量展示有可信的观感。
type isoSpec struct {
	filename  string
	sizeBytes int64
	osType    string
	osVariant string
	minDiskGB int
}

var isoSpecs = []isoSpec{
	{"ubuntu-24.04-live-server.iso", 3_221_225_472, "linux", "ubuntu", 10},
	{"debian-12-amd64-netinst.iso", 629_145_600, "linux", "debian", 8},
	{"Win11_24H2-x64.iso", 5_699_272_704, "windows", "win11", 64},
}

// Deps 是预置所需的协作服务。
type Deps struct {
	DB      *gorm.DB
	Nodes   *node.Service
	SwitchN *vswitch.Service
}

// Ensure 补齐全部演示数据，任一环节失败立即返回（调用方降级告警）。
// 重复调用幂等：节点按名字去重，资源按「该节点是否已有」判断。
func Ensure(ctx context.Context, d Deps) error {
	if err := d.Nodes.EnsureSimulated(ctx, NodeNames); err != nil {
		return fmt.Errorf("预置节点: %w", err)
	}

	var nodes []model.Node
	if err := d.DB.WithContext(ctx).Where("name IN ?", NodeNames).Find(&nodes).Error; err != nil {
		return fmt.Errorf("查询预置节点: %w", err)
	}
	for i := range nodes {
		if err := ensureNodeResources(ctx, d, &nodes[i]); err != nil {
			return err
		}
	}
	return nil
}

// ensureNodeResources 为单台节点补齐存储池 / 系统网络 / 镜像 / 模板。
func ensureNodeResources(ctx context.Context, d Deps, n *model.Node) error {
	if err := ensurePool(ctx, d.DB, n.ID); err != nil {
		return err
	}
	if err := d.SwitchN.EnsureSystemNetwork(ctx, n.ID); err != nil {
		return fmt.Errorf("节点 %d 系统网络: %w", n.ID, err)
	}
	if err := ensureISOs(ctx, d.DB, n.ID); err != nil {
		return err
	}
	return ensureTemplate(ctx, d.DB, n.ID)
}

// ensurePool 保证节点有一个「就绪」的默认本地池。真实创建流程会探测
// 设备并异步起池；演示数据直接落**创建完成后的最终形态**。
func ensurePool(ctx context.Context, db *gorm.DB, nodeID int64) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.StoragePool{}).
		Where("node_id = ? AND status = ?", nodeID, model.StoragePoolReady).
		Count(&count).Error; err != nil {
		return fmt.Errorf("统计存储池: %w", err)
	}
	if count > 0 {
		return nil
	}
	mount := fmt.Sprintf("/var/lib/kc/node-%d", nodeID)
	now := time.Now()
	pool := model.StoragePool{
		NodeID:         nodeID,
		DeviceID:       fmt.Sprintf("dev-root-%d", nodeID),
		Kind:           "local",
		MountPath:      &mount,
		TotalBytes:     500 << 30,
		UsableBytes:    420 << 30,
		IsDefault:      true,
		AutoMount:      true,
		Status:         model.StoragePoolReady,
		LastReportedAt: &now,
	}
	if err := db.WithContext(ctx).Create(&pool).Error; err != nil {
		// mock 的磁盘扫描可能并发建了池：唯一冲突视为「已存在」，不算失败。
		if isDuplicate(err) {
			return nil
		}
		return fmt.Errorf("创建演示存储池: %w", err)
	}
	log.Printf("[devdata] 节点 %d 已预置演示存储池", nodeID)
	return nil
}

// isDuplicate 报告错误是否为唯一约束冲突（演示数据预置里的并发兜底）。
func isDuplicate(err error) bool {
	// errors.Is 到驱动层需要引入具体驱动类型，演示预置用字符串判定足够。
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

// ensureISOs 保证节点有演示镜像。已有任何就绪 ISO 就不再加——用户自己
// 上传过的节点不应该被塞进一堆演示文件。
func ensureISOs(ctx context.Context, db *gorm.DB, nodeID int64) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.StorageFile{}).
		Where("node_id = ? AND category = ? AND uploaded_at IS NOT NULL", nodeID, model.FileCategoryISO).
		Count(&count).Error; err != nil {
		return fmt.Errorf("统计镜像: %w", err)
	}
	if count > 0 {
		return nil
	}
	now := time.Now()
	for _, spec := range isoSpecs {
		f := model.StorageFile{
			NodeID:     nodeID,
			RelPath:    "iso/" + spec.filename,
			Category:   model.FileCategoryISO,
			Filename:   spec.filename,
			SizeBytes:  spec.sizeBytes,
			OsType:     strPtr(spec.osType),
			OsVariant:  strPtr(spec.osVariant),
			MinDiskGB:  spec.minDiskGB,
			UploadedAt: &now,
		}
		if err := db.WithContext(ctx).Create(&f).Error; err != nil {
			return fmt.Errorf("创建演示镜像 %s: %w", spec.filename, err)
		}
	}
	log.Printf("[devdata] 节点 %d 已预置 %d 个演示镜像", nodeID, len(isoSpecs))
	return nil
}

// ensureTemplate 保证节点有一个可直接克隆的就绪模板（模板克隆模式的入口）。
func ensureTemplate(ctx context.Context, db *gorm.DB, nodeID int64) error {
	var count int64
	if err := db.WithContext(ctx).Model(&model.Template{}).
		Where("node_id = ? AND status = ?", nodeID, model.TemplateReady).
		Count(&count).Error; err != nil {
		return fmt.Errorf("统计模板: %w", err)
	}
	if count > 0 {
		return nil
	}
	diskPath := fmt.Sprintf("/var/lib/kc/node-%d/templates/demo-base.qcow2", nodeID)
	osType := "linux"
	tpl := model.Template{
		NodeID:          nodeID,
		Name:            "demo-ubuntu-base",
		Status:          model.TemplateReady,
		DiskPath:        &diskPath,
		DiskSizeGB:      10,
		OSType:          &osType,
		MinDiskGB:       10,
		DefaultCPU:      2,
		DefaultMemoryMB: 2048,
		Published:       true,
		Visibility:      "public",
	}
	if err := db.WithContext(ctx).Create(&tpl).Error; err != nil {
		return fmt.Errorf("创建演示模板: %w", err)
	}
	log.Printf("[devdata] 节点 %d 已预置演示模板", nodeID)
	return nil
}

func strPtr(s string) *string { return &s }
