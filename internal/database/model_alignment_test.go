package database

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"k_cockpit/internal/model"
)

// TestModelColumnsExistInMigrations 静态比对「模型字段名」与「迁移 SQL 里的列名」。
//
// 为什么需要这个测试：模型字段到列名的转换由 GORM 的命名策略完成，而表由
// **手写 SQL 迁移**创建，两条路径各写一遍。不一致时：
//
//   - 不会编译报错；
//   - 其它测试也发现不了——测试库由 AutoMigrate 按**同一套策略**建表，
//     两边一起错，永远不会见面；
//   - 只在连上由迁移建的真实库时暴露，且表现为一个笼统的「服务内部错误」。
//
// 本项目已经踩过两次：`VCPU` → `v_cpu`（创建虚拟机必然失败）、`CIDR` → `c_id_r`
// （VPC 交换机读写必失败）。两次都是「测试全绿、生产必挂」，排查要翻到数据库
// 返回的原始错误才看得见。所以这里把它变成一条 CI 能拦住的规则，而不是靠人记得。
//
// 本测试**不连数据库**：它只解析迁移文件里的 DDL 文本。这样它在任何环境下都
// 会运行——包括没有 PostgreSQL 的开发机，而那正是最容易漏掉这类问题的地方。
func TestModelColumnsExistInMigrations(t *testing.T) {
	columns := migrationColumns(t)

	models := []any{
		&model.AuditLog{}, &model.VMCredential{}, &model.VpcSwitch{},
		&model.Node{}, &model.Session{}, &model.SystemSetting{},
		&model.StoragePool{}, &model.Task{}, &model.User{}, &model.VM{},
		&model.VMSnapshot{}, &model.VMSchedule{}, &model.VMLock{}, &model.PortForward{},
		&model.TaskStage{}, &model.VMInterface{}, &model.StaticIP{},
		&model.Template{}, &model.VMExport{}, &model.UserStorage{},
		&model.ImageImport{},
	}

	var cache sync.Map
	problems := 0

	for _, m := range models {
		parsed, err := schema.Parse(m, &cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatalf("解析模型 %T 失败: %v", m, err)
		}

		table, ok := columns[parsed.Table]
		if !ok {
			problems++
			t.Errorf("%s：迁移里没有创建这张表", parsed.Table)
			continue
		}

		var missing []string
		for _, f := range parsed.Fields {
			// DBName 为 "-" 表示该字段刻意不映射（gorm:"-"）。
			if f.DBName == "" || f.DBName == "-" {
				continue
			}
			if _, ok := table[f.DBName]; !ok {
				missing = append(missing, f.DBName+"（字段 "+f.Name+"）")
			}
		}

		if len(missing) > 0 {
			problems++
			t.Errorf("%s：迁移里缺少这些列：%s\n"+
				"修复方式：在模型字段上显式声明 gorm:\"column:xxx\"（若列名由命名策略转错），"+
				"或新增迁移补列（若确实漏建）。**不要修改已应用的迁移**——迁移只增不改。",
				parsed.Table, strings.Join(missing, "、"))
		}
	}

	if problems == 0 {
		t.Logf("已核对 %d 个模型与迁移的一致性", len(models))
	}
}

var (
	createTableRe = regexp.MustCompile(
		`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?(\w+)["` + "`" + `]?\s*\((.*?)\n\)\s*;`)
	addColumnRe = regexp.MustCompile(
		`(?is)ALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?["` + "`" + `]?(\w+)["` + "`" + `]?\s+ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?(\w+)`)
)

// migrationColumns 解析迁移目录，返回「表名 → 列名集合」。
//
// 只做声明式扫描，不建库：本测试要能在任何机器上跑，包括没装 PostgreSQL 的。
func migrationColumns(t *testing.T) map[string]map[string]bool {
	t.Helper()

	dir := "migrations"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}

	out := make(map[string]map[string]bool)

	ensure := func(table string) map[string]bool {
		table = strings.ToLower(table)
		if out[table] == nil {
			out[table] = make(map[string]bool)
		}
		return out[table]
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		script := string(raw)

		for _, m := range createTableRe.FindAllStringSubmatch(script, -1) {
			cols := ensure(m[1])
			for _, c := range parseColumnList(m[2]) {
				cols[c] = true
			}
		}
		for _, m := range addColumnRe.FindAllStringSubmatch(script, -1) {
			ensure(m[1])[strings.ToLower(m[2])] = true
		}
	}

	if len(out) == 0 {
		t.Fatal("没有从迁移里解析出任何表——正则或目录可能不对，这个测试会变成永远通过")
	}
	return out
}

// parseColumnList 从 CREATE TABLE 的括号内容里取列名。
//
// 跳过表级约束（PRIMARY KEY / FOREIGN KEY / UNIQUE / CHECK / CONSTRAINT）：
// 它们的首词是关键字而不是列名。行注释也跳过。
func parseColumnList(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}

		upper := strings.ToUpper(line)
		skip := false
		for _, kw := range []string{"PRIMARY KEY", "FOREIGN KEY", "UNIQUE", "CHECK", "CONSTRAINT", "EXCLUDE"} {
			if strings.HasPrefix(upper, kw) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out = append(out, strings.ToLower(strings.Trim(fields[0], `"`+"`")))
	}
	return out
}
