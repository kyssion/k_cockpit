package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/version"
)

// Version 提供版本与关于信息（F-9-05）。
//
// **不限制角色**：这一页的全部内容是版本号与依赖清单，它不含任何租户数据，
// 也不含任何密钥。而它最常见的用途是——用户遇到问题时先看一眼自己跑的是
// 哪个版本，再去提问。把这个入口藏起来只会让他去找别的地方猜。
type Version struct{}

// NewVersion 构造接口。
func NewVersion() *Version { return &Version{} }

// Get 返回版本与构建信息（API-280）。
func (h *Version) Get(ctx context.Context, c *app.RequestContext) {
	api.OK(c, version.Get())
}
