package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestCreateFormPublishesFieldRules 覆盖「规则由后端下发」。
//
// 前端不再维护第二份取值表，因此**矩阵里有什么**就是唯一的答案：向导里
// 少一项，用户就少一个本来可以设的配置。
func TestCreateFormPublishesFieldRules(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	form, err := svc.CreateFormOf(ctx, 1, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取创建表单失败: %v", err)
	}
	if len(form.Fields) == 0 {
		t.Fatal("创建表单没有任何字段")
	}

	byKey := map[string]vm.EditField{}
	for _, f := range form.Fields {
		byKey[f.Key] = f
	}
	// 规格三件套与磁盘、网络、系统配置都要在。
	for _, key := range []string{"disk_gb", "disk_format", "disk_bus", "nic_model", "os_type", "firmware", "machine_type"} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("创建表单缺少 %s", key)
		}
	}
	// 只读的探测结果不该出现在创建表单里：它不是用户能设定的东西。
	if _, ok := byKey["guest_agent"]; ok {
		t.Error("探测结果 guest_agent 不该出现在创建表单")
	}
	// 默认值必须下发，否则界面上的初始值全靠前端猜。
	if byKey["disk_gb"].Default == "" {
		t.Error("disk_gb 没有默认值")
	}
	if form.Values["vcpu"] != 2 {
		t.Errorf("vcpu 默认值 = %v, 期望 2", form.Values["vcpu"])
	}
	// 步骤顺序由后端给定：新增一个分组时只改一处。
	if len(form.Groups) == 0 || form.Groups[0].Key != vm.EditGroupBasic {
		t.Errorf("步骤顺序 = %+v, 期望以基础信息开头", form.Groups)
	}
	if !form.CanSubmit {
		t.Errorf("前置条件齐全时 CanSubmit 应为 true: %+v", form.Prerequisites)
	}
}

// TestCreateFormReportsMissingPrerequisites 覆盖前置拦截。
//
// 没有存储池时创建必定失败，而这个事实应该在**打开向导时**就说清楚，
// 而不是等用户填完八步才被告知。
func TestCreateFormReportsMissingPrerequisites(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	if err := db.Where("node_id = ?", 1).Delete(&model.StoragePool{}).Error; err != nil {
		t.Fatalf("清空存储池失败: %v", err)
	}

	form, err := svc.CreateFormOf(ctx, 1, authz.Viewer{UserID: 7})
	if err != nil {
		t.Fatalf("获取创建表单失败: %v", err)
	}
	if form.CanSubmit {
		t.Fatal("缺少存储池时仍允许提交")
	}
	for _, p := range form.Prerequisites {
		if p.Key == vm.PreStorage {
			if p.OK {
				t.Error("存储池前置条件被判为通过")
			}
			if p.Message == "" || p.Link == "" {
				t.Errorf("前置条件缺少可执行的提示或入口: %+v", p)
			}
		}
	}
}

// TestCreateBatchEnqueuesOneTaskPerVM 覆盖批量语义（f-2-02 R-007）。
func TestCreateBatchEnqueuesOneTaskPerVM(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	tasks, err := svc.CreateBatch(ctx, vm.CreateRequest{
		Name: "web", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		Count: 3, BatchKey: "batch-1",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("批量创建失败: %v", err)
	}
	if len(tasks) != 3 {
		t.Fatalf("任务数 = %d, 期望 3", len(tasks))
	}
	want := []string{"web-1", "web-2", "web-3"}
	for i, name := range want {
		got := ""
		if tasks[i].ResourceName != nil {
			got = *tasks[i].ResourceName
		}
		if got != name {
			t.Errorf("第 %d 台名称 = %q, 期望 %q", i+1, got, name)
		}
		// 每台一个任务、任务 ID 互不相同：单台失败不该影响其它台的结论。
		if i > 0 && tasks[i].ID == tasks[i-1].ID {
			t.Error("多台共用了同一个任务")
		}
	}
}

// TestCreateIsIdempotentByClientToken 覆盖幂等（f-2-02 R-004）。
//
// 重复点击是客户端行为，只有客户端生成的 token 才能识别「这是同一次提交」。
func TestCreateIsIdempotentByClientToken(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	req := vm.CreateRequest{
		Name: "idem", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		ClientToken: "token-abc",
	}
	first, err := svc.Create(ctx, req, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	second, err := svc.Create(ctx, req, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("重复创建失败: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("重复提交产生了新任务: %d vs %d", first.ID, second.ID)
	}
}

// TestCreateRejectsDuplicateNameWithSuggestion 覆盖重名（f-2-02 R-010）。
func TestCreateRejectsDuplicateNameWithSuggestion(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	if err := db.Create(&model.VM{NodeID: 1, Name: "taken"}).Error; err != nil {
		t.Fatalf("预置虚拟机失败: %v", err)
	}

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "taken", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("重名未被拒绝")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != 409 {
		t.Errorf("状态码 = %d, 期望 409", apiErr.Status)
	}
	// 报错里必须给出可用名字：只说「已存在」等于让用户自己去试。
	if !strings.Contains(apiErr.Message, "taken-2") {
		t.Errorf("错误文案未给出建议名: %q", apiErr.Message)
	}
}

// TestCreateRejectsExclusiveIOPS 覆盖 IOPS 互斥（与编辑页同一条规则）。
func TestCreateRejectsExclusiveIOPS(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "iops", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		DiskIOPSTotal: 1000, DiskIOPSRead: 500,
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("同时设置总量与读写分离未被拒绝")
	}
}

// TestCreateRejectsIllegalEnum 覆盖「按矩阵校验取值」。
//
// 前端的下拉框只来自同一份矩阵，因此这个用例针对的是**绕过界面**的调用
// ——校验必须落在服务端，否则矩阵就只是一份装饰性的文档。
func TestCreateRejectsIllegalEnum(t *testing.T) {
	svc, _, _ := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	_, err := svc.Create(ctx, vm.CreateRequest{
		Name: "bad", NodeID: 1, VCPU: 2, MemoryMB: 2048, DiskGB: 20,
		DiskBus: "nvme",
	}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("非法磁盘驱动未被拒绝")
	}
	if !strings.Contains(err.Error(), "磁盘驱动") {
		t.Errorf("错误未定位到具体字段: %v", err)
	}
}
