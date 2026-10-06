package vm

import (
	"context"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/scheduler"
)

// MediaEjectOptions 是安装介质自动弹出的参数。
type MediaEjectOptions struct {
	// Interval 是扫描间隔。cloudbase-init 的耗时以分钟计，一分钟扫一次
	// 足够及时也足够便宜。
	Interval time.Duration
}

// MediaEjectLoop 周期检查"欠一次自动弹出"的虚拟机，初始化就绪后弹出
// 安装介质（F-2-17）。
//
// 为什么是一个循环而不是创建时的一次性任务：就绪时刻取决于用户什么时候
// 开机、cloudbase-init 跑多久——控制面在创建时既不知道也不该猜。把
// "还欠着"记在 vm 表上、由循环收敛，天然覆盖"用户三天后才第一次开机"。
type MediaEjectLoop struct {
	svc   *Service
	db    *gorm.DB
	agent agent.Client
	rec   *scheduler.Recorder
	opts  MediaEjectOptions
	once  sync.Once
	stop  chan struct{}
	done  chan struct{}
}

// NewMediaEjectLoop 构造弹出循环。
func NewMediaEjectLoop(svc *Service, db *gorm.DB, client agent.Client, rec *scheduler.Recorder, opts MediaEjectOptions) *MediaEjectLoop {
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	return &MediaEjectLoop{svc: svc, db: db, agent: client, rec: rec, opts: opts,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动循环。
func (l *MediaEjectLoop) Start(ctx context.Context) {
	go l.loop(ctx)
}

// Stop 停止并等待退出。
func (l *MediaEjectLoop) Stop() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

func (l *MediaEjectLoop) loop(ctx context.Context) {
	defer close(l.done)

	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-l.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := l.RunOnce(ctx); err != nil {
				log.Printf("[vm] 安装介质弹出扫描失败: %v", err)
			}
		}
	}
}

// RunOnce 扫描并处理一轮（导出供测试驱动），返回本轮发起弹出的台数。
func (l *MediaEjectLoop) RunOnce(ctx context.Context) (int, error) {
	var vms []model.VM
	// 只看运行中的：没开机的机器 cloudbase-init 不会跑，探测必然"未就绪"。
	if err := l.db.WithContext(ctx).
		Where("media_auto_eject = ? AND status = ? AND present = ?",
			true, model.VMStatusRunning, true).
		Limit(100).Find(&vms).Error; err != nil {
		return 0, err
	}

	ejected := 0
	for i := range vms {
		vm := &vms[i]

		result, err := l.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpVMInitReady,
			NodeID: vm.NodeID,
			Target: vm.Name,
		})
		if err != nil || !result.Success {
			// 节点不可达或探测失败：留着标记下一轮再试。这不是错误——
			// "还没就绪"本来就是常态，多数轮次都该走到这里。
			continue
		}
		info, ok := result.Data[agent.InitReadyDataKey].(agent.InitReadyInfo)
		if !ok || !info.Ready {
			continue
		}

		// 先清标记再入队：入队失败的话下一轮扫描会把它再捞起来重试
		//（弹出是幂等的——空光驱再弹一次只是无害的空转）；反过来"入队
		// 成功但清标志失败"会每分钟重复弹一次，那才是要避免的。
		if err := l.db.WithContext(ctx).Model(&model.VM{}).
			Where("id = ?", vm.ID).Update("media_auto_eject", false).Error; err != nil {
			log.Printf("[vm] 清除自动弹出标记失败 vm=%d: %v", vm.ID, err)
			continue
		}

		if n := l.svc.ejectAllInserted(ctx, vm); n > 0 {
			ejected++
			if l.rec != nil {
				l.rec.Record(ctx, scheduler.Event{
					Key:    scheduler.KeyVMMediaEject,
					Status: model.SchedulerDone,
					Scope:  vm.Name,
					Message: "初始化已完成，自动弹出安装介质（" +
						itoaMedia(n) + " 个光驱）",
				})
			}
			log.Printf("[vm] 已自动弹出安装介质 vm=%s(%d)", vm.Name, vm.ID)
		}
	}
	return ejected, nil
}

// ejectAllInserted 弹出一台虚拟机全部**已插入**的光驱介质（光驱保留）。
//
// vm_cdrom 表里登记的是用户挂的安装 ISO；ConfigDrive 介质由节点侧生成、
// 不进这张表——因此"弹出表里全部已插入的"正好就是安装介质，不会误弹
// 初始化盘。
func (s *Service) ejectAllInserted(ctx context.Context, vm *model.VM) int {
	var rows []model.VMCDROM
	if err := s.db.WithContext(ctx).
		Where("vm_id = ? AND iso_file_id IS NOT NULL", vm.ID).
		Find(&rows).Error; err != nil {
		log.Printf("[vm] 查询已插入介质失败 vm=%d: %v", vm.ID, err)
		return 0
	}

	n := 0
	for i := range rows {
		// 控制面记录与下发走既有弹出路径（enqueueCDROM）：这就是一次
		// 普通的弹出，只是触发者是从初始化循环，不该有第二条代码路径。
		if err := s.db.WithContext(ctx).Model(&model.VMCDROM{}).
			Where("id = ?", rows[i].ID).Update("iso_file_id", nil).Error; err != nil {
			log.Printf("[vm] 清空光驱记录失败 vm=%d cdrom=%d: %v", vm.ID, rows[i].ID, err)
			continue
		}
		if _, err := s.enqueueCDROM(ctx, vm, rows[i], "eject", authz.Viewer{IsAdmin: true}); err != nil {
			log.Printf("[vm] 弹出入队失败 vm=%d cdrom=%d: %v", vm.ID, rows[i].ID, err)
			continue
		}
		n++
	}
	return n
}

func itoaMedia(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
