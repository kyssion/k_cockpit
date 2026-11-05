package platform

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// adminAPIRoutes 是接口文档里的「管理员专属」清单，真正的权限边界在路由层。
// 两处靠人工同步迟早漂移（曾出现 client-ip 被文档误标为 admin），因此用
// 静态对账把它们锁在一起：增删 adminOnly 路由时，这张表必须一起改。
func TestAdminAPIRoutesMatchRouter(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "router", "routes_*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到路由文件: %v", err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)
	routerAdmin := map[string]bool{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.Contains(line, "g.adminOnly") {
				continue
			}
			m := routeRe.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("%s 中的 adminOnly 注册跨行，对账无法解析：%s", filepath.Base(f), strings.TrimSpace(line))
				continue
			}
			routerAdmin[m[1]+" "+m[2]] = true
		}
	}
	for key := range adminAPIRoutes {
		if !routerAdmin[key] {
			t.Errorf("接口文档标为 admin，但路由未声明 adminOnly：%s", key)
		}
	}
	for key := range routerAdmin {
		if !adminAPIRoutes[key] {
			t.Errorf("路由声明 adminOnly，但接口文档未标注：%s", key)
		}
	}
}
