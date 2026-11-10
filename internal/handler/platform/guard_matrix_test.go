package platform

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// guardMatrix 是「路由声明 → 守卫分层」的一次性解析结果。
//
// 权限边界的唯一权威是 internal/router/routes_*.go；publicAPIRoutes 与
// adminAPIRoutes 只是接口文档的标注。已有的 TestAdminAPIRoutesMatchRouter
// 只守住了 admin 一档，本用例把 public / user 两档也纳入，并校验总数与
// 方法分布——否则新增一条「忘了挂 requireAuth」的路由不会被任何测试发现。
type guardMatrix struct {
	all    map[string]bool // "METHOD /path"（不含 /api/v1 前缀）
	public map[string]bool
	admin  map[string]bool
	user   map[string]bool
	method map[string]int
}

func parseGuardMatrix(t *testing.T) guardMatrix {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "router", "routes_*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到路由文件: %v", err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	m := guardMatrix{
		all:    map[string]bool{},
		public: map[string]bool{},
		admin:  map[string]bool{},
		user:   map[string]bool{},
		method: map[string]int{},
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			match := routeRe.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			key := match[1] + " " + match[2]
			if m.all[key] {
				t.Errorf("重复注册的端点：%s（%s）", key, filepath.Base(f))
			}
			m.all[key] = true
			m.method[match[1]]++
			switch {
			case strings.Contains(line, "g.adminOnly"):
				m.admin[key] = true
			case strings.Contains(line, "g.requireAuth"):
				m.user[key] = true
			default:
				m.public[key] = true
			}
		}
	}
	return m
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// 域路由总数与方法分布：盘点基线变更时必须先确认是有意增删，而不是漏注册。
func TestGuardMatrixRouteCountAndMethods(t *testing.T) {
	m := parseGuardMatrix(t)
	if got := len(m.all); got != 340 {
		t.Errorf("域路由总数 = %d，期望 340（新增/删除路由请同步 publicAPIRoutes、adminAPIRoutes 与本用例）", got)
	}
	want := map[string]int{"GET": 130, "POST": 133, "PUT": 19, "PATCH": 16, "DELETE": 42}
	for method, n := range want {
		if got := m.method[method]; got != n {
			t.Errorf("%s 路由数 = %d，期望 %d", method, got, n)
		}
	}
	for method := range m.method {
		if _, ok := want[method]; !ok {
			t.Errorf("出现未登记的 HTTP 方法：%s", method)
		}
	}
}

// 三档加起来必须等于全集，且互不相交。
func TestGuardMatrixPartitionsAllRoutes(t *testing.T) {
	m := parseGuardMatrix(t)
	if len(m.public)+len(m.admin)+len(m.user) != len(m.all) {
		t.Fatalf("三档合计 %d+%d+%d=%d，与全集 %d 不符",
			len(m.public), len(m.admin), len(m.user),
			len(m.public)+len(m.admin)+len(m.user), len(m.all))
	}
	for key := range m.all {
		n := 0
		if m.public[key] {
			n++
		}
		if m.admin[key] {
			n++
		}
		if m.user[key] {
			n++
		}
		if n != 1 {
			t.Errorf("端点 %s 落在 %d 档守卫里，期望恰好 1 档", key, n)
		}
	}
	if got := len(m.admin); got != 160 {
		t.Errorf("adminOnly 路由数 = %d，期望 160", got)
	}
	if got := len(m.public); got != 15 {
		t.Errorf("域内公开路由数 = %d，期望 15（加 /health 共 16）", got)
	}
	if got := len(m.user); got != 165 {
		t.Errorf("登录即可路由数 = %d，期望 165", got)
	}
}

// 公开面是安全边界：publicAPIRoutes 的 16 条必须与源码逐条对应。
func TestPublicAPIRoutesMatchRouter(t *testing.T) {
	m := parseGuardMatrix(t)
	want := map[string]bool{}
	for key := range publicAPIRoutes {
		if key == "GET /health" {
			continue // /health 注册在 router.go，不在域文件里
		}
		want[key] = true
	}
	for key := range want {
		if !m.public[key] {
			t.Errorf("接口文档标为公开，但源码挂了认证守卫：%s", key)
		}
	}
	for key := range m.public {
		if !want[key] {
			t.Errorf("源码未挂认证守卫，但接口文档未标为公开：%s", key)
		}
	}
	if got := len(publicAPIRoutes); got != 16 {
		t.Errorf("publicAPIRoutes 条数 = %d，期望 16", got)
	}
}

// adminOnly 必须同时挂 requireAuth：只挂 admin 而不认证会让
// authz.Admin() 在拿不到用户时行为不确定，属于危险的半配状态。
func TestAdminRoutesAlsoRequireAuth(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "router", "routes_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "g.adminOnly") {
				continue
			}
			match := routeRe.FindStringSubmatch(line)
			if match == nil {
				continue // 跨行注册由 TestAdminAPIRoutesMatchRouter 报告
			}
			if !strings.Contains(line, "g.requireAuth") {
				t.Errorf("%s 的 %s %s 只挂了 adminOnly，缺少 requireAuth",
					filepath.Base(f), match[1], match[2])
			}
		}
	}
}

// 打印三档清单，供 §6.8 的机械对账与测试报告引用（失败时也能定位）。
func TestGuardMatrixDump(t *testing.T) {
	if os.Getenv("KC_DUMP_GUARD_MATRIX") == "" {
		t.Skip("设置 KC_DUMP_GUARD_MATRIX=1 输出完整清单")
	}
	m := parseGuardMatrix(t)
	t.Logf("public(%d): %v", len(m.public), keys(m.public))
	t.Logf("admin(%d): %v", len(m.admin), keys(m.admin))
	t.Logf("user(%d): %v", len(m.user), keys(m.user))
}
