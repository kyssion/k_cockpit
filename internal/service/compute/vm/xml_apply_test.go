package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
)

// xmlApplyClient 只替换 validate/define 的结果，其余沿用本包既有 stub。
// 用它可以精确构造「节点回报 Success=true 却拿不到合法 VMXMLValidateInfo」
// 这一内部故障，而不必去改生产代码或依赖真机。
func xmlApplyClient(validate *agent.Result) *stubClient {
	c := newStubClient()
	c.exec = func(_ context.Context, op agent.Operation) (*agent.Result, error) {
		switch op.Kind {
		case agent.OpVMXML:
			// PrecheckXML 先读当前定义作为 diff 基准。
			return &agent.Result{Success: true, Data: map[string]any{
				agent.VMXMLKey: agent.VMXMLInfo{XML: "<domain><name>xml-e2e</name><vcpu>2</vcpu></domain>"},
			}}, nil
		case agent.OpVMXMLApply:
			if action, _ := op.Params["action"].(string); action == "validate" {
				return validate, nil
			}
			return &agent.Result{Success: true, Message: "域定义已更新"}, nil
		default:
			return &agent.Result{Success: true, Message: "stub"}, nil
		}
	}
	return c
}

func newXMLTestEnv(t *testing.T, client *stubClient) (*vm.Service, int64) {
	t.Helper()
	svc, _, db := newTestEnvWithClient(t, client)
	if err := db.Create(&model.VM{
		NodeID: 1, Name: "xml-e2e", Status: model.VMStatusRunning,
		VCPU: 2, MemoryMB: 4096, DiskGB: 40, CloneMode: model.CloneFull, Present: true,
	}).Error; err != nil {
		t.Fatalf("插入测试虚拟机失败: %v", err)
	}
	var row model.VM
	if err := db.Where("name = ?", "xml-e2e").First(&row).Error; err != nil {
		t.Fatalf("读回测试虚拟机失败: %v", err)
	}
	return svc, row.ID
}

func asAPIError(t *testing.T, err error) *api.Error {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望 *api.Error，实际 %T: %v", err, err)
	}
	return apiErr
}

// TestPrecheckXMLRejectsMissingValidateInfo 是 DEF-001 的核心回归：节点
// Success=true 却没给出可解析的校验结果时，必须返回明确错误，而不是留
// Valid=false 静默放行（那会把内部故障伪装成「用户提交的 XML 非法」）。
func TestPrecheckXMLRejectsMissingValidateInfo(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"缺该键", map[string]any{}},
		{"类型错误（通用 JSON 解码后的 map）", map[string]any{
			agent.VMXMLValidateKey: map[string]any{"Valid": true, "Errors": []any{}},
		}},
		{"标量占位", map[string]any{agent.VMXMLValidateKey: "ok"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, id := newXMLTestEnv(t, xmlApplyClient(&agent.Result{Success: true, Data: tc.data}))
			out, err := svc.PrecheckXML(context.Background(), id,
				"<domain><name>xml-e2e</name><vcpu>4</vcpu></domain>",
				authz.Viewer{UserID: 1, IsAdmin: true})
			if err == nil {
				t.Fatalf("应返回明确错误，却静默返回 out=%+v（Valid=%v）", out, out != nil && out.Valid)
			}
			apiErr := asAPIError(t, err)
			if apiErr.Status != 503 || apiErr.Code != api.CodeServiceUnavailable {
				t.Errorf("应是 503 SERVICE_UNAVAILABLE（依赖/协议故障），实际 %d %s", apiErr.Status, apiErr.Code)
			}
			// 绝不能降级成 422 VALIDATION_FAILED —— 那是把内部故障推给用户。
			if apiErr.Code == api.CodeValidationFailed {
				t.Errorf("不得把内部解码故障伪装成用户输入错误")
			}
			if !strings.Contains(apiErr.Message, "格式异常") {
				t.Errorf("错误信息应指向校验结果格式，实际 %q", apiErr.Message)
			}
		})
	}
}

// TestPrecheckXMLPassesValidInfo 是正向对照：登记解码器后，合法的
// VMXMLValidateInfo 三个字段都应被原样带出，precheck 判为可用。
func TestPrecheckXMLPassesValidInfo(t *testing.T) {
	info := agent.VMXMLValidateInfo{
		Valid:    true,
		Warnings: []string{"节点缺少 virt-xml-validate，未做 schema 校验"},
	}
	svc, id := newXMLTestEnv(t, xmlApplyClient(&agent.Result{
		Success: true,
		Data:    map[string]any{agent.VMXMLValidateKey: info},
	}))
	out, err := svc.PrecheckXML(context.Background(), id,
		"<domain><name>xml-e2e</name><vcpu>4</vcpu></domain>",
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("合法校验结果不应报错: %v", err)
	}
	if !out.Valid {
		t.Errorf("Valid 应为 true，实际 false（errors=%v）", out.Errors)
	}
	if len(out.Warnings) != 1 || out.Warnings[0] != info.Warnings[0] {
		t.Errorf("Warnings 未原样带出: %v", out.Warnings)
	}
}

