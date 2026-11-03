package router

import (
	"context"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"k_cockpit/internal/platform/api"
)

// registerStatic 把前端构建产物挂到根路径。
//
// 口径见 docs/04-engineering/DEPLOYMENT.md §1：前端产物由控制面静态托管，
// 单独部署前端没有意义。
//
// 用 NoRoute 而不是 StaticFS("/*filepath")：静态产物只应答「没有匹配到任何
// API 路由」的请求，新增接口永远不会被前端路径抢走，也不必依赖路由树里
// 通配段与静态段的优先级。
//
// 目录不存在时不注册任何东西——开发期前端跑在 vite dev server 上，控制面
// 只提供 API 是常态，不该因此启动失败。
func registerStatic(h *server.Hertz, dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		log.Printf("[router] 静态目录 %q 无法解析为绝对路径，已跳过前端托管: %v", dir, err)
		return
	}
	indexPath := filepath.Join(root, "index.html")
	if _, err := os.Stat(indexPath); err != nil {
		log.Printf("[router] 未找到前端构建产物 %s，本次只提供 API（前端请用 pnpm dev）", indexPath)
		return
	}

	log.Printf("[router] 前端静态托管已启用：%s", root)
	h.NoRoute(staticHandler(root, indexPath))
}

// staticHandler 返回未匹配路由的处理器：先找静态文件，找不到回退到
// index.html（前端是纯 SPA，刷新 /vm/123 这类深链时必须回退）。
func staticHandler(root, indexPath string) app.HandlerFunc {
	return func(_ context.Context, c *app.RequestContext) {
		reqPath := string(c.Path())

		// API 不做 SPA 回退：未匹配的接口必须得到 JSON 404。若回退成
		// index.html，前端会把 HTML 当 JSON 解析，报错位置跑到解析器里，
		// 与"接口不存在"这个真实原因无关。
		if reqPath == "/api" || strings.HasPrefix(reqPath, "/api/") {
			api.Fail(c, api.NotFound("接口不存在"))
			return
		}

		if m := string(c.Method()); m != http.MethodGet && m != http.MethodHead {
			api.Fail(c, api.NotFound("接口不存在"))
			return
		}

		if file, ok := staticFile(root, reqPath); ok {
			// Vite 产物的文件名带内容哈希，可以长缓存；其余（index.html、
			// favicon 等）每次回源，否则发版后客户端会拿着旧壳去请求新接口。
			if strings.HasPrefix(reqPath, "/assets/") {
				c.Header("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				c.Header("Cache-Control", "no-cache")
			}
			c.File(file)
			return
		}

		c.Header("Cache-Control", "no-cache")
		c.File(indexPath)
	}
}

// staticFile 把 URL 路径映射到 root 内的真实文件，并保证不越界。
//
// URL 是 POSIX 语义，与「被管宿主机路径」无关——route 层拿到的请求路径只
// 用于查自己的静态目录，所以这里用 path 做规范化、用 filepath 拼接本机路径。
func staticFile(root, urlPath string) (string, bool) {
	cleaned := path.Clean("/" + urlPath)
	if cleaned == "/" {
		return "", false
	}

	full := filepath.Join(root, filepath.FromSlash(cleaned))

	// 包含性校验：Clean 之后仍可能拼出目录外路径（多段 .. 与分隔符差异），
	// 用 Rel 明确判断，而不是靠字符串前缀（前缀判断会被 userdata-evil 这类
	// 同前缀目录绕过，本项目已踩过）。
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		return "", false
	}
	return full, true
}
