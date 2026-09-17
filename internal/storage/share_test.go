package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
)

type shareEnv struct {
	svc *storage.Service
	db  *gorm.DB
	vm  *model.VM
}

func newShareEnv(t *testing.T, root string) *shareEnv {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "share.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.ShareMount{}, &model.Node{}, &model.VM{}, &model.UserStorage{},
		&model.StoragePool{}, &model.Task{}, &model.TaskStage{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	owner := int64(7)
	vm := model.VM{NodeID: 1, Name: "vm-share", Status: model.VMStatusStopped,
		OwnerID: &owner, Present: true}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	// 只有配了存储根的用户才能共享目录。
	us := model.UserStorage{UserID: owner, NodeID: 1, Enabled: true, RootPath: &root}
	if err := db.Create(&us).Error; err != nil {
		t.Fatalf("创建用户存储失败: %v", err)
	}

	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	queue.Register(storage.NewShareExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return &shareEnv{
		svc: storage.NewService(db, queue, recorder, client, nil),
		db:  db, vm: &vm,
	}
}

func ownerViewer() authz.Viewer { return authz.Viewer{UserID: 7} }

// TestMountRejectsAbsolutePath 覆盖整块功能的**安全前提**。
//
// 共享是一个跨越虚拟化边界的读取入口，而参数来自用户。允许绝对路径意味着
// 一个租户可以把 /etc 挂进自己的虚拟机读出来——而这不会触发任何权限检查，
// 因为 qemu 是以一个有权读它的用户在跑。
//
// 本项目从**表示形式上**堵死这条路：只接受相对路径，用户根本没有机会给出
// /etc。事后比对绝对路径前缀要处理 .. 穿越、符号链接、大小写、尾斜杠等等，
// 任何一个漏掉都是一个可以直接读宿主机文件系统的漏洞。
func TestMountRejectsAbsolutePath(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	// 纯绝对路径：文案要明确指向「该填相对路径」。
	for _, bad := range []string{
		"/etc",
		"/etc/passwd",
		"/srv/users/7/data", // 即使指向自己的根，也要求用相对路径
		"C:/windows",
	} {
		_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
			VMID: env.vm.ID, RelPath: bad,
		}, ownerViewer(), "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("绝对路径 %q 应被拒绝", bad)
			continue
		}
		var apiErr *api.Error
		if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "相对") {
			t.Errorf("拒绝文案应说明该填相对路径: %q", apiErr.Message)
		}
	}

	// 含反斜杠的写法走另一条检查（反斜杠在部分平台被当作分隔符），
	// 拒绝理由不同但同样是拒绝——这里只要求它被拦下。
	if _, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: `c:\\windows`,
	}, ownerViewer(), "alice", ""); err == nil {
		t.Error("含反斜杠的路径应被拒绝")
	}
}

// TestMountRejectsTraversal 覆盖 .. 穿越。
//
// 相对路径本身不构成保证：`a/../../etc` 清理之后仍在存储根之外。
func TestMountRejectsTraversal(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	for _, bad := range []string{
		"..",
		"../etc",
		"a/../../etc",
		"a/../../..",
		"./../outside",
	} {
		_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
			VMID: env.vm.ID, RelPath: bad,
		}, ownerViewer(), "alice", "10.0.0.1")
		assertStatus(t, err, 400)
	}
}

func TestMountAcceptsRelativePath(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	// 越界的要拒绝，正常的要能过——只测拒绝的用例拦不住一个「全都拒绝」
	// 的实现，而那种实现是没用的。
	for _, good := range []string{"data", "data/iso", "./data", "a/b/c"} {
		task, err := env.svc.MountShare(ctx, storage.MountShareRequest{
			VMID: env.vm.ID, RelPath: good,
		}, ownerViewer(), "alice", "10.0.0.1")
		if err != nil {
			t.Errorf("相对路径 %q 应被接受: %v", good, err)
			continue
		}
		waitForShareActive(t, env, task.ID)

		items, err := env.svc.ListShares(ctx, env.vm.ID, ownerViewer())
		if err != nil {
			t.Fatalf("查询共享失败: %v", err)
		}
		if len(items) != 1 {
			t.Fatalf("共享数 = %d, 期望 1", len(items))
		}
		if !strings.HasPrefix(items[0].HostPath, "/srv/users/7/") {
			t.Errorf("拼出的绝对路径应在存储根之下: %q", items[0].HostPath)
		}
		// tag 默认取**目录名**（最后一段）：用户在来宾里
		// `mount -t 9p <tag>` 时一眼能对上是哪个共享。
		wantTag := good
		if i := strings.LastIndex(good, "/"); i >= 0 {
			wantTag = good[i+1:]
		}
		if items[0].Tag != wantTag {
			t.Errorf("路径 %q 的默认 tag = %q, 期望 %q", good, items[0].Tag, wantTag)
		}
		// 清理，避免影响下一轮。
		if _, err := env.svc.UnmountShare(ctx, env.vm.ID, items[0].Tag,
			ownerViewer(), "alice", ""); err != nil {
			t.Fatalf("卸载失败: %v", err)
		}
		waitForShareCount(t, env, 0)
	}
}

