package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// consoleFileFixture 准备一台开着 SPICE、且节点填了控制台地址的虚拟机。
//
// SPICE 支持由**节点探测**填入（SPICESupported），因此这里要显式置位——
// 否则连接会被判成"这个协议不可用"。
func consoleFileFixture(t *testing.T) (*vm.Service, *model.VM) {
	t.Helper()

	svc, db, row := consoleFixture(t, model.DisplayVNC, true)

	// 控制台对外地址在**节点**上。
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Updates(map[string]any{"console_host": "kvm-node-1.example.com"}).Error; err != nil {
		t.Fatalf("设置节点控制台地址失败: %v", err)
	}
	port := 5901
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Updates(map[string]any{
			"spice_supported": true, "spice_enabled": true, "spice_port": port,
		}).Error; err != nil {
		t.Fatalf("开启 SPICE 失败: %v", err)
	}
	row.SPICESupported = true
	row.SPICEEnabled = true
	row.SPICEPort = &port
	return svc, row
}

// TestConnectionFileIsSpiceOnly 覆盖「只有 SPICE 有连接文件」。
//
// VNC 在浏览器里由 noVNC 承载，没有跨客户端通用的 .vv 等价物；给它也做
// 一个"下载文件"的入口，只会得到一个用户不知道拿什么打开的文件。
func TestConnectionFileIsSpiceOnly(t *testing.T) {
	svc, row := consoleFileFixture(t)
	ctx := context.Background()

	_, err := svc.ConsoleConnectionFile(ctx, row.ID, "vnc", false,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("VNC 也生成了连接文件")
	}
	if !strings.Contains(err.Error(), "SPICE") {
		t.Errorf("错误未说明只支持 SPICE: %v", err)
	}
}

// TestConnectionFileContainsHostAndPort 覆盖文件内容。
//
// 默认**不含密码**：控制台密码平时只写不读（R-005），默认路径不泄漏。
func TestConnectionFileContainsHostAndPort(t *testing.T) {
	svc, row := consoleFileFixture(t)
	ctx := context.Background()

	file, err := svc.ConsoleConnectionFile(ctx, row.ID, "spice", false,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("生成连接文件失败: %v", err)
	}
	if !strings.Contains(file.Content, "host=kvm-node-1.example.com") {
		t.Errorf("文件内容缺少主机: %s", file.Content)
	}
	if !strings.Contains(file.Content, "port=5901") {
		t.Errorf("文件内容缺少端口: %s", file.Content)
	}
	if strings.Contains(file.Content, "password=") {
		t.Error("默认不应写入密码")
	}
	if !strings.HasSuffix(file.Filename, "-spice.vv") {
		t.Errorf("文件名 = %q", file.Filename)
	}
	// delete-this-file 提示客户端用完即删：文件里可能带凭据。
	if !strings.Contains(file.Content, "delete-this-file=1") {
		t.Error("未提示客户端删除文件")
	}
}

// TestConnectionFileRejectsWithoutConsoleHost 覆盖"没有地址就不给文件"。
//
// 猜一个地址的代价是"下载了却连不上"，而用户无从判断是控制台没开还是
// 地址写错了——那比明确拒绝更难排查。
func TestConnectionFileRejectsWithoutConsoleHost(t *testing.T) {
	svc, db, row := consoleFixture(t, model.DisplayVNC, true)
	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Updates(map[string]any{"console_host": nil}).Error; err != nil {
		t.Fatalf("清空地址失败: %v", err)
	}
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Updates(map[string]any{"spice_supported": true, "spice_enabled": true}).Error; err != nil {
		t.Fatalf("开启 SPICE 失败: %v", err)
	}

	_, err := svc.ConsoleConnectionFile(context.Background(), row.ID, "spice", false,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("节点没填地址却生成了连接文件")
	}
	if !strings.Contains(err.Error(), "地址") {
		t.Errorf("错误应说明缺少地址: %v", err)
	}
}

// TestConnectionFilePasswordRequiresPasswordSet 覆盖"要密码但没设密码"。
func TestConnectionFilePasswordRequiresPasswordSet(t *testing.T) {
	svc, row := consoleFileFixture(t)

	_, err := svc.ConsoleConnectionFile(context.Background(), row.ID, "spice", true,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("未设置密码却生成了带密码的连接文件")
	}
	if !strings.Contains(err.Error(), "密码") {
		t.Errorf("错误应说明尚未设置密码: %v", err)
	}
}
