package model

import (
	"time"

	"gorm.io/gorm"
)

// 节点注册状态。
const (
	// NodeEnrollPending 已生成注册令牌，等待 agent 注册。
	NodeEnrollPending = "pending"
	// NodeEnrollEnrolled agent 已完成注册。
	NodeEnrollEnrolled = "enrolled"
)

// 节点运行态。
//
// 这些值由**心跳时间推导**得出，不由 agent 直接上报——见 node.Service.deriveStatus。
const (
	NodeStatusOnline  = "online"
	NodeStatusOffline = "offline"
	NodeStatusUnknown = "unknown"
)

// Node 对应 node 表。
//
// 「元数据」与「运行态」是两类数据，本结构两处都存：
//   - 元数据（名称、注册信息、备注）由控制面管理，是权威来源；
//   - 运行态（心跳、能力、版本）是 agent 上报结果的**缓存**，
//     控制面只是代为保存，用于列表展示与离线判定。
type Node struct {
	ID int64 `gorm:"primaryKey"`
	// Name 带唯一索引：节点名是用户识别机器的唯一凭据，重名会让「哪台是哪台」
	// 无法回答。索引名与迁移中的 uniq_node_name 对应。
	Name              string `gorm:"size:64;not null;uniqueIndex:uniq_node_name"`
	Enabled           bool   `gorm:"not null;default:true"`
	Status            string `gorm:"size:16;not null;default:unknown"`
	MaintenanceMode   bool   `gorm:"not null;default:false"`
	IsMigrationTarget bool   `gorm:"not null;default:true"`

	// MaintenanceReason / MaintenanceAt 描述**当前这次**维护。
	//
	// 退出维护时与 MaintenanceMode 一同清空（见 node.Service.SetMaintenance）：
	// 保留一个「未在维护、但原因是『升级内核』」的记录，界面要么显示一个
	// 不生效的理由，要么得写额外判断去忽略它。
	//
	// 与业务软锁的 vm_lock.reason / locked_at 是同一套口径。
	MaintenanceReason *string `gorm:"size:255"`
	MaintenanceAt     *time.Time

	// agent 注册与信任信息。
	// 索引与迁移一致（uniq_node_agent_id）：agent 身份唯一，避免同一个
	// agent 被登记成两个节点。**可为空**，而空值不参与唯一性判定——
	// 尚未接入的节点 AgentID 为空，不该因为「已有另一个空值」而冲突。
	AgentID         *string `gorm:"size:64;uniqueIndex:uniq_node_agent_id"`
	EnrollTokenHash *string `gorm:"size:128"`
	EnrollExpiresAt *time.Time
	CertFingerprint *string `gorm:"size:128"`
	EnrollState     string  `gorm:"size:16;not null;default:pending"`

	// 版本、心跳与能力上报。
	AgentVersion    *string `gorm:"size:32"`
	ProtocolVersion int     `gorm:"not null;default:0"`
	LastHeartbeatAt *time.Time
	LastSeenAt      *time.Time
	Capabilities    *string `gorm:"type:text"`
	CapabilitiesAt  *time.Time
	LastError       *string `gorm:"size:255"`

	Remark    *string `gorm:"size:255"`
	CreatedAt time.Time
	UpdatedAt time.Time
	// 用 gorm.DeletedAt 而非 *time.Time：前者让 GORM **自动**为所有查询
	// 追加 `deleted_at IS NULL`。用普通指针时，任何一处忘记写条件都会
	// 让已删除的记录重新出现——这类遗漏在代码审查中很难被发现。
	DeletedAt gorm.DeletedAt
}

// TableName 固定表名。
func (Node) TableName() string { return "node" }

// IsEnrolled 报告该节点是否已完成 agent 注册。
func (n *Node) IsEnrolled() bool { return n.EnrollState == NodeEnrollEnrolled }
