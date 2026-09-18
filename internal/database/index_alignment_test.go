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
		&model.PublicIP{}, &model.PublicIPBinding{},
		&model.SecurityGroup{}, &model.SecurityGroupRule{}, &model.InterfaceSecurityGroup{},
		&model.ShareMount{}, &model.StorageVolume{},
		&model.SchedulerEvent{},
		&model.FirewallPolicy{}, &model.FirewallRule{}, &model.FirewallVMPolicy{},
		&model.UserAPIKey{}, &model.AuthActionToken{},
		&model.PortMirror{}, &model.NetworkBridge{},
	}

	var cache sync.Map
	problems := 0

	for _, m := range models {
		parsed, err := schema.Parse(m, &cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatalf("解析模型 %T 失败: %v", m, err)
		}

		fromModel := make(map[string]indexInfo)
		for _, idx := range parsed.ParseIndexes() {
			// Class 为 "UNIQUE" 才是唯一索引；普通索引不比对（见文件头说明）。
			if strings.EqualFold(idx.Class, "UNIQUE") {
				name := strings.ToLower(idx.Name)
				fromModel[name] = indexInfo{Name: name, Where: normalizeWhere(idx.Where)}
			}
		}

		// 方向一：迁移有、模型没有。
		//
		// 这是**危险的那个方向**：测试库没有这条约束，于是「依赖约束才会
		// 拦住」的路径在测试里畅通无阻，缺陷只在生产暴露。
		for _, name := range sortedIndexNames(fromMigration[parsed.Table]) {
			// 已知例外：这些索引无法在模型上声明，约束由服务层承担。
			if reason, exempt := modelIndexExemptions[parsed.Table+"."+name]; exempt {
				t.Logf("跳过 %s.%s：%s", parsed.Table, name, reason)
				continue
			}
			want := fromMigration[parsed.Table][name]
			got, ok := fromModel[name]
			if !ok {
				problems++
				t.Errorf("%s：迁移里的唯一索引 %s 没有在模型中声明。\n"+
					"后果：测试库由 AutoMigrate 按模型建表，不会有这条约束，"+
					"依赖它的分支在测试中永远走不到。\n"+
					"修复方式：在模型的对应字段上补 gorm:\"uniqueIndex:%s\"（复合索引用 priority 指定列序，"+
					"带条件的索引用 where:...）。**不要修改已应用的迁移**。",
					parsed.Table, name, name)
				continue
			}
			// 名字对上了还要比条件：条件不同，约束的行为就完全不同。
			if got.Where != want.Where {
				problems++
				t.Errorf("%s：唯一索引 %s 的**条件**与迁移不一致。\n"+
					"  迁移：%q\n  模型：%q\n"+
					"后果：测试库由 AutoMigrate 按模型建表，两种条件的约束行为不同，"+
					"于是测试与生产在「什么算重复」上给出不同答案。\n"+
					"修复方式：把迁移里的条件原样写进模型的 where: 选项。**不要修改已应用的迁移**。",
					parsed.Table, name, want.Where, got.Where)
			}
		}

		// 方向二：模型有、迁移没有。
		//
		// 这个方向不会漏掉缺陷，但会让**测试比生产更严**：测试里被唯一约束
		// 拦下的操作，到生产上却能成功。这类差异同样值得修，否则测试给出的
		// 保证是假的。
		for _, name := range sortedIndexNames(fromModel) {
			if _, ok := fromMigration[parsed.Table][name]; !ok {
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

// createIndexRe 额外捕获索引末尾可选的 WHERE 条件。
//
// 条件**必须一起比**：索引名相同而条件不同的两个索引，约束的行为完全不同。
// 安全组那次就是如此——迁移里是 `WHERE deleted_at IS NULL`（当前存在的组名
// 唯一），模型里没有条件（组名一辈子唯一）。只比名字的话两者看着一样，
// 而测试库拿到的是无条件那版：删掉的组会永久占住名字，用户重建同名组时
// 撞上一句他无法理解的唯一约束冲突。
// modelIndexExemptions 是无法在 GORM 模型上声明、因而由服务层承担约束的
// 唯一索引。
//
// **每一项都必须写明理由**，并且对应的服务层实现要真的做了这件事——否则
// 这个表就变成了一个「把报错藏起来」的地方，而它存在的全部意义恰恰是
// 不让分叉被藏起来。
var modelIndexExemptions = map[string]string{
	"firewall_rule.uniq_firewall_rule_dedup": "" +
		"表达式索引（coalesce 把 port_start/port_end/source_cidr 的 NULL 归一化），" +
		"而 GORM 的索引选项以逗号分隔，coalesce 的参数里就有逗号——写进 tag 会生成" +
		"残缺的 SQL（AutoMigrate 直接报错）。约束由 firewall.Service.duplicateOf 按" +
		"**同一套归一化口径**承担。两者不等价（并发写入仍可能挤进两条），因而是" +
		"一个已知的、可接受的缺口，而不是「已经处理好了」。",
}

var createIndexRe = regexp.MustCompile(
	`(?is)CREATE\s+(UNIQUE\s+)?INDEX\s+(?:IF\s+NOT\s+EXISTS\s+)?["` + "`" + `]?(\w+)["` + "`" + `]?\s+ON\s+["` + "`" + `]?(\w+)["` + "`" + `]?[^;]*?(?:\bWHERE\s+([^;]+))?;`)

// dropIndexRe 匹配 DROP INDEX，用于把已删除的索引从集合里去掉。
//
// 不处理它的话，一条「建了又删」的索引会永远停在集合里：模型那边已经按
// 最终状态声明（或不需要声明），而这里还在要求它存在——一个永远修不好的
// 失败。DROP 不知道表名（SQL 语法里本就不需要），因此按索引名全局移除。
var dropIndexRe = regexp.MustCompile(
	`(?is)DROP\s+INDEX\s+(?:IF\s+EXISTS\s+)?["` + "`" + `]?(\w+)["` + "`" + `]?`)

// migrationUniqueIndexes 解析迁移目录，返回「表名 → **最终存在**的唯一索引名集合」。
//
// 按文件名顺序处理（os.ReadDir 已排序），因此「先 CREATE、后 DROP」的写法
// 会得到正确结果；反过来写成「先 DROP、后 CREATE」同样成立——我们关心的是
// 全部迁移跑完之后的最终状态。
// indexInfo 是一个唯一索引：名与它的条件（无条件是空串）。
type indexInfo struct {
	Name  string
	Where string
}

func migrationUniqueIndexes(t *testing.T) map[string]map[string]indexInfo {
	t.Helper()

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("读取迁移目录失败: %v", err)
	}

	out := make(map[string]map[string]indexInfo)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("migrations", e.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		text := string(raw)

		// **按源文件里的先后顺序**处理，不能「先加完再删完」。
		//
		// 一个迁移文件里完全可能出现「先 DROP 旧索引、再 CREATE 新的」，
		// 而两步的处理顺序反了的话，刚加上的那条会被随后的删除逻辑抹掉——
		// 结果是「迁移里有、模型里没有」，指向一个并不存在的问题。
		type stmt struct {
			pos  int
			drop bool
			m    []string
		}
		var stmts []stmt
		for _, m := range createIndexRe.FindAllStringSubmatchIndex(text, -1) {
			sub := make([]string, 5)
			for i := 0; i < 5 && 2*i+1 < len(m); i++ {
				if m[2*i] >= 0 {
					sub[i] = text[m[2*i]:m[2*i+1]]
				}
			}
			stmts = append(stmts, stmt{pos: m[0], m: sub})
		}
		for _, m := range dropIndexRe.FindAllStringSubmatchIndex(text, -1) {
			sub := make([]string, 2)
			sub[1] = text[m[2]:m[3]]
			stmts = append(stmts, stmt{pos: m[0], drop: true, m: sub})
		}
		sort.Slice(stmts, func(i, j int) bool { return stmts[i].pos < stmts[j].pos })

		for _, st := range stmts {
			if st.drop {
				name := strings.ToLower(st.m[1])
				// DROP 不带表名（SQL 语法里本就不需要），因此按名字全局移除。
				for table := range out {
					delete(out[table], name)
				}
				continue
			}
			// m[1] 非空表示这是 UNIQUE 索引。
			if strings.TrimSpace(st.m[1]) == "" {
				continue
			}
			table := strings.ToLower(st.m[3])
			if out[table] == nil {
				out[table] = make(map[string]indexInfo)
			}
			name := strings.ToLower(st.m[2])
			out[table][name] = indexInfo{Name: name, Where: normalizeWhere(st.m[4])}
		}
	}

	// 解析不出任何索引说明正则或目录不对——那时这个测试会变成永远通过，
	// 比没有这个测试更糟（它会给人「这块已经查过了」的错觉）。
	if len(out) == 0 {
		t.Fatal("没有从迁移里解析出任何唯一索引——正则或目录可能不对")
	}
	return out
}

func sortedIndexNames(set map[string]indexInfo) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// normalizeWhere 把条件规范化后比较。
//
// 两端写法的差异（大小写、多余空白、双引号）都是格式问题而不是语义问题，
// 逐字符比对会得到一堆噪声失败，而噪声会让人把这个检查关掉。
func normalizeWhere(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "\"", "")
	s = strings.ReplaceAll(s, "`", "")
	return strings.Join(strings.Fields(s), " ")
}
