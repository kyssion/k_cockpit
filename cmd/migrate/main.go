// Command migrate 是数据库迁移执行器（docs/02-architecture/DATA_MODEL.md §6.1）。
//
// 用法：
//
//	go run ./cmd/migrate            执行未应用的迁移
//	go run ./cmd/migrate -status    只列出状态，不改动任何东西
//	go run ./cmd/migrate -dry-run   打印将要执行的语句但不执行
//
// 三条来自迁移策略的硬约束：
//
//   - **建表入口唯一**：结构变更一律走显式迁移，服务启动不做自动迁移；
//   - **只增不改**：已执行的迁移不得修改，修正靠新增迁移。因此这里会比对
//     已应用迁移的校验和，发现历史文件被改动时**报错而不是忽略**——忽略
//     会让「库里的结构」与「代码假设的结构」悄悄分叉；
//   - **单事务**：一个文件内的语句在一个事务里执行，任一句失败即整体回滚，
//     不会留下半成品表结构。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gorm.io/gorm"

	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
)

// AppliedMigration 对应 schema_migration 表。
type AppliedMigration struct {
	MigrationID string    `gorm:"primaryKey;size:128"`
	Checksum    string    `gorm:"size:64"`
	Note        *string   `gorm:"size:255"`
	AppliedAt   time.Time `gorm:"not null;default:now()"`
}

func (AppliedMigration) TableName() string { return "schema_migration" }

func main() {
	statusOnly := flag.Bool("status", false, "只列出迁移状态，不执行")
	dryRun := flag.Bool("dry-run", false, "打印将要执行的语句，不实际执行")
	dir := flag.String("dir", "internal/database/migrations", "迁移文件目录")
	flag.Parse()

	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 迁移文件是 PostgreSQL 专用语法，在 SQLite 上执行只会得到一堆语法错误。
	// 这里直接拦下并说明，而不是让用户去读半屏的报错。
	if cfg.DB.Driver != config.DriverPostgres {
		log.Fatalf("迁移仅支持 PostgreSQL（当前驱动 %s）；"+
			"本地 SQLite 走 GORM 模型建表，不走显式迁移", cfg.DB.Driver)
	}

	db, err := database.Open(cfg.DB, false)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}

	migrations, err := loadMigrations(*dir)
	if err != nil {
		log.Fatalf("读取迁移文件失败: %v", err)
	}
	if len(migrations) == 0 {
		log.Fatalf("目录 %s 下没有迁移文件", *dir)
	}

	applied, err := loadApplied(db)
	if err != nil {
		// 首次运行时 schema_migration 还不存在——它由第一个迁移创建。
		// 此时建一张同结构的表，让后续记录有地方落。
		if err := db.AutoMigrate(&AppliedMigration{}); err != nil {
			log.Fatalf("准备 schema_migration 表失败: %v", err)
		}
		applied = map[string]AppliedMigration{}
	}

	fmt.Printf("数据库: %s@%s/%s\n", cfg.DB.User, cfg.DB.Host, cfg.DB.Name)
	fmt.Printf("迁移文件: %d 个，已应用: %d 个\n\n", len(migrations), len(applied))

	// 先校验已应用的迁移有没有被改动过。放在执行之前：一旦发现分叉，
	// 继续执行只会让差异扩大。
	if problems := verifyChecksums(migrations, applied); len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("  ✗ " + p)
		}
		log.Fatalf("已应用的迁移被改动：请新增迁移而不是修改历史文件" +
			"（库里的结构与代码假设的结构已经分叉，继续执行会扩大差异）")
	}

	pending := 0
	for _, m := range migrations {
		state := "待应用"
		if _, ok := applied[m.ID]; ok {
			state = "已应用"
		} else {
			pending++
		}
		fmt.Printf("  %-44s %8s  %3d 条语句  %s\n",
			m.ID, state, len(m.Statements), humanSize(len(m.Raw)))
	}
	fmt.Println()

	if *statusOnly {
		fmt.Printf("待应用 %d 个（-status 模式未做任何改动）\n", pending)
		return
	}
	if pending == 0 {
		fmt.Println("数据库已是最新，无需执行。")
		return
	}

	if *dryRun {
		for _, m := range migrations {
			if _, ok := applied[m.ID]; ok {
				continue
			}
			fmt.Printf("\n===== %s（%d 条语句）=====\n", m.ID, len(m.Statements))
			for i, stmt := range m.Statements {
				fmt.Printf("--- [%d] ---\n%s\n", i+1, truncate(stmt, 400))
			}
		}
		fmt.Println("\n(-dry-run 模式未执行任何语句)")
		return
	}

	// 逐文件执行。每个文件一个事务：整个脚本要么全部生效，要么全部回滚。
	for _, m := range migrations {
		if _, ok := applied[m.ID]; ok {
			continue
		}

		fmt.Printf("→ 应用 %s ... ", m.ID)
		if err := applyOne(db, m); err != nil {
			fmt.Println("失败")
			log.Fatalf("执行 %s 失败（已回滚，库结构未改变）: %v", m.ID, err)
		}
		fmt.Println("完成")
	}

	fmt.Printf("\n全部完成，本次应用 %d 个迁移。\n", pending)
}

