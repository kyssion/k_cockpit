package userstorage_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// deleteClient 是 agent 替身：记录下发的删除指令，并按需成功/失败/不可达。
type deleteClient struct {
	ops  []agent.Operation
	fail bool
	down bool
}

func (c *deleteClient) Execute(_ context.Context, op agent.Operation) (*agent.Result, error) {
	c.ops = append(c.ops, op)
	if c.down {
		return nil, errors.New("node unreachable")
	}
	if c.fail {
		return &agent.Result{Success: false, Message: "删除文件失败: permission denied"}, nil
	}
	return &agent.Result{Success: true}, nil
}

// TestDeleteFileRemovesNodeBytesFirst 覆盖「只删记录、不删节点文件」。
//
// 实机现象：`DELETE /my-storage/files/:id` 之后节点上的文件还在，攒出 19 个
// 没有任何记录指向它的孤儿（约 250MB）。孤儿既不会出现在列表里，也不会被
// 任何清理路径碰到，只能靠人 ssh 上去删。
//
// 要求：先下发删除、成功后再删记录——顺序反了，一次失败就会留下孤儿。
func TestDeleteFileRemovesNodeBytesFirst(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	client := &deleteClient{}
	svc = svc.WithAgent(client)

	row := seedFile(t, db, userA, "orphan.bin", strings.Repeat("a", 64))
	if err := svc.DeleteFile(context.Background(), userA, 1, row.ID, owner(userA), "alice", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if len(client.ops) != 1 {
		t.Fatalf("应下发一次删除指令，实际 %d 次", len(client.ops))
	}
	op := client.ops[0]
	if op.Kind != agent.OpStorageFileDelete {
		t.Errorf("指令类型 = %v, 期望 %v", op.Kind, agent.OpStorageFileDelete)
	}
	// 下发的是**相对路径**：节点自己拼存储根（当前根→历史根回退），
	// 控制面不掌握宿主机上的绝对路径。
	if op.Target != row.RelPath {
		t.Errorf("下发路径 = %q, 期望相对路径 %q", op.Target, row.RelPath)
	}
	if op.NodeID != row.NodeID {
		t.Errorf("下发节点 = %d, 期望 %d", op.NodeID, row.NodeID)
	}

	var n int64
	db.Model(&model.StorageFile{}).Where("id = ?", row.ID).Count(&n)
	if n != 0 {
		t.Error("节点删除成功后记录应当消失")
	}
}

// TestDeleteFileKeepsRecordWhenNodeFails 覆盖节点侧失败。
//
// 记录必须留着：删了记录就等于宣告"这份内容已经不在"，而字节还在盘上——
// 那正是孤儿的成因，且用户再也无法通过界面定位到它。
func TestDeleteFileKeepsRecordWhenNodeFails(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	svc = svc.WithAgent(&deleteClient{fail: true})

	row := seedFile(t, db, userA, "keep.bin", strings.Repeat("b", 64))
	err := svc.DeleteFile(context.Background(), userA, 1, row.ID, owner(userA), "alice", "")
	if err == nil {
		t.Fatal("节点删除失败时接口必须报错，不能静默删记录")
	}
	assertStatus(t, err, 422)

	var n int64
	db.Model(&model.StorageFile{}).Where("id = ?", row.ID).Count(&n)
	if n != 1 {
		t.Error("节点删除失败后记录必须保留（否则字节成孤儿）")
	}
}

// TestDeleteFileReportsNodeUnreachable 覆盖节点不可达。
//
// 这一类失败最容易变成孤儿：如果"节点不可达"被当成成功，记录消失而文件
// 永远留在黑名单里查不到。
func TestDeleteFileReportsNodeUnreachable(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	svc = svc.WithAgent(&deleteClient{down: true})

	row := seedFile(t, db, userA, "down.bin", strings.Repeat("c", 64))
	err := svc.DeleteFile(context.Background(), userA, 1, row.ID, owner(userA), "alice", "")
	if err == nil {
		t.Fatal("节点不可达时应报错")
	}
	assertStatus(t, err, 503)

	var n int64
	db.Model(&model.StorageFile{}).Where("id = ?", row.ID).Count(&n)
	if n != 1 {
		t.Error("节点不可达时记录必须保留")
	}
}
