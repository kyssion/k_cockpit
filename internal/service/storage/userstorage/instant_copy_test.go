package userstorage_test

import (
	"context"
	"errors"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/service/storage/userstorage"
)

// 本文件覆盖第 3 轮 DEF-R3-05 的控制面侧：秒传必须让**节点把文件真正落到
// 目标路径**，落地失败则整个请求失败，不得留下"有记录、无文件"。
//
// 真机证据：`instant=true`、返回新 rel_path，但该路径 isfile=false——之后
// 按 rel_path 取文件全部失败（并连锁导致 IMG-02 / IMG-X3 / VMX-01 假失败）。

// recordingAgent 记录下发给节点的操作，并按需失败。
type recordingAgent struct {
	ops    []agent.Operation
	result *agent.Result
	err    error
}

func (r *recordingAgent) Execute(_ context.Context, op agent.Operation) (*agent.Result, error) {
	r.ops = append(r.ops, op)
	if r.err != nil {
		return nil, r.err
	}
	if r.result != nil {
		return r.result, nil
	}
	return &agent.Result{Success: true}, nil
}

const instantDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

// TestInstantCopyDispatchesLinkToNode 覆盖主路径：秒传请求里必须带
// `link_from_rel`（来源相对路径）与新的 `rel_path`，节点确认后才写记录。
func TestInstantCopyDispatchesLinkToNode(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	stub := &recordingAgent{result: &agent.Result{Success: true, Data: map[string]any{
		agent.StorageFileDataKey: agent.StorageFileInfo{
			RelPath: "share/copy.iso", SizeBytes: 1024, Checksum: instantDigest,
		},
	}}}
	svc = svc.WithAgent(stub)
	ctx := context.Background()

	seedFile(t, db, userA, "mine.iso", instantDigest)
	view, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso",
		Filename: "copy.iso", TotalSize: 1024, ChunkSize: 1024, SHA256: instantDigest,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("秒传应成功: %v", err)
	}
	if !view.Instant {
		t.Fatal("应命中秒传")
	}
	if len(stub.ops) != 1 {
		t.Fatalf("应向节点下发 1 次落地操作，实际 %d", len(stub.ops))
	}
	op := stub.ops[0]
	if op.Kind != agent.OpStorageFileCommit {
		t.Errorf("落地应复用 storage.file.commit，实际 %s", op.Kind)
	}
	if got, _ := op.Params["link_from_rel"].(string); got != "share/mine.iso" {
		t.Errorf("link_from_rel = %q，期望来源相对路径 share/mine.iso", got)
	}
	if got, _ := op.Params["rel_path"].(string); got == "" || got == "share/mine.iso" {
		t.Errorf("rel_path 应是**新的**目标路径，实际 %q", got)
	}
	if got, _ := op.Params["checksum"].(string); got != instantDigest {
		t.Errorf("checksum 应带来源摘要（节点据此校验内容），实际 %q", got)
	}

	var n int64
	if err := db.Model(&model.StorageFile{}).Where("rel_path = ?", op.Params["rel_path"]).Count(&n).Error; err != nil {
		t.Fatalf("查询记录失败: %v", err)
	}
	if n != 1 {
		t.Errorf("节点落地成功后应写入 1 条记录，实际 %d", n)
	}
}

// TestInstantCopyFailsWithoutNodeFile 覆盖落地失败：**不得留下记录**。
func TestInstantCopyFailsWithoutNodeFile(t *testing.T) {
	cases := []struct {
		name string
		stub *recordingAgent
	}{
		{"节点明确失败", &recordingAgent{result: &agent.Result{
			Success: false, Message: "来源文件不存在: no such file",
		}}},
		{"节点不可达", &recordingAgent{err: errors.New("dial tcp: connection refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, db := newTestEnv(t, &fakeQuota{allow: true})
			svc = svc.WithAgent(tc.stub)
			ctx := context.Background()
			seedFile(t, db, userA, "mine.iso", instantDigest)

			_, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
				NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso2",
				Filename: "copy.iso", TotalSize: 1024, ChunkSize: 1024, SHA256: instantDigest,
			}, owner(userA), "alice", "")
			if err == nil {
				t.Fatal("节点未能落地时请求必须失败")
			}
			var n int64
			if err := db.Model(&model.StorageFile{}).
				Where("filename = ?", "copy.iso").Count(&n).Error; err != nil {
				t.Fatalf("查询记录失败: %v", err)
			}
			if n != 0 {
				t.Errorf("落地失败却写了记录（正是 DEF-R3-05 的形状）: %d 条", n)
			}
		})
	}
}

// TestInstantCopyRejectsChecksumMismatch 覆盖节点回报的内容与来源不一致。
//
// 不一致说明链接/复制拿到了坏数据；此时**不得登记记录**（等于把损坏文件
// 当成功交给用户），但也不能把请求判失败——退化为普通上传。
//
// 实机抓出：把请求判失败后，只要库里有一条「摘要与内容不符」的记录，
// 该摘要的秒传就永久失败，用户换文件名也传不上去（创建会话这一步就 422）。
func TestInstantCopyRejectsChecksumMismatch(t *testing.T) {
	svc, db := newTestEnv(t, &fakeQuota{allow: true})
	svc = svc.WithAgent(&recordingAgent{result: &agent.Result{Success: true, Data: map[string]any{
		agent.StorageFileDataKey: agent.StorageFileInfo{
			RelPath: "share/copy.iso", SizeBytes: 1024,
			Checksum: "0000000000000000000000000000000000000000000000000000000000000000",
		},
	}}})
	ctx := context.Background()
	seedFile(t, db, userA, "mine.iso", instantDigest)

	view, err := svc.CreateUpload(ctx, userA, userstorage.CreateUploadRequest{
		NodeID: 1, Category: model.FileCategoryISO, RelDir: "iso3",
		Filename: "copy.iso", TotalSize: 1024, ChunkSize: 1024, SHA256: instantDigest,
	}, owner(userA), "alice", "")
	if err != nil {
		t.Fatalf("内容摘要不一致时应退化为普通上传，而不是失败: %v", err)
	}
	if view.Instant {
		t.Error("摘要不一致时不得标记为秒传成功")
	}
	if view.UploadID == "" {
		t.Error("退化路径必须给出可续传的上传会话")
	}
	var n int64
	_ = db.Model(&model.StorageFile{}).Where("filename = ?", "copy.iso").Count(&n).Error
	if n != 0 {
		t.Errorf("摘要不一致却写了记录: %d 条", n)
	}
}
