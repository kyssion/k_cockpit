package hosttuning_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/hosttuning"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *hosttuning.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "tune.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.CPUAffinityPreset{}, &model.Node{}, &model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	db.Create(&model.Node{ID: 1, Name: "node-1"})
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(hosttuning.NewExecutor(db, client))
	return db, hosttuning.NewService(db, q, client, recorder)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}

// TestStateCarriesEffectNotJustSwitch 覆盖本包最重要的一条。
//
// 只显示「已启用」的话，用户无法回答两个最实际的问题：该不该开、开了有没有用。
// KSM 尤其如此——它**持续消耗 CPU**，在什么都没合并的时候照样扫描内存。
func TestStateCarriesEffectNotJustSwitch(t *testing.T) {
	_, svc := newEnv(t)

	view, err := svc.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 收益：省下的内存由**节点**算好（页大小随架构不同，控制面猜会算错）。
	if view.KSM.SavedBytes <= 0 {
		t.Error("KSM 必须给出收益数字——只给开关，用户无法判断它有没有用")
	}
	// 收益的构成：共享页数与引用数都在，才有可解释性。
	if view.KSM.PagesShared == 0 || view.KSM.PagesSharing == 0 {
		t.Error("应给出共享页数与引用数——它们的差才是收益")
	}
	// **扫描轮次**：它回答"开了但省了 0"是正常还是异常。
	if view.KSM.FullScans <= 0 {
		t.Error("应给出扫描轮次——轮次多而收益为 0 说明该关掉它")
	}
	// ZRAM 的收益只能用压缩率表达。
	if view.ZRAM.OrigDataBytes <= view.ZRAM.UsedBytes {
		t.Error("ZRAM 应给出压缩前的数据量——没有它就读不出压缩率")
	}
	if view.ZRAM.Algorithm == "" {
		t.Error("应给出压缩算法：lz4 与 zstd 是快与省之间的取舍")
	}
}

// TestNestedNonPersistentIsSurfaced 覆盖「重启就没了」。
//
// 用户改完看到"已启用"，重启之后又变回去——而他会以为是面板没保存成功。
func TestNestedNonPersistentIsSurfaced(t *testing.T) {
	_, svc := newEnv(t)

	view, err := svc.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if view.Nested.Enabled && !view.Nested.Persistent && view.Nested.Fix == "" {
		t.Error("嵌套虚拟化已启用但不持久时，必须告诉用户怎么持久化——" +
			"否则他重启之后会以为是面板没保存成功")
	}
}

// TestItemsExplainCost 覆盖代价说明。
//
// KSM 与 ZRAM 的开关看起来一样，而代价完全不同。把代价藏起来，用户只能
// 凭感觉选——而"感觉"在内存与 CPU 的取舍上没有依据。
func TestItemsExplainCost(t *testing.T) {
	items := hosttuning.Items()
	if len(items) != 3 {
		t.Fatalf("应有 3 个调优项，实际 %d", len(items))
	}
	for _, it := range items {
		if it.Key == "" || it.Label == "" {
			t.Errorf("项缺少标识或名称: %+v", it)
		}
		// 三项都不能省：收益说清"为什么要开"，代价说清"开了会失去什么"，
		// 度量说清"怎么判断它有没有用"。
		if it.Benefit == "" {
			t.Errorf("%s 缺少收益说明", it.Key)
		}
		if it.Cost == "" {
			t.Errorf("%s 缺少代价说明——这一项不能省", it.Key)
		}
		if it.Metric == "" {
			t.Errorf("%s 缺少判断依据", it.Key)
		}
	}
}

// TestCPUSetRejectsInjection 覆盖 cpuset 的格式校验。
//
// 这个字符串会被拼进 libvirt 的域配置，因此只接受数字、逗号与连字符——
// 少一种可表达的写法，就少一整类绕过方式（与目录共享只收相对路径同理）。
func TestCPUSetRejectsInjection(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	bad := []string{
		"0-3; rm -rf /",
		"0-3 && reboot",
		"0-3|cron",
		"$(whoami)",
		"0-3\n8",
		"0-3'",
		`0-3"`,
		"0-3/../../etc",
	}
	for _, c := range bad {
		_, err := svc.CreatePreset(ctx, hosttuning.PresetRequest{
			NodeID: 1, Name: "x", CPUSet: c,
		}, admin(), "root", "")
		if err == nil {
			t.Errorf("cpuset %q 应被拒绝", c)
		}
	}
}

