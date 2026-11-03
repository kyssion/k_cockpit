package router

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
)

// newStaticServer 建一个只挂了静态托管的最小引擎。
func newStaticServer(t *testing.T) (*server.Hertz, string) {
	t.Helper()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), `<!doctype html><div id="root"></div>`)
	writeFile(t, filepath.Join(root, "assets", "app-abc123.js"), `console.log("app")`)
	writeFile(t, filepath.Join(root, "favicon.svg"), `<svg/>`)

	h := server.New()
	registerStatic(h, root)
	return h, root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 %s 失败: %v", path, err)
	}
}

// 根路径必须返回 index.html，且不允许缓存——否则发版后客户端拿旧壳。
func TestStaticServesIndexAtRoot(t *testing.T) {
	h, _ := newStaticServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/", nil)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="root"`) {
		t.Fatalf("响应不是 index.html: %s", w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q，期望 no-cache", got)
	}
}

// 带内容哈希的产物可以长缓存，这是首屏性能依赖的口径。
func TestStaticAssetsAreImmutable(t *testing.T) {
	h, _ := newStaticServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/assets/app-abc123.js", nil)
	if w.Code != 200 {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "console.log") {
		t.Fatalf("响应不是静态文件内容: %s", w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q，期望含 immutable", got)
	}
}

// SPA 深链在服务端没有对应文件，刷新必须回退到 index.html。
func TestStaticFallsBackToIndexForSpaRoute(t *testing.T) {
	h, _ := newStaticServer(t)

	for _, p := range []string{"/vm/123", "/settings/security", "/network/base"} {
		w := ut.PerformRequest(h.Engine, "GET", p, nil)
		if w.Code != 200 {
			t.Fatalf("%s 状态码 = %d，期望 200", p, w.Code)
		}
		if !strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatalf("%s 未回退到 index.html: %s", p, w.Body.String())
		}
	}
}

// 未匹配的 API 必须得到 JSON 404，而不是 200 的 HTML。
// 否则前端会把 HTML 当 JSON 解析，报错位置与真实原因无关。
func TestStaticDoesNotSwallowAPINotFound(t *testing.T) {
	h, _ := newStaticServer(t)

	w := ut.PerformRequest(h.Engine, "GET", "/api/v1/not-exist", nil)
	if w.Code != 404 {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 JSON", ct)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v（body=%s）", err, w.Body.String())
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("错误响应缺少 code/message: %s", w.Body.String())
	}
}

// 非 GET/HEAD 的未知路径不该被静态处理器接走。
func TestStaticOnlyServesGetAndHead(t *testing.T) {
	h, _ := newStaticServer(t)

	w := ut.PerformRequest(h.Engine, "POST", "/vm/123", nil)
	if w.Code != 404 {
		t.Fatalf("状态码 = %d，期望 404", w.Code)
	}
}

// 路径穿越必须被挡住：Normalize 之后要么落在 root 内、要么判定不存在。
func TestStaticFileRejectsEscapingPaths(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "index.html"), "index")

	cases := []string{
		"/../../etc/passwd",
		"/../go.mod",
		"/..%2f..%2fetc/passwd",
		"/....//....//etc/passwd",
	}
	for _, p := range cases {
		if got, ok := staticFile(root, p); ok {
			t.Fatalf("%q 越界命中 %s", p, got)
		}
	}
}

// 目录不存在时静默跳过：开发期前端跑在 vite dev server 上，
// 控制面不该因此启动失败或拦截 API 之外的请求。
func TestStaticSkippedWhenDirMissing(t *testing.T) {
	h := server.New()
	registerStatic(h, filepath.Join(t.TempDir(), "missing"))

	w := ut.PerformRequest(h.Engine, "GET", "/", nil)
	if w.Code == 200 {
		t.Fatalf("目录不存在时不应提供静态内容（body=%s）", w.Body.String())
	}
}
