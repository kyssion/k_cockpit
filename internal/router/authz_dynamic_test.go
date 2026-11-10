package router_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"

	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/router"
)

// 本文件是 U-11 / U-28 的**动态**一半。
//
// guard_matrix_test.go 守的是「声明」：routes_*.go 里写了 g.adminOnly。
// 但声明正确不等于行为正确——中间件挂反顺序、把 handler 写在守卫之前、
// 或者某条路由在 Group 上继承了错误的中间件，静态解析一律看不出来。
// 因此这里把**真实路由表**装进引擎，逐条发请求，看它是否真的拦得住。
//
// 判定口径只有一条：**未认证的请求不得触达任何业务 handler**。它同时
// 覆盖两种情况——「忘了挂 requireAuth」与「挂了但没生效」。
const apiPrefix = "/api/v1"

// routeTier 是一条路由的守卫档位。
type routeTier struct {
	Method string
	Path   string // 不含 /api/v1 前缀，保留 :param 原名
	Tier   string // public / user / admin
}

// parseRouteTiers 从 routes_*.go 解析全部路由及其守卫档位。
//
// 路径与档位都取自声明文件而不是 handler 内部：权限边界的唯一权威是
// 路由文件（f-1-06 R-003）。
func parseRouteTiers(t *testing.T) []routeTier {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(repoRoot(t), "internal/router/routes_*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到路由文件: %v", err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	var out []routeTier
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			m := routeRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			tier := "public"
			switch {
			case strings.Contains(line, "g.adminOnly"):
				tier = "admin"
			case strings.Contains(line, "g.requireAuth"):
				tier = "user"
			}
			out = append(out, routeTier{Method: m[1], Path: m[2], Tier: tier})
		}
	}
	if len(out) < 300 {
		t.Fatalf("只解析出 %d 条路由，正则大概失效了", len(out))
	}
	return out
}

// concretePath 把 :param 换成 1。
//
// 值本身不重要：认证中间件在**读 Cookie 之前**就拒绝，路径参数不会被
// 解析。用 1 而不是不存在的 ID，是为了让「万一真的进了 handler」的
// 失败表现更明确（会走到业务校验并返回 4xx，而不是 404 让人误以为是
// 路由没注册）。
func concretePath(p string) string {
	return regexp.MustCompile(`:[A-Za-z_][A-Za-z0-9_]*`).ReplaceAllString(p, "1")
}

// TestAnonymousRequestRejectedOnEveryAuthenticatedRoute 是 U-11 的主用例。
//
// Deps 全空不是偷懒：没有 Cookie 时认证中间件直接 401，根本不碰服务，
// 因此这里能用**真实注册流程**跑完全部端点。反过来说，它同时断言了
// 「路由注册不依赖运行期服务已初始化」——否则本用例会在注册阶段 panic。
func TestAnonymousRequestRejectedOnEveryAuthenticatedRoute(t *testing.T) {
	engine := server.New(server.WithHandleMethodNotAllowed(true))
	router.Register(engine, router.Deps{})

	routes := parseRouteTiers(t)
	var leaked []string
	publicCount, userCount, adminCount := 0, 0, 0
	for _, r := range routes {
		switch r.Tier {
		case "public":
			publicCount++
			continue
		case "admin":
			adminCount++
		default:
			userCount++
		}

		w := ut.PerformRequest(engine.Engine, r.Method, apiPrefix+concretePath(r.Path), nil)
		if w.Code == 401 {
			// 401 的响应体也必须是统一封包：前端按 code 分支引导登录，
			// 运维按 request_id 关联日志。裸 401 会让两者都失效。
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
				RequestID string `json:"request_id"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Error.Code != api.CodeUnauthenticated {
				leaked = append(leaked, r.Method+" "+r.Path+
					"（401 但错误码是 "+body.Error.Code+"）")
			}
			if body.RequestID == "" {
				leaked = append(leaked, r.Method+" "+r.Path+"（401 缺 request_id）")
			}
			continue
		}
		// 403 也算泄漏：未认证的调用方不该知道「这条路由存在且需要管理员」。
		leaked = append(leaked, r.Method+" "+r.Path+"（返回 "+strconv.Itoa(w.Code)+"，期望 401）")
	}
	sort.Strings(leaked)
	if len(leaked) > 0 {
		t.Errorf("以下 %d 条端点未认证即可触达（共 %d 条非公开路由）：\n  %s",
			len(leaked), userCount+adminCount, strings.Join(leaked, "\n  "))
	}
	t.Logf("匿名扫描：公开 %d / 登录 %d / 管理员 %d，共 %d 条",
		publicCount, userCount, adminCount, len(routes))
	if publicCount+userCount+adminCount != len(routes) {
		t.Errorf("档位统计不闭合：%d+%d+%d != %d",
			publicCount, userCount, adminCount, len(routes))
	}
}

// TestRouteTableMatchesDeclarations 断言**引擎里的路由**与**声明文件**一致。
//
// 装饰性路由（OPTIONS、api-endpoints 自身）之外，多一条意味着有代码绕过
// routes_*.go 直接注册（这类路由不会被上面的匿名扫描覆盖，也不会出现在
// 接口文档里）；少一条意味着声明写了但没注册成功。
func TestRouteTableMatchesDeclarations(t *testing.T) {
	engine := server.New(server.WithHandleMethodNotAllowed(true))
	router.Register(engine, router.Deps{})

	declared := map[string]bool{}
	for _, r := range parseRouteTiers(t) {
		declared[r.Method+" "+r.Path] = true
	}
	// /api-endpoints 在 Register 内部注册（它必须排在全部路由之后取快照），
	// 因此不在 routes_*.go 里，单独计入。
	declared["GET /api-endpoints"] = true

	var unexpected []string
	seen := map[string]bool{}
	for _, r := range engine.Routes() {
		if r.Method == "OPTIONS" {
			continue
		}
		rel := strings.TrimPrefix(r.Path, apiPrefix)
		if rel == r.Path {
			// 非 /api/v1 下的路由：目前只有 /health，由 Register 直接登记。
			if r.Path != "/health" {
				unexpected = append(unexpected, r.Method+" "+r.Path+"（不在 /api/v1 下）")
			}
			continue
		}
		key := r.Method + " " + rel
		seen[key] = true
		if !declared[key] {
			unexpected = append(unexpected, key+"（声明文件里没有）")
		}
	}
	var missing []string
	for k := range declared {
		if !seen[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) > 0 {
		t.Errorf("引擎里有 %d 条路由不在声明文件中：\n  %s",
			len(unexpected), strings.Join(unexpected, "\n  "))
	}
	if len(missing) > 0 {
		t.Errorf("声明了 %d 条路由但引擎里没有：\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("引擎路由 %d 条，声明 %d 条", len(seen), len(declared))
}
