// Package version 提供面板版本、组件版本与依赖清单（F-9-05）。
//
// 有一条原则贯穿本包：**全部来自构建信息，一个字段都不手写。**
//
// 手写的版本清单在第一次 `go mod tidy` 之后就漂移了，而它恰恰是排查
// 「这个版本里用的是哪个库」「跑的是哪个提交」时要看的东西。一份漂移的
// 清单比没有更糟：它会给出一个看起来很具体的错误答案，而人不会去质疑它。
//
// 与 F-9-04（内置 API 文档「由路由与接口定义生成，避免手写漂移」）是同一条
// 原则——凡是"代码里本来就有的东西"，就不要在别处再写一遍。
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
)

// Info 是版本与构建信息。
type Info struct {
	// PanelVersion 是面板版本。未由构建注入时为空——**如实留空**，
	// 而不是填一个 "dev" 假装它是版本号。
	PanelVersion string `json:"panel_version"`
	// GoVersion 是编译用的 Go 版本。
	GoVersion string `json:"go_version"`
	// Platform 是目标平台（如 linux/amd64）。
	Platform string `json:"platform"`
	// Revision 是 VCS 提交号，Dirty 表示工作区有未提交改动。
	//
	// 这两项是排障时**最有用**的一对：它们回答"跑的是哪个提交、那个提交
	// 是不是就是仓库里能看到的那份"。没有它们时，唯一的办法是去比对
	// 二进制的时间戳。
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	// BuildTime 是构建时刻（由构建脚本注入时才有）。
	BuildTime string `json:"build_time,omitempty"`
	// BuildAvailable 为 false 表示二进制里没有构建信息。
	//
	// `go run` 与某些构建方式下拿不到，这时界面上应当说明"这份运行没有
	// 可用的构建信息"，而不是显示一片空白让人以为功能坏了。
	BuildAvailable bool `json:"build_available"`
	// Dependencies 是直接与间接依赖清单。
	Dependencies []Dependency `json:"dependencies"`
	// Replacements 是被替换的模块（replace 指令）。
	//
	// 单独列出来而不是混在依赖里：一个被 replace 的模块意味着**实际跑的
	// 不是官方版本**，而这正是"本地能跑、线上出问题"这类现象最常见的原因。
	Replacements []Dependency `json:"replacements,omitempty"`
}

// Dependency 是一条依赖。
type Dependency struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// 由构建脚本通过 -ldflags -X 注入。
//
// 留空是合法的：开发环境下 `go run` 不会注入，而那时界面应当如实说明，
// 而不是显示一个编出来的版本号。
var (
	panelVersion string
	buildTime    string
)

// Get 汇总版本与构建信息。
func Get() Info {
	info := Info{
		PanelVersion: panelVersion,
		BuildTime:    buildTime,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		// **如实报告"没有"**，而不是返回一个空结构让人以为接口挂了。
		return info
	}
	info.BuildAvailable = true

	if info.PanelVersion == "" {
		// 模块版本由 Go 自己填（从版本控制推导），比自己猜准。
		// 主模块在开发构建时是 "(devel)"，那不是一个可用的版本号，
		// 因此不把它当成面板版本。
		if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			info.PanelVersion = bi.Main.Version
		}
	}

	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.modified":
			info.Dirty = s.Value == "true"
		case "vcs.time":
			if info.BuildTime == "" {
				info.BuildTime = s.Value
			}
		}
	}

	for _, d := range bi.Deps {
		dep := Dependency{Path: d.Path, Version: d.Version}
		if d.Replace != nil {
			// 被替换的模块单独归类，见 Replacements 的说明。
			info.Replacements = append(info.Replacements, Dependency{
				Path:    d.Path,
				Version: d.Replace.Path + "@" + d.Replace.Version,
			})
			continue
		}
		info.Dependencies = append(info.Dependencies, dep)
	}

	// 排序让清单稳定：一份每次刷新顺序都不同的依赖表没法逐行比对，
	// 而"和上次比多了什么"恰恰是升级时最常做的动作。
	sort.Slice(info.Dependencies, func(i, j int) bool {
		return info.Dependencies[i].Path < info.Dependencies[j].Path
	})
	sort.Slice(info.Replacements, func(i, j int) bool {
		return info.Replacements[i].Path < info.Replacements[j].Path
	})
	return info
}

// Summary 返回一行版本摘要，供诊断包与日志使用。
//
// 格式刻意做成"人一眼能读"的一行：它最常见的去处是一份排障包的 MANIFEST
// 或一条 issue 的开头，而那里需要的是一句话而不是一个 JSON。
func Summary() string {
	i := Get()
	parts := []string{fmt.Sprintf("Go %s", strings.TrimPrefix(i.GoVersion, "go"))}
	if i.PanelVersion != "" {
		parts = append([]string{i.PanelVersion}, parts...)
	}
	if i.Revision != "" {
		rev := i.Revision
		if len(rev) > 8 {
			rev = rev[:8]
		}
		if i.Dirty {
			// 「有未提交改动」必须标出来：一个 dirty 的构建无法用提交号
			// 复现，而这正是"我本地是对的"这类问题的根源。
			rev += "（有未提交改动）"
		}
		parts = append(parts, rev)
	}
	parts = append(parts, i.Platform)
	return strings.Join(parts, " · ")
}