// migration 是一个待执行或已执行的迁移文件。
type migration struct {
	ID         string
	Raw        string
	Statements []string
	Checksum   string
}

// loadMigrations 读取目录下的 .sql 并按文件名排序。
//
// 文件名的数字前缀即执行顺序——排序用文件名而不是修改时间：修改时间在
// 克隆仓库、切换分支后并不可靠。
func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	out := make([]migration, 0, len(names))
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{
			ID:         strings.TrimSuffix(name, ".sql"),
			Raw:        string(raw),
			Statements: splitStatements(string(raw)),
			Checksum:   hex.EncodeToString(sum[:]),
		})
	}
	return out, nil
}

// splitStatements 把 SQL 脚本切成可逐条执行的语句。
//
// **不能简单地按分号切分**：注释里、字符串字面量里都可能出现分号
// （本项目的迁移注释里就有「语句带 ; 」这类表述），硬切会把它们切断，
// 得到的语句既无法执行，报错位置也会指向一个看不懂的地方。
//
// 因此按字符扫描，跟踪三种状态：单引号字符串、双引号标识符、行注释。
func splitStatements(script string) []string {
	var (
		statements []string
		current    strings.Builder
		inSingle   bool // '...' 字符串字面量
		inDouble   bool // "..." 标识符（如 "order" 列名）
		inLineCmt  bool // -- 到行尾
	)

	runes := []rune(script)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]

		switch {
		case inLineCmt:
			if ch == '\n' {
				inLineCmt = false
				current.WriteRune(ch)
			}
			continue

		case inSingle:
			current.WriteRune(ch)
			if ch == '\'' {
				// '' 是转义的单引号，不结束字符串。
				if i+1 < len(runes) && runes[i+1] == '\'' {
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				inSingle = false
			}
			continue

		case inDouble:
			current.WriteRune(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}

		switch ch {
		case '\'':
			inSingle = true
			current.WriteRune(ch)
		case '"':
			inDouble = true
			current.WriteRune(ch)
		case '-':
			if i+1 < len(runes) && runes[i+1] == '-' {
				inLineCmt = true
				i++
				continue
			}
			current.WriteRune(ch)
		case ';':
			if stmt := strings.TrimSpace(current.String()); stmt != "" {
				statements = append(statements, stmt)
			}
			current.Reset()
		default:
			current.WriteRune(ch)
		}
	}

	// 文件末尾可能没有分号。补上最后一条，而不是静默丢弃它——
	// 「最后一句没执行」是最难发现的一类问题。
	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		statements = append(statements, stmt)
	}
	return statements
}

// applyOne 在一个事务内执行单个迁移文件并登记。
func applyOne(db *gorm.DB, m migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		for i, stmt := range m.Statements {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("第 %d 条语句失败: %w\n语句: %s",
					i+1, err, truncate(stmt, 300))
			}
		}

		note := fmt.Sprintf("applied by cmd/migrate, %d statements", len(m.Statements))
		return tx.Exec(
			`INSERT INTO schema_migration (migration_id, checksum, note, applied_at)
			 VALUES (?, ?, ?, now())
			 ON CONFLICT (migration_id) DO UPDATE
			 SET checksum = EXCLUDED.checksum, note = EXCLUDED.note, applied_at = now()`,
			m.ID, m.Checksum, note,
		).Error
	})
}

// loadApplied 读取已应用的迁移。
func loadApplied(db *gorm.DB) (map[string]AppliedMigration, error) {
	var rows []AppliedMigration
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]AppliedMigration, len(rows))
	for _, r := range rows {
		out[r.MigrationID] = r
	}
	return out, nil
}

// verifyChecksums 检查已应用的迁移文件是否被改动过。
//
// 迁移「只增不改」：改动已执行的文件，会让库里的结构停留在旧版本，而代码
// 已经按新版本假设——这类分叉不会立刻报错，只会在某个字段缺失时才暴露。
func verifyChecksums(migrations []migration, applied map[string]AppliedMigration) []string {
	var problems []string
	for _, m := range migrations {
		rec, ok := applied[m.ID]
		if !ok {
			continue
		}
		// 没有校验和的旧记录（如手工执行 psql 后补登）不参与比对。
		if rec.Checksum == "" {
			continue
		}
		if rec.Checksum != m.Checksum {
			problems = append(problems,
				fmt.Sprintf("%s 的校验和与库中记录不一致（文件已被改动）", m.ID))
		}
	}
	return problems
}

func humanSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + " ...(截断)"
}
