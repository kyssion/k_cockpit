package platform

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// 摘要的底线：中文接口文档里不允许漏出路径原文。
//
// 这条测试因实机缺陷而生：`POST /host/passthrough/bind` 曾被推断成
// 「创建host」、`GET /ovs/status` 成「查询ovs列表」——生成规则只认识
// 一级模块名，遇到未登记的首段或子资源就直接把英文拼进中文句子。
//
// 与 TestAdminAPIRoutesMatchRouter 同一手法：直接扫描路由文件，保证
// 「新加路由」时这条约束自动生效，而不是等人去补测试用例。
func TestAPIDocsSummariesUseChinese(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "router", "routes_*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("找不到路由文件: %v", err)
	}
	routeRe := regexp.MustCompile(`v1\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"`)

	checked := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", f, err)
		}
		for _, m := range routeRe.FindAllStringSubmatch(string(b), -1) {
			method, path := m[1], m[2]
			rel := strings.TrimPrefix(path, "/api/v1")
			// 覆盖表是手写文案，允许出现阶段名等英文（如登录的多阶段返回值）。
			if _, hand := summaryOverrides[method+" "+rel]; hand {
				continue
			}
			summary := summarizeOf(method, rel)
			checked++

			if strings.TrimSpace(summary) == "" {
				t.Errorf("%s %s: 摘要为空", method, rel)
				continue
			}
			if !hasCJK(summary) {
				t.Errorf("%s %s: 摘要没有中文（%q）", method, rel, summary)
			}
			// 路径段原文漏出检查：参数段跳过，其余逐段比对（区分大小写，
			// 「OVS / VPC / ACL」这类缩写是允许的写法）。
			for _, seg := range strings.Split(rel, "/") {
				if seg == "" || strings.HasPrefix(seg, ":") {
					continue
				}
				if strings.Contains(summary, seg) {
					t.Errorf("%s %s: 摘要漏出路径原文 %q（%q）", method, rel, seg, summary)
				}
			}
		}
	}
	if checked < 100 {
		t.Fatalf("只检查了 %d 条路由，路由文件解析可能失效", checked)
	}
}

func hasCJK(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}
