package handler

import (
	"context"
	"sort"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"

	"k_cockpit/internal/api"
)

// Endpoint 是一条已注册的路由。
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// APIDocs 提供接口清单（F-9-07）。
type APIDocs struct {
	endpoints []Endpoint
}

// NewAPIDocs 构造接口。
//
// 清单在**路由注册完成后**从框架里取出，而不是在代码里再维护一份：
// 手写清单的命运只有一个——第一次加接口时忘了同步，而那时它错得毫无痕迹。
func NewAPIDocs(h *server.Hertz) *APIDocs {
	routes := h.Routes()
	out := make([]Endpoint, 0, len(routes))
	for _, r := range routes {
		// Hertz 为 CORS 自动注册的 OPTIONS 路由不是我们的接口能力，
		// 列出来只会让人以为需要调用它。
		if r.Method == "OPTIONS" {
			continue
		}
		out = append(out, Endpoint{Method: r.Method, Path: r.Path})
	}
	// 排序：同一份数据每次都按同样的顺序返回，界面上的分组才不会跳动。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].Method < out[j].Method
		}
		return out[i].Path < out[j].Path
	})
	return &APIDocs{endpoints: out}
}

// List 返回全部已注册的接口。
func (d *APIDocs) List(ctx context.Context, c *app.RequestContext) {
	api.OK(c, map[string]any{"endpoints": d.endpoints})
}
