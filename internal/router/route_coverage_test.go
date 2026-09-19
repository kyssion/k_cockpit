package router_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryHandlerMethodIsRouted 断言**每个 handler 的公开方法都有一条路由**。
//
// 这道检查来自一个真实的疏漏：`52e6592`（publicip 与端口转发的批量操作）
// 加了 handler、服务、测试与文档，**但没有改路由文件**——于是那几个方法
// 全部不可达。提交信息说「已实现」，接口文档里也登记了，而用户点不到它。
//
// 这类问题的共同点是**每一处单独看都是对的**：handler 写对了、服务写对了、
// 测试也过了（它们直接调服务层，不经过路由），唯一的缺口在两处之间。而
// 编译不会报错——Go 里一个没被引用的方法完全合法。
//
// 因此这里做一次静态比对：从 router.go 里取出所有「已注册的 handler」，
// 再从 handler 包里取出它们的公开方法，逐一对账。
func TestEveryHandlerMethodIsRouted(t *testing.T) {
	root := repoRoot(t)
	routerSrc := readFile(t, filepath.Join(root, "internal/router/router.go"))

	// 1) 已注册的 handler：变量名 → 类型名。
	//    `publicIPHandler := handler.NewPublicIP(deps.PublicIP)` → publicIPHandler / PublicIP
	//
	// **捕获含 Handler 后缀的完整变量名**：写成 `(\w+)Handler` 会让第一组不含
	// 后缀，而下面「实际使用」那条正则捕获的是含后缀的完整名——两者对不上，
	// 于是每一个方法都会被报成「没有路由」。这个错误的表现是**满屏假阳性**，
	// 而假阳性会让人把整道检查当成没用，然后把它关掉。
	regRe := regexp.MustCompile(`(\w+Handler)\s*:=\s*handler\.New(\w+)\(`)
	registered := map[string]string{} // 变量名 → 类型名
	for _, m := range regRe.FindAllStringSubmatch(routerSrc, -1) {
		registered[m[1]] = m[2]
	}
	if len(registered) < 20 {
		t.Fatalf("只解析出 %d 个已注册 handler，正则大概失效了", len(registered))
	}

	// 2) 路由里实际用到的 `变量名.方法名`。
	useRe := regexp.MustCompile(`(\w+Handler)\.(\w+)`)
	used := map[string]map[string]bool{}
	for _, m := range useRe.FindAllStringSubmatch(routerSrc, -1) {
		if used[m[1]] == nil {
			used[m[1]] = map[string]bool{}
		}
		used[m[1]][m[2]] = true
	}

	// 3) handler 包里的公开方法：类型名 → 方法集合。
	methodRe := regexp.MustCompile(`func \(\w+ \*(\w+)\) ([A-Z]\w*)\(`)
	methods := map[string]map[string]bool{}
	handlerFiles, err := filepath.Glob(filepath.Join(root, "internal/handler/*.go"))
	if err != nil {
		t.Fatalf("列 handler 文件失败: %v", err)
	}
	for _, f := range handlerFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		for _, m := range methodRe.FindAllStringSubmatch(string(src), -1) {
			typ, name := m[1], m[2]
			if methods[typ] == nil {
				methods[typ] = map[string]bool{}
			}
			methods[typ][name] = true
		}
	}

	// 4) 对账。
	//
	// **允许例外，但必须写在这里并说明理由**——一个静默的跳过清单会让这道
	// 检查慢慢失效，而失效的方式是「它一直绿着」。
	allowed := map[string]string{
		// 形如 "类型.方法": "理由"
		//
		// 目前为空：所有 handler 方法都应当有路由。
	}

	var missing []string
	for varName, typ := range registered {
		for method := range methods[typ] {
			if used[varName][method] {
				continue
			}
			key := typ + "." + method
			if reason, ok := allowed[key]; ok {
				t.Logf("已允许的例外 %s：%s", key, reason)
				continue
			}
			missing = append(missing, varName+"."+method+"（"+key+"）")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("以下 handler 方法没有任何路由，因此不可达：\n  %s\n\n"+
			"这类疏漏每一处单独看都是对的（handler 写对了、服务写对了、测试也过了，"+
			"因为测试直接调服务层），唯一的缺口在两处之间，而编译器不会报错。"+
			"要么补路由，要么把它加进本测试的 allowed 清单并写明理由。",
			strings.Join(missing, "\n  "))
	}
}

// TestRegisteredHandlersAreWired 断言每个已注册的 handler 至少被用过一次。
//
// 与上一个测试互补：那个查「方法没接到路由」，这个查「handler 整个没被用」
// ——比如声明了 `xxxHandler := handler.NewXxx(...)` 却一个路由都没挂。
func TestRegisteredHandlersAreWired(t *testing.T) {
	root := repoRoot(t)
	src := readFile(t, filepath.Join(root, "internal/router/router.go"))

	regRe := regexp.MustCompile(`(\w+Handler)\s*:=\s*handler\.New(\w+)\(`)
	useRe := regexp.MustCompile(`(\w+Handler)\.`)

	used := map[string]bool{}
	for _, m := range useRe.FindAllStringSubmatch(src, -1) {
		used[m[1]] = true
	}

	var unused []string
	for _, m := range regRe.FindAllStringSubmatch(src, -1) {
		if !used[m[1]] {
			unused = append(unused, m[1]+"（"+m[2]+"）")
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf("以下 handler 被构造了但一条路由都没挂：\n  %s",
			strings.Join(unused, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	// 测试在 internal/router 下跑。
	return filepath.Dir(filepath.Dir(dir))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return string(b)
}
