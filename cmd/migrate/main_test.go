package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDuplicatePrefixes(t *testing.T) {
	names := []string{
		"0017_security_group_multi.sql",
		"0018_bridge_uplink_watchdog.sql",
		"0018_drop_unused_security_challenge.sql",
		"0019_bridge_detail.sql",
		"0019_resource_quota.sql",
		"0020_host_firewall.sql",
	}
	got := duplicatePrefixes(names)
	if len(got) != 2 {
		t.Fatalf("应发现 2 组重复编号，实际 %d: %v", len(got), got)
	}
	if got[0][0] != "0018_bridge_uplink_watchdog.sql" || got[0][1] != "0018_drop_unused_security_challenge.sql" {
		t.Errorf("第一组不对: %v", got[0])
	}
	if got[1][0] != "0019_bridge_detail.sql" || got[1][1] != "0019_resource_quota.sql" {
		t.Errorf("第二组不对: %v", got[1])
	}
}

func TestDuplicatePrefixesEmptyWhenUnique(t *testing.T) {
	if got := duplicatePrefixes([]string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}); len(got) != 0 {
		t.Errorf("编号唯一时不应报告重复: %v", got)
	}
}

// macOS 打出的发布包会带 AppleDouble 伴生文件（`._0001_x.sql`），内容不是
// SQL。它们必须被跳过，否则执行时报的是数据库协议错（invalid message
// format），把「包里有垃圾文件」伪装成「迁移写错了」。
func TestLoadMigrationsSkipsNonMigrationFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", name, err)
		}
	}
	write("0001_init_schema.sql", "CREATE TABLE t (id INT);")
	write("0002_add_column.sql", "ALTER TABLE t ADD COLUMN c INT;")
	write("._0001_init_schema.sql", "\x00\x05\x16\x07Mac OS X        ")
	write("README.sql", "这不是迁移")
	write("0003_.sql", "SELECT 1;") // 名称段为空，同样不符合规则

	got, err := loadMigrations(dir)
	if err != nil {
		t.Fatalf("loadMigrations 失败: %v", err)
	}
	if len(got) != 2 {
		names := make([]string, 0, len(got))
		for _, m := range got {
			names = append(names, m.ID)
		}
		t.Fatalf("应只载入 2 个合法迁移，实际 %d: %v", len(got), names)
	}
	if got[0].ID != "0001_init_schema" || got[1].ID != "0002_add_column" {
		t.Errorf("迁移顺序或 ID 不对: %s, %s", got[0].ID, got[1].ID)
	}
}
