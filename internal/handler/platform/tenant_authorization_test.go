package platform_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
)

// 本文件是 U-28「160 条 adminOnly 动态逐条越权」的落地。
//
// 与 guard_matrix_test.go 的分工：那个用例证明**声明**里写了 g.adminOnly，
// 这个用例证明**请求**真的被挡住。中间差着一段实际执行——中间件顺序接反、
// 把 handler 写在守卫之前、或者某条路由在 Group 上继承了错误的中间件，
// 静态对账一律看不出来，只有发一次真实请求才知道。
//
// 用租户身份打全部 adminOnly 路由，期望**每一条**都 403 且错误码为
// PERMISSION_DENIED。返回 200 意味着越权；返回 500 意味着守卫之后还有
// nil 依赖（这条路由要么漏了守卫，要么把 admin 判定写在了 handler 里）；
// 返回 404 意味着判定散落在 handler 内部而不是路由层。
func TestTenantForbiddenOnEveryAdminRoute(t *testing.T) {
	h, db := newAuthServer(t)
	seedUser(t, db, "tenant-r13", model.RoleTenant)
	cookie := sessionCookie(t, h, "tenant-r13")

	routes := adminOnlyRoutes(t)
	var broken []string
	for _, r := range routes {
		w := ut.PerformRequest(h.Engine, r.method, "/api/v1"+concreteRoutePath(r.path), nil,
			ut.Header{Key: "Cookie", Value: cookie})
		if w.Code == 403 {
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Error.Code != api.CodePermissionDenied {
				broken = append(broken, r.method+" "+r.path+"（403 但错误码是 "+body.Error.Code+"）")
			}
			continue
		}
		broken = append(broken, r.method+" "+r.path+"（返回 "+strconv.Itoa(w.Code)+"，期望 403）")
	}
	sort.Strings(broken)
	if len(broken) > 0 {
		t.Errorf("以下 %d 条 adminOnly 路由未挡住租户（共 %d 条）：\n  %s",
			len(broken), len(routes), strings.Join(broken, "\n  "))
	}
	t.Logf("租户越权扫描：%d 条 adminOnly 路由全部 403", len(routes))
}

// TestAnonymousGetsUnauthenticatedNotForbidden 断言未认证时**不返回 403**。
//
// 403 会告诉未登录的调用方「这条路由存在，只是你没权限」，等于把管理面
// 的端点清单白送出去。401 对全部端点一致，不泄漏任何存在性信息。
func TestAnonymousGetsUnauthenticatedNotForbidden(t *testing.T) {
	h, _ := newAuthServer(t)

	var leaked []string
	for _, r := range adminOnlyRoutes(t) {
		w := ut.PerformRequest(h.Engine, r.method, "/api/v1"+concreteRoutePath(r.path), nil)
		if w.Code == 401 {
			continue
		}
		leaked = append(leaked, r.method+" "+r.path+"（返回 "+strconv.Itoa(w.Code)+"）")
	}
	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Errorf("以下 adminOnly 路由对未认证请求没有返回 401：\n  %s", strings.Join(leaked, "\n  "))
	}
}

type routeRef struct {
	method string
	path   string
}

// adminOnlyRoutes 从 routes_*.go 解析挂了 g.adminOnly 的路由。
//
// 解析源码而不是读接口文档：文档是给人看的副本，源码是唯一权威。
func adminOnlyRoutes(t *testing.T) []routeRef {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根目录失败: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(root, "internal/router/routes_*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到路由文件: %v", err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	var out []routeRef
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			m := routeRe.FindStringSubmatch(line)
			if m == nil || !strings.Contains(line, "g.adminOnly") {
				continue
			}
			out = append(out, routeRef{method: m[1], path: m[2]})
		}
	}
	if len(out) != 160 {
		t.Fatalf("解析出 %d 条 adminOnly 路由，期望 160——新增/删除管理端点时请同步本用例", len(out))
	}
	return out
}

var routeParamRe = regexp.MustCompile(`:[A-Za-z_][A-Za-z0-9_]*`)

func concreteRoutePath(p string) string {
	return routeParamRe.ReplaceAllString(p, "1")
}
