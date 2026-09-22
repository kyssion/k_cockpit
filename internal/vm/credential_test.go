package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// withCredentialKey 在本组用例内启用凭据加密：读取与写入必须同一把密钥，
// 且加密方（执行器）与解密方（Service）不能各拿各的。
func withCredentialKey(t *testing.T) {
	t.Helper()
	testCredentialKey = cryptoutil.DeriveKey([]byte("test-key-for-vm-cred"), "v1")
	t.Cleanup(func() { testCredentialKey = nil })
}

// TestInitialCredentialRoundTrip 覆盖凭据的写入与读取（G-30）。
//
// 创建时给定的初始密码要能在详情页读回明文：凭据展示的意义就是让人能
// 登进机器。读回必须经过密文（库里不该有明文），且审计不能记密码本身。
func TestInitialCredentialRoundTrip(t *testing.T) {
	withCredentialKey(t)
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "cred-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		OSType: "linux", InitialPassword: "s3cret-pass",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	vmRow := waitForVMNamed(t, db, "cred-vm")

	viewer := authz.Viewer{UserID: 7}
	cred, err := svc.InitialCredentialOf(ctx, vmRow.ID, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("读取凭据失败: %v", err)
	}
	if !cred.Has {
		t.Fatal("创建时给了初始密码，凭据却不存在")
	}
	if cred.Username != "root" {
		t.Errorf("用户名 = %q, 期望 root（按 Linux 推断）", cred.Username)
	}
	if cred.Password != "s3cret-pass" {
		t.Errorf("密码 = %q, 期望与创建时一致", cred.Password)
	}

	// 审计必须留痕，且**不含密码**：审计表是排查时翻的，
	// 不能同时成为泄漏源。
	var actions []string
	var params []string
	if err := db.Table("audit_log").Where("action = ?", "vm.credential.read").
		Pluck("action", &actions).Error; err != nil || len(actions) == 0 {
		t.Fatalf("读取凭据没有写审计: %v", err)
	}
	if err := db.Table("audit_log").Where("action = ?", "vm.credential.read").
		Pluck("params", &params).Error; err == nil {
		for _, p := range params {
			if strings.Contains(p, "s3cret-pass") {
				t.Error("审计参数里出现了明文密码")
			}
		}
	}
}

// TestInitialCredentialAbsent 覆盖「创建时没给密码」的分支：
// 它不是错误，前端据此显示引导而不是报错。
func TestInitialCredentialAbsent(t *testing.T) {
	withCredentialKey(t)
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "nocred-vm", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	vmRow := waitForVMNamed(t, db, "nocred-vm")

	cred, err := svc.InitialCredentialOf(
		ctx, vmRow.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("读取凭据失败: %v", err)
	}
	if cred.Has {
		t.Error("没有初始凭据时 Has 应为 false")
	}
}

// TestInitialCredentialCoexistsWithConsolePassword 是索引修复（迁移 0047）的
// 回归用例：控制台密码（_vnc）与初始登录凭据是同一张表里的两行用途。
// 旧索引只按 vm_id 唯一，两者并存时后写的一方会静默失败——表现为
// 「设了控制台密码，初始凭据不见了」或反过来。
func TestInitialCredentialCoexistsWithConsolePassword(t *testing.T) {
	withCredentialKey(t)
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	if _, err := svc.Create(ctx, vm.CreateRequest{
		Name: "both-cred", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		InitialPassword: "login-pass-1",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	vmRow := waitForVMNamed(t, db, "both-cred")

	// 控制台密码与初始凭据先后写入，两行都必须存活。
	vncPass := "vncpas9"
	if _, err := svc.UpdateConsole(ctx, vmRow.ID, vm.ConsoleUpdate{
		Password: &vncPass,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("设置控制台密码失败: %v", err)
	}

	var count int64
	if err := db.Table("vm_credential").Where("vm_id = ?", vmRow.ID).
		Count(&count).Error; err != nil {
		t.Fatalf("统计凭据行失败: %v", err)
	}
	if count != 2 {
		t.Fatalf("凭据行数 = %d, 期望 2（控制台 + 初始登录）", count)
	}

	// 两个用途各自读回的都是自己的密码。
	cred, err := svc.InitialCredentialOf(
		ctx, vmRow.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("读取凭据失败: %v", err)
	}
	if cred.Password != "login-pass-1" {
		t.Errorf("初始凭据 = %q, 期望 login-pass-1（控制台密码不应混入）", cred.Password)
	}
}
