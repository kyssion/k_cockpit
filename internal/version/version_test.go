package version_test

import (
	"strings"
	"testing"

	"k_cockpit/internal/version"
)

// TestGetReportsRealEnvironment 覆盖"字段来自真实的运行时环境"。
func TestGetReportsRealEnvironment(t *testing.T) {
	info := version.Get()

	if info.GoVersion == "" {
		t.Error("Go 版本不能为空——它来自 runtime.Version()，总是拿得到")
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Errorf("Go 版本格式不对: %q", info.GoVersion)
	}
	if info.Platform == "" || !strings.Contains(info.Platform, "/") {
		t.Errorf("平台格式应为 os/arch，实际 %q", info.Platform)
	}
}

// TestDependenciesComeFromBuildInfo 覆盖本包的核心原则。
//
// **依赖清单来自构建信息，一个字段都不手写。** 手写的清单在第一次
// `go mod tidy` 之后就漂移了，而它恰恰是排查「这个版本用的是哪个库」时
// 要看的东西——一份漂移的清单比没有更糟，它给出一个看起来很具体的错误答案。
func TestDependenciesComeFromBuildInfo(t *testing.T) {
	info := version.Get()

	// 测试是用 `go test` 跑的，构建信息一定存在。
	if !info.BuildAvailable {
		t.Fatal("测试进程应有构建信息")
	}
	// **不断言「一定有依赖」**：本包的测试二进制只链了标准库，因此它的
	// 依赖清单本来就是空的——而这不是缺陷，恰恰说明了构建信息的价值：
	// 它反映的是**这个二进制里实际链了什么**，而不是 go.mod 里写了什么。
	// 后者包含大量根本不会被链接的东西。
	//
	// 真正要断言的是机制本身：有依赖时它们被列出来且有序。

	// 排序必须是稳定的：一份每次顺序都不同的依赖表没法逐行比对，
	// 而「和上次比多了什么」正是升级时最常做的动作。
	for i := 1; i < len(info.Dependencies); i++ {
		if info.Dependencies[i-1].Path > info.Dependencies[i].Path {
			t.Fatalf("依赖清单未按路径排序: %q 在 %q 之前",
				info.Dependencies[i-1].Path, info.Dependencies[i].Path)
		}
	}
	for _, d := range info.Dependencies {
		if d.Path == "" {
			t.Error("依赖必须有路径")
		}
	}
}

// TestSummaryIsReadable 覆盖摘要的用途。
//
// 它最常见的去处是一份排障包的开头或一条 issue 的第一行，那里需要的是一句
// 话而不是一个 JSON。
func TestSummaryIsReadable(t *testing.T) {
	s := version.Summary()
	if s == "" {
		t.Fatal("摘要不能为空")
	}
	if strings.Contains(s, "{") || strings.Contains(s, "\n") {
		t.Errorf("摘要应是一行纯文本，实际 %q", s)
	}
	if !strings.Contains(s, "Go ") {
		t.Errorf("摘要应含 Go 版本: %q", s)
	}
}

// TestNothingIsFabricated 覆盖"没有就如实说"。
//
// `go run` 与某些构建方式下拿不到构建信息，这时应当说明「这份运行没有可用
// 的构建信息」，而不是显示一片空白让人以为功能坏了——更不该编一个版本号。
func TestNothingIsFabricated(t *testing.T) {
	info := version.Get()

	// 面板版本在开发构建下可能为空（模块版本是 "(devel)"）——那时必须是
	// 空字符串，而不是 "(devel)" 这个不是版本号的东西。
	if info.PanelVersion == "(devel)" {
		t.Error("(devel) 不是可用的版本号，应当留空")
	}
	// 摘要里也不该出现它。
	if strings.Contains(version.Summary(), "(devel)") {
		t.Errorf("摘要里不该出现 (devel): %q", version.Summary())
	}
}
