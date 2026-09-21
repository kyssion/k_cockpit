package passaudit

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

// Options 是周期运行的参数。
type Options struct {
	Interval time.Duration
	// Key 是登记到调度器注册表时用的标识，供「调度事件」页显示"有哪些东西
	// 在周期性跑"。
	Key string
}

// DefaultOptions 返回默认参数：一天一次。
//
// 口令检查不需要更频繁——它比对的是一份不常变的清单，一天一次足以在"密码
// 上了泄露榜"之后及时提示，而更频繁只会让节点反复做同样的比对。
func DefaultOptions() Options {
	return Options{Interval: 24 * time.Hour, Key: "security.password_audit"}
}

// Loop 是口令检查的周期运行器。
type Loop struct {
	svc  *Service
	opts Options
	obs  *scheduler.Recorder
}

// NewLoop 构造周期运行器。
func NewLoop(svc *Service, opts Options) *Loop {
	if opts.Interval <= 0 {
		opts.Interval = DefaultOptions().Interval
	}
	return &Loop{svc: svc, opts: opts}
}

// Observe 接上调度事件记录器，记录每轮的触发与结果。
func (l *Loop) Observe(r *scheduler.Recorder) { l.obs = r }

// Key 返回调度器标识。
func (l *Loop) Key() string { return l.opts.Key }

// Start 阻塞运行，直到 ctx 取消。
//
// 与配额评估同一个形状：**只在真的做了事的时候记事件**。绝大多数轮次里
// 开关是关的或没有任何命中，为它们各记一条只会让调度事件页变成噪声。
func (l *Loop) Start(ctx context.Context) {
	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()

	l.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.tick(ctx)
		}
	}
}

func (l *Loop) tick(ctx context.Context) {
	res, err := l.svc.RunNow(ctx)
	if err != nil {
		// 开关关闭时的"未开启"不算故障，因此只在真出错时记一条失败事件。
		log.Printf("[passaudit] 口令检查失败: %v", err)
		if l.obs != nil {
			l.obs.Record(ctx, scheduler.Event{
				Key: l.opts.Key, Status: model.SchedulerFailed,
				Message: "口令检查失败：" + err.Error(),
			})
		}
		return
	}
	if res == nil || res.Unavailable != "" {
		if l.obs != nil {
			l.obs.Record(ctx, scheduler.Event{
				Key: l.opts.Key, Status: model.SchedulerFailed,
				Message: firstNonEmpty(res.Unavailable, "本次未执行检查"),
			})
		}
		return
	}
	if l.obs != nil {
		l.obs.Record(ctx, scheduler.Event{
			Key: l.opts.Key, Status: model.SchedulerDone,
			Message: "口令检查完成：检查 " + itoa(res.Checked) + " 个账号，命中 " + itoa(len(res.Hits)) + " 个",
		})
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [12]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