// TestReadOnlyDefaultsTrue 覆盖一个安全的默认值。
//
// 可写共享意味着来宾可以往宿主机写文件，而**那些写入不受控制面的配额约束**
// ——配额是控制面受理请求时算的，而来宾绕过控制面直接写盘。默认只读让
// 「不小心把宿主机写满」不可能发生。
func TestReadOnlyDefaultsTrue(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	task, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data",
	}, ownerViewer(), "alice", "")
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	waitForShareActive(t, env, task.ID)

	items, _ := env.svc.ListShares(ctx, env.vm.ID, ownerViewer())
	if len(items) != 1 {
		t.Fatalf("共享数 = %d, 期望 1", len(items))
	}
	if !items[0].ReadOnly {
		t.Error("默认应为只读——可写共享的写入不受配额约束")
	}
}

// TestPassthroughRequiresAdmin 覆盖一道权限边界。
//
// passthrough 会让来宾里的 root 直接在宿主机上写出属于 root 的文件，共享
// 目录与宿主机之间因此不再有权限边界。这是用户能主动选择的最危险的一档。
func TestPassthroughRequiresAdmin(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data", SecurityModel: model.ShareSecurityPassthrough,
	}, ownerViewer(), "alice", "")
	assertStatus(t, err, 422)

	// 管理员可以。
	adminViewer := authz.Viewer{UserID: 1, IsAdmin: true}
	if _, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data", SecurityModel: model.ShareSecurityPassthrough,
	}, adminViewer, "root", ""); err != nil {
		t.Errorf("管理员应可使用 passthrough: %v", err)
	}
}

// TestTagUniqueness 覆盖 tag 在虚拟机内唯一。
//
// 两个共享用同一个 tag 时，来宾里只能挂上其中一个，而另一个会以一句
// 「设备忙」或更含糊的错误失败——用户很难把那个错误与他刚做的操作联系起来。
func TestTagUniqueness(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	if _, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data", Tag: "shared",
	}, ownerViewer(), "alice", ""); err != nil {
		t.Fatalf("首次挂载失败: %v", err)
	}

	_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "other", Tag: "shared",
	}, ownerViewer(), "alice", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "shared") {
		t.Errorf("拒绝文案应带出冲突的 tag: %v", err)
	}
}

// TestTagCharsetRestricted 覆盖 tag 的字符集。
//
// tag 会写进 qemu 的 `-virtfs ...,mount_tag=<tag>` 参数，也会出现在来宾的
// 挂载命令里。允许空白、引号、逗号这类字符等于把两台机器上的命令解析器
// 都交给用户——拆错的结果是把后半段当成另一个参数。
func TestTagCharsetRestricted(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	for _, bad := range []string{
		"a b", "a,b", "a;b", "a'b", `a"b`, "-lead", "_lead", "a/b",
		strings.Repeat("x", 65),
	} {
		_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
			VMID: env.vm.ID, RelPath: "data", Tag: bad,
		}, ownerViewer(), "alice", "")
		if err == nil {
			t.Errorf("tag %q 应被拒绝", bad)
		}
	}
}

// TestMountRequiresStorageRoot 覆盖存储根缺失的情况。
//
// 存储根为空时**不能退化成「拼在 / 下」**：那会让相对路径校验失去意义，
// 用户填 "etc" 就指向了 /etc。
func TestMountRequiresStorageRoot(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	// 把一个没有用户存储记录的人挂到该虚拟机上。
	other := int64(99)
	if err := env.db.Model(&model.VM{}).Where("id = ?", env.vm.ID).
		Update("owner_id", other).Error; err != nil {
		t.Fatalf("改属主失败: %v", err)
	}

	_, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data",
	}, authz.Viewer{UserID: other}, "bob", "")
	assertStatus(t, err, 422)
}

// TestMountRejectsOtherOwnersVM 覆盖越权。
func TestMountRejectsOtherOwnersVM(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")

	_, err := env.svc.MountShare(context.Background(), storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data",
	}, authz.Viewer{UserID: 10}, "mallory", "")
	// 404 而非 403：403 会确认「这个 ID 存在」。
	assertStatus(t, err, 404)
}

func TestUnmountRemovesRecord(t *testing.T) {
	env := newShareEnv(t, "/srv/users/7")
	ctx := context.Background()

	task, err := env.svc.MountShare(ctx, storage.MountShareRequest{
		VMID: env.vm.ID, RelPath: "data", Tag: "docs",
	}, ownerViewer(), "alice", "")
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	waitForShareActive(t, env, task.ID)

	if _, err := env.svc.UnmountShare(ctx, env.vm.ID, "docs", ownerViewer(), "alice", ""); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	// 记录的存活期就是这个共享的存活期；卸载后不应留下一条「已卸载」的
	// 记录让人分不清哪条是当前有效的。
	waitForShareCount(t, env, 0)
}

// --- 辅助 ---

func waitForShareActive(t *testing.T, env *shareEnv, taskID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var row model.ShareMount
		if err := env.db.Where("mounted_at IS NOT NULL").First(&row).Error; err == nil {
			return
		}
		// 失败要**及时**报出来，而不是等到超时——超时信息对排障没有帮助。
		// 失败时记录会被删掉，因此这里看的是任务。
		var tk model.Task
		if err := env.db.Where("id = ?", taskID).First(&tk).Error; err == nil {
			if tk.Status == model.TaskFailed {
				msg := ""
				if tk.Error != nil {
					msg = *tk.Error
				}
				t.Fatalf("共享下发失败: %s", msg)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("等待共享生效超时")
}

func waitForShareCount(t *testing.T, env *shareEnv, want int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		env.db.Model(&model.ShareMount{}).Count(&n)
		if n == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待共享记录变为 %d 条超时", want)
}

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
