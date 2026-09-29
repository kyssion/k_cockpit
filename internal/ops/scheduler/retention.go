package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/model"
)

// RetentionOptions 是事件保留清理的参数。
type RetentionOptions struct {
	// Interval 是清理检查的间隔。事件量级远小于任务表，一小时查一次足够。
	Interval time.Duration
	// KeepHours 返回当前生效的保留小时数；每次清理前现读，改设置立即生效。
	// 返回 <=0 时按默认保留期处理（而不是"全部保留"——那会让关闭设置
	// 的唯一途径变成"等表大到查询变慢"）。
	KeepHours func() int
}

// defaultKeepHours 与对方面板的默认保留期一致：一周足够回看"上周那次
// 失败是什么原因"，再久的事件对排障的价值已经很低。
const defaultKeepHours = 168

// RetentionLoop 周期清理过期的调度事件（G-48）。
//
// 事件表只在调度器**实际做了事**时才有行，量级通常很小；但"通常很小"
// 不是"永远很小"——一个反复失败的调度器每轮都会留一条失败事件，几个月
// 也足以把表堆厚。保留期让这件事有一个确定的边界。
type RetentionLoop struct {
	db   *gorm.DB
	rec  *Recorder
	opts RetentionOptions
	once sync.Once
	stop chan struct{}
	done chan struct{}
}

// NewRetentionLoop 构造清理循环。
func NewRetentionLoop(db *gorm.DB, rec *Recorder, opts RetentionOptions) *RetentionLoop {
	if opts.Interval <= 0 {
		opts.Interval = time.Hour
	}
	if opts.KeepHours == nil {
		opts.KeepHours = func() int { return defaultKeepHours }
	}
	return &RetentionLoop{db: db, rec: rec, opts: opts, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动清理循环。
func (l *RetentionLoop) Start(ctx context.Context) {
	go l.loop(ctx)
}

// Stop 停止并等待循环退出。
func (l *RetentionLoop) Stop() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

func (l *RetentionLoop) loop(ctx context.Context) {
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
			// 单次失败只记日志继续跑：清理是自愈类动作，一次失败（比如
			// 恰逢数据库重启）不该把整个循环停掉——下次到点再试即可。
			if _, err := l.RunOnce(ctx); err != nil {
				log.Printf("[scheduler] 清理过期事件失败: %v", err)
			}
		}
	}
}

// RunOnce 执行一次清理，返回删除的条数（导出供测试驱动）。
func (l *RetentionLoop) RunOnce(ctx context.Context) (int64, error) {
	keep := l.opts.KeepHours()
	if keep <= 0 {
		keep = defaultKeepHours
	}
	before := time.Now().Add(-time.Duration(keep) * time.Hour)

	res := l.db.WithContext(ctx).
		Where("at < ?", before).
		Delete(&model.SchedulerEvent{})
	if res.Error != nil {
		return 0, res.Error
	}
	// 只有真的删了才记事件：Record 的约定是"只记录实际发生的动作"，
	// 每小时一条"没什么可清理"会把真正有信息的那几条埋掉。
	if res.RowsAffected > 0 {
		l.rec.Record(ctx, Event{
			Key:    KeySchedulerRetention,
			Status: model.SchedulerDone,
			Scope:  "调度事件",
			Message: "清理了 " + itoa(res.RowsAffected) + " 条超过保留期（" +
				itoa(int64(keep)) + " 小时）的调度事件",
		})
	}
	return res.RowsAffected, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