// TestCPUSetAcceptsValidForms 覆盖**只测拒绝是不够的**。
//
// 一组只测拒绝的用例拦不住一个"全都拒绝"的实现，而那样这个功能就等于没有。
func TestCPUSetAcceptsValidForms(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	good := []string{"0", "0-3", "0,2,4", "0-1,4-5", "12"}
	for i, c := range good {
		if _, err := svc.CreatePreset(ctx, hosttuning.PresetRequest{
			NodeID: 1, Name: "p" + itoa(i), CPUSet: c,
		}, admin(), "root", ""); err != nil {
			t.Errorf("cpuset %q 应被接受，实际 %v", c, err)
		}
	}
}

// TestCPUSetDescExpands 覆盖人话展开。
//
// 「0-3,8」要用户在脑子里展开，而展开错了的后果是"我明明绑了 4 个核却只用了
// 2 个"——那种问题不报错，只表现为性能不如预期。
func TestCPUSetDescExpands(t *testing.T) {
	_, svc := newEnv(t)

	view, err := svc.CreatePreset(context.Background(), hosttuning.PresetRequest{
		NodeID: 1, Name: "四核", CPUSet: "0-3",
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if !strings.Contains(view.CPUSetDesc, "4") {
		t.Errorf("应展开出核数，实际 %q", view.CPUSetDesc)
	}

	v2, _ := svc.CreatePreset(context.Background(), hosttuning.PresetRequest{
		NodeID: 1, Name: "混合", CPUSet: "0-1,4-5",
	}, admin(), "root", "")
	if !strings.Contains(v2.CPUSetDesc, "4") {
		t.Errorf("0-1,4-5 合计 4 核，实际 %q", v2.CPUSetDesc)
	}
}

// TestPresetNameUniqueAmongLiveRows 覆盖软删除之后的同名重建。
//
// 唯一索引不给 `where deleted_at is null` 的话，删掉「高性能」之后就再也
// 建不了同名的了——用户会撞上一句他无法理解、也无法自行解决的约束冲突。
func TestPresetNameUniqueAmongLiveRows(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()

	view, err := svc.CreatePreset(ctx, hosttuning.PresetRequest{
		NodeID: 1, Name: "高性能", CPUSet: "0-3",
	}, admin(), "root", "")
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}

	// 同名冲突。
	_, err = svc.CreatePreset(ctx, hosttuning.PresetRequest{
		NodeID: 1, Name: "高性能", CPUSet: "4-7",
	}, admin(), "root", "")
	assertStatus(t, err, 409)

	// 删掉之后同名可以重建。
	if err := svc.DeletePreset(ctx, 1, view.ID, admin(), "root", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	db.Model(&model.CPUAffinityPreset{}).Where("id = ?", view.ID).
		Update("deleted_at", "now()")

	if _, err := svc.CreatePreset(ctx, hosttuning.PresetRequest{
		NodeID: 1, Name: "高性能", CPUSet: "4-7",
	}, admin(), "root", ""); err != nil {
		t.Fatalf("软删除之后同名应可重建: %v", err)
	}
}

// TestZRAMBounds 覆盖 ZRAM 参数边界。
func TestZRAMBounds(t *testing.T) {
	_, svc := newEnv(t)
	ctx := context.Background()

	// 下限：比 128MB 更小的压缩区基本不起作用。
	_, err := svc.Apply(ctx, hosttuning.Request{
		NodeID: 1, Item: "zram", DisksizeMB: 32,
	}, admin(), "root", "")
	assertStatus(t, err, 400)

	// 上限：多半是把 GB 填成了 MB。
	_, err = svc.Apply(ctx, hosttuning.Request{
		NodeID: 1, Item: "zram", DisksizeMB: 2_000_000,
	}, admin(), "root", "")
	assertStatus(t, err, 400)
	if err != nil && !strings.Contains(err.Error(), "MB") {
		t.Errorf("报错应指向单位问题: %v", err)
	}

	// 算法白名单。
	_, err = svc.Apply(ctx, hosttuning.Request{
		NodeID: 1, Item: "zram", DisksizeMB: 4096, Algorithm: "bogus",
	}, admin(), "root", "")
	assertStatus(t, err, 400)

	// 合法参数可以过。
	if _, err := svc.Apply(ctx, hosttuning.Request{
		NodeID: 1, Item: "zram", DisksizeMB: 4096, Algorithm: "zstd",
	}, admin(), "root", ""); err != nil {
		t.Fatalf("合法参数应通过: %v", err)
	}
}

func TestUnknownItemRejected(t *testing.T) {
	_, svc := newEnv(t)

	_, err := svc.Apply(context.Background(), hosttuning.Request{
		NodeID: 1, Item: "超频",
	}, admin(), "root", "")
	assertStatus(t, err, 400)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
