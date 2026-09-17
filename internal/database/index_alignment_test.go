package database

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"

	"k_cockpit/internal/model"
)

// TestUniqueIndexesMatchBetweenModelAndMigration 静态比对「唯一索引」在两个
// 地方的声明：手写 SQL 迁移与 GORM 模型标签。
//
// 为什么只有**唯一索引**需要比对，普通索引不用：普通索引只影响查询快慢，
// 缺了它性能会下降但行为不变；唯一索引是一条**约束**，缺了它行为会变——
// 数据库允许写入重复数据，而代码里依赖「不可能重复」的分支会静默走错。
//
// 这个测试的由来是一次真实的漏网：storage_pool 的「每节点至多一个默认池」
// 由迁移里的部分唯一索引 uniq_storage_pool_default 保证，而模型里没声明它。
// 于是测试库（AutoMigrate 按模型建）根本没有这条约束，两个池同时成为默认池
// 也照建不误——执行器里那段「默认池冲突就退让」的兜底逻辑因此从未被验证过，
// 而它实际上在 SQLite 上也不工作（判定用到了只有 PostgreSQL 才返回的索引名）。
//
// 与列名那次（VCPU → v_cpu）是同一个成因：**模型与迁移各说各话，而测试库
// 站在模型这一边**。列名靠 TestModelColumnsExistInMigrations 拦住，索引靠这里。
//
// 本测试不连数据库，只解析文本——它必须能在没装 PostgreSQL 的机器上运行。
func TestUniqueIndexesMatchBetweenModelAndMigration(t *testing.T) {
	fromMigration := migrationUniqueIndexes(t)

	models := []any{
		&model.AuditLog{}, &model.VMCredential{}, &model.VpcSwitch{},
		&model.Node{}, &model.Session{}, &model.SystemSetting{},
		&model.StoragePool{}, &model.Task{}, &model.TaskStage{}, &model.User{},
		&model.VM{}, &model.VMSnapshot{}, &model.VMSchedule{},
		&model.VMLock{}, &model.PortForward{},
		&model.VMInterface{}, &model.StaticIP{}, &model.Template{},
	}

	var cache sync.Map
	problems := 0

	for _, m := range models {
		parsed, err := schema.Parse(m, &cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatalf("解析模型 %T 失败: %v", m, err)
		}

		fromModel := make(map[string]bool)
		for _, idx := range parsed.ParseIndexes() {
			// Class 为 "UNIQUE" 才是唯一索引；普通索引不比对（见文件头说明）。
			if strings.EqualFold(idx.Class, "UNIQUE") {
				fromModel[strings.ToLower(idx.Name)] = true
			}
		}

		// 方向一：迁移有、模型没有。
		//
		// 这是**危险的那个方向**：测试库没有这条约束，于是「依赖约束才会
		// 拦住」的路径在测试里畅通无阻，缺陷只在生产暴露。
		for _, name := range sortedNames(fromMigration[parsed.Table]) {
			if !fromModel[name] {
				problems++
				t.Errorf("%s：迁移里的唯一索引 %s 没有在模型中声明。\n"+
					"后果：测试库由 AutoMigrate 按模型建表，不会有这条约束，"+
					"依赖它的分支在测试中永远走不到。\n"+
					"修复方式：在模型的对应字段上补 gorm:\"uniqueIndex:%s\"（复合索引用 priority 指定列序，"+
					"带条件的索引用 where:...）。**不要修改已应用的迁移**。",
					parsed.Table, name, name)
			}
		}

		// 方向二：模型有、迁移没有。
		//
		// 这个方向不会漏掉缺陷，但会让**测试比生产更严**：测试里被唯一约束
		// 拦下的操作，到生产上却能成功。这类差异同样值得修，否则测试给出的
		// 保证是假的。
		for _, name := range sortedNames(fromModel) {
			if !fromMigration[parsed.Table][name] {
				problems++
				t.Errorf("%s：模型声明了唯一索引 %s，但迁移里没有。\n"+
					"后果：测试库会拒绝的写入，在生产库上会成功——测试比生产更严，"+
					"它给出的通过并不能代表生产行为。\n"+
					"修复方式：新增迁移补上该索引（迁移只增不改）。",
					parsed.Table, name)
			}
		}
	}

	if problems == 0 {
		t.Logf("已核对 %d 个模型的唯一索引与迁移的一致性", len(models))
	}
}

var createIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?(\w+)["` + "`" + `]?\s+ON\s+["` + "`" + `]?(\w+)["` + "`" + `]?`)

// migrationUniqueIndexes 解析迁移目录，返回「表名 → 唯一索引名集合」。
func migrationUniqueIndexes(t *testing.T) map[string]map[string]bool {
	t.Helper()

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}

	out := make(map[string]map[string]bool)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}

		for _, m := range createIndexRe.FindAllStringSubmatch(string(raw), -1) {
			// m[1] 非空表示这是 UNIQUE 索引。
			if strings.TrimSpace(m[1]) == "" {
				continue
			}
			table := strings.ToLower(m[3])
			if out[table] == nil {
				out[table] = make(map[string]bool)
			}
			out[table][strings.ToLower(m[2])] = true
		}
	}

	// 解析不出任何索引说明正则或目录不对——那时这个测试会变成永远通过，
	// 比没有这个测试更糟（它会给人「这块已经查过了」的错觉）。
	if len(out) == 0 {
		t.Fatal("没有从迁移里解析出任何唯一索引——正则或目录可能不对")
	}
	return out
}

func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
