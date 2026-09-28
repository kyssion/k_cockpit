package handler

import "testing"

// 覆盖摘要与认证的推断规则（G-50）：CRUD 靠启发式，关键接口靠覆盖表，
// 认证靠显式数据表。
func TestAPIDocsMeta(t *testing.T) {
	cases := []struct {
		method, relPath string
		wantSummary     string
		wantAuth        string
	}{
		{"GET", "/vms", "查询虚拟机列表", "user"},
		{"POST", "/vms", "创建虚拟机", "user"},
		{"GET", "/vms/:id", "查询虚拟机详情", "user"},
		{"DELETE", "/vms/:id", "删除虚拟机", "user"},
		{"POST", "/vms/:id/power", "对虚拟机执行「电源操作」", "user"},
		{"GET", "/settings", "查询系统设置列表", "admin"},
		{"POST", "/auth/login", "登录（多阶段：ok / login_verify / force_password_change / bootstrap_security）", "public"},
		{"GET", "/maintenance", "查询站点维护状态", "admin"},
	}
	for _, tc := range cases {
		key := tc.method + " " + tc.relPath
		if got := summarizeOf(tc.method, tc.relPath); got != tc.wantSummary {
			t.Errorf("summarizeOf(%s) = %q, 期望 %q", key, got, tc.wantSummary)
		}
		if got := authOf(tc.relPath, tc.method); got != tc.wantAuth {
			t.Errorf("authOf(%s) = %q, 期望 %q", key, got, tc.wantAuth)
		}
	}

	// curl：路径参数替换成示例值，认证头按认证方式给。
	curl := curlOf("DELETE", "/api/v1/vms/:id", "user")
	if want := "curl -X DELETE 'http://127.0.0.1:8080/api/v1/vms/1'"; curl[:len(want)] != want {
		t.Errorf("curl 路径参数未替换: %q", curl)
	}
	if curl = curlOf("GET", "/api/v1/setup/status", "public"); len(curl) > 0 && curl[len(curl)-1] == '>' {
		t.Error("公开接口不应附带认证头")
	}
}