// TestPrecheckXMLHonestInvalid 覆盖节点判「非法」的诚实路径：Success=true
// 但 Valid=false 且带具体 Errors —— 这是真实的用户输入错误，必须如实回显。
func TestPrecheckXMLHonestInvalid(t *testing.T) {
	info := agent.VMXMLValidateInfo{Valid: false, Errors: []string{
		"Relax-NG validity error : line 3: Element domain has extra content: bogus",
	}}
	svc, id := newXMLTestEnv(t, xmlApplyClient(&agent.Result{
		Success: true,
		Data:    map[string]any{agent.VMXMLValidateKey: info},
	}))
	out, err := svc.PrecheckXML(context.Background(), id,
		"<domain><name>xml-e2e</name><bogus/></domain>",
		authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("precheck 本身不应报错（结果由 Valid 表达）: %v", err)
	}
	if out.Valid {
		t.Errorf("非法定义应判 Valid=false")
	}
	if len(out.Errors) != 1 || !strings.Contains(out.Errors[0], "Relax-NG") {
		t.Errorf("节点的具体原因应原样带出，实际 %v", out.Errors)
	}
}

// TestUpdateXMLEmptyErrorsFallback 是 DEF-004 的回归：即使节点判不合法却
// 没给任何明细，PUT 的 422 文案也不能是「定义未通过校验：」这种空尾巴。
func TestUpdateXMLEmptyErrorsFallback(t *testing.T) {
	// Errors 为空的合法 VMXMLValidateInfo（节点只回 Valid=false）。
	svc, id := newXMLTestEnv(t, xmlApplyClient(&agent.Result{
		Success: true,
		Data: map[string]any{
			agent.VMXMLValidateKey: agent.VMXMLValidateInfo{Valid: false},
		},
	}))
	ctx := vm.WithXMLVerified(context.Background())
	_, err := svc.UpdateXML(ctx, id,
		"<domain><name>xml-e2e</name><vcpu>4</vcpu></domain>",
		authz.Viewer{UserID: 1, IsAdmin: true}, "tester", "127.0.0.1")
	if err == nil {
		t.Fatal("非法定义应返回 422")
	}
	apiErr := asAPIError(t, err)
	if apiErr.Status != 422 || apiErr.Code != api.CodeValidationFailed {
		t.Errorf("应是 422 VALIDATION_FAILED，实际 %d %s", apiErr.Status, apiErr.Code)
	}
	if strings.TrimSpace(apiErr.Message) == "" || strings.HasSuffix(apiErr.Message, "：") {
		t.Errorf("422 文案不得为空或以裸冒号结尾: %q", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "节点未返回校验详情") {
		t.Errorf("空 Errors 时应给出兜底提示，实际 %q", apiErr.Message)
	}
}

// TestUpdateXMLKeepsConcreteReasons 确认有明细时原样拼接，兜底不篡改
// libvirt 的行号/元素名信息（那条信息是用户定位改坏了哪一行的唯一依据）。
func TestUpdateXMLKeepsConcreteReasons(t *testing.T) {
	info := agent.VMXMLValidateInfo{Valid: false, Errors: []string{
		"line 3: unknown element <bogus/>", "line 9: missing <memory/>",
	}}
	svc, id := newXMLTestEnv(t, xmlApplyClient(&agent.Result{
		Success: true,
		Data:    map[string]any{agent.VMXMLValidateKey: info},
	}))
	ctx := vm.WithXMLVerified(context.Background())
	_, err := svc.UpdateXML(ctx, id,
		"<domain><name>xml-e2e</name><bogus/></domain>",
		authz.Viewer{UserID: 1, IsAdmin: true}, "tester", "127.0.0.1")
	if err == nil {
		t.Fatal("非法定义应返回 422")
	}
	apiErr := asAPIError(t, err)
	for _, want := range []string{"line 3", "line 9", "；"} {
		if !strings.Contains(apiErr.Message, want) {
			t.Errorf("422 文案应保留具体原因 %q，实际 %q", want, apiErr.Message)
		}
	}
	if strings.Contains(apiErr.Message, "节点未返回校验详情") {
		t.Errorf("有明细时不应触发兜底文案: %q", apiErr.Message)
	}
}
