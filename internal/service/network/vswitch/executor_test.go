package vswitch_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/service/network/vswitch"
)

// 本文件覆盖第 3 轮 DEF-R3-12：软删记录占着唯一索引，同名重建在写记录这
// 一步失败，而节点上刚建的桥留了下来（真机 ORPHAN_AFTER_FAILED_CREATE）。

// resizeTask 造一条交换机变更任务（与队列入队的形状一致）。
func switchTask(t *testing.T, action, bridge string, spec map[string]any) *model.Task {
	t.Helper()
	nodeID := int64(1)
	raw, err := json.Marshal(map[string]any{
		"action":      action,
		"node_id":     nodeID,
		"bridge_name": bridge,
		"spec":        spec,
	})
	if err != nil {
		t.Fatalf("编码任务参数失败: %v", err)
	}
	s := string(raw)
	return &model.Task{ID: 1, Type: model.TaskVPCSwitchChange, NodeID: &nodeID, Params: &s}
}

// recordingClient 记录节点收到的操作，并按 created 回报桥是否本任务创建。
type recordingClient struct {
	*stubClient
	ops     []agent.Operation
	created bool
}

func (c *recordingClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	c.ops = append(c.ops, op)
	if op.Kind == agent.OpVPCSwitchChange && op.Params["action"] == "create" {
		return &agent.Result{Success: true, Data: map[string]any{
			agent.SwitchBridgeCreatedKey: c.created,
		}}, nil
	}
	return &agent.Result{Success: true}, nil
}

// TestCreateConflictRollsBackOwnBridge 覆盖「DB 冲突 → 节点不留残留」。
func TestCreateConflictRollsBackOwnBridge(t *testing.T) {
	client := &recordingClient{stubClient: newStubClient(), created: true}
	_, db := newTestEnv(t, client)

	// 先占住名字，让执行器写记录时必然撞唯一约束。
	live := model.VpcSwitch{NodeID: 1, Name: "dup", BridgeName: "kbrlive", Status: model.NetworkActive}
	if err := db.Create(&live).Error; err != nil {
		t.Fatalf("预置交换机失败: %v", err)
	}

	ex := vswitch.NewSwitchChangeExecutor(db, client)
	err := ex.Run(context.Background(), switchTask(t, "create", "kbrnew",
		map[string]any{"name": "dup", "mode": "nat", "cidr": "192.168.99.0/24"}))
	if err == nil {
		t.Fatal("重名创建应失败")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 409 {
		t.Fatalf("应为 409 冲突，实际: %v", err)
	}
	if !strings.Contains(apiErr.Message, "已回滚") {
		t.Errorf("文案应说明网桥已回滚，实际: %s", apiErr.Message)
	}

	var rollback *agent.Operation
	for i := range client.ops {
		if client.ops[i].Params["action"] == "delete" {
			rollback = &client.ops[i]
		}
	}
	if rollback == nil {
		t.Fatalf("冲突后必须下发节点侧逆操作，实际操作序列: %+v", client.ops)
	}
	if rollback.Kind != agent.OpVPCSwitchChange {
		t.Errorf("逆操作应复用 vpc.switch.change，实际 %s", rollback.Kind)
	}
	if rollback.Target != "kbrnew" {
		t.Errorf("逆操作应针对本次建的桥 kbrnew，实际 %q", rollback.Target)
	}
	if rollback.Params["spec"] == nil {
		t.Error("逆操作要带 spec，节点删 NAT 规则需要 cidr")
	}
}

// TestCreateConflictKeepsForeignBridge 覆盖「桥不是本任务建的」。
//
// `--may-exist add-br` 对已存在的桥也返回成功，此时删桥等于毁掉别人正在用的
// 网络——那比留一座空桥严重得多。所以只在节点回报 created=true 时回滚。
func TestCreateConflictKeepsForeignBridge(t *testing.T) {
	client := &recordingClient{stubClient: newStubClient(), created: false}
	_, db := newTestEnv(t, client)

	live := model.VpcSwitch{NodeID: 1, Name: "dup2", BridgeName: "kbrlive2", Status: model.NetworkActive}
	if err := db.Create(&live).Error; err != nil {
		t.Fatalf("预置交换机失败: %v", err)
	}

	ex := vswitch.NewSwitchChangeExecutor(db, client)
	err := ex.Run(context.Background(), switchTask(t, "create", "kbrforeign",
		map[string]any{"name": "dup2", "mode": "nat"}))
	if err == nil {
		t.Fatal("重名创建应失败")
	}
	for _, op := range client.ops {
		if op.Params["action"] == "delete" {
			t.Fatalf("桥非本任务创建时不得下发删除: %+v", op)
		}
	}
}

// TestSoftDeletedSwitchNameCanBeReused 覆盖 DEF-R3-12 的主症状。
//
// 软删（保留审计）之后，同名重建必须能成功：唯一性是"当前存在的唯一"，
// 不是"一辈子唯一"。
func TestSoftDeletedSwitchNameCanBeReused(t *testing.T) {
	svc, db := newTestEnv(t, newStubClient())

	deleted := time.Now().Add(-time.Hour)
	old := model.VpcSwitch{
		NodeID: 1, Name: "prod-net", BridgeName: "kbrold",
		Status: model.NetworkActive, DeletedAt: &deleted,
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatalf("预置软删交换机失败: %v", err)
	}

	if _, err := svc.CreateSwitch(context.Background(), 1, validSwitch(), 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("软删之后同名重建应被受理: %v", err)
	}
	waitForSwitch(t, func() bool {
		var n int64
		db.Model(&model.VpcSwitch{}).
			Where("name = ? AND deleted_at IS NULL", "prod-net").Count(&n)
		return n == 1
	})
}

// TestPartialUniqueIndexExcludesSoftDeleted 直接钉住数据库约束本身。
//
// 这条用例的意义在测试库：它由 AutoMigrate 按**模型 tag** 建表，模型漏写
// where:deleted_at IS NULL 时这里会失败，而生产库（迁移 0052）是对的——
// 那种分叉正是 model/security_group.go 记过的教训。
func TestPartialUniqueIndexExcludesSoftDeleted(t *testing.T) {
	_, db := newTestEnv(t, newStubClient())

	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-time.Hour)
	for _, at := range []*time.Time{&t1, &t2} {
		if err := db.Create(&model.VpcSwitch{
			NodeID: 1, Name: "reuse-me", BridgeName: "kbrx",
			Status: model.NetworkActive, DeletedAt: at,
		}).Error; err != nil {
			t.Fatalf("多条软删同名记录不应冲突: %v", err)
		}
	}

	// 当前存在的那一条仍然唯一。
	if err := db.Create(&model.VpcSwitch{
		NodeID: 1, Name: "reuse-me", BridgeName: "kbry", Status: model.NetworkActive,
	}).Error; err != nil {
		t.Fatalf("第一条在用记录应能写入: %v", err)
	}
	if err := db.Create(&model.VpcSwitch{
		NodeID: 1, Name: "reuse-me", BridgeName: "kbrz", Status: model.NetworkActive,
	}).Error; err == nil {
		t.Fatal("两条在用的同名记录必须冲突（唯一性只对在用记录成立，但仍必须成立）")
	}
}
