package authkey

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

// Options 是自动轮换循环的参数。
type Options struct {
	Interval time.Duration
	Key      string
}

// DefaultOptions 一天检查一次是否需要轮换。
func DefaultOptions() Options {
	return Options{Interval: 24 * time.Hour, Key: "security.auth_key_rotate"}
}

// Loop 是自动轮换的周期运行器。
type Loop struct {
	svc *Service
	// maxAgeDays 读取设置项 security.auth_key_rotate_days；0 表示不自动轮换。
	maxAgeDays func() int
	opts       Options
	obs        *scheduler.Recorder
}

// NewLoop 构造循环。
func NewLoop(svc *Service, maxAgeDays func() int, opts Options) *Loop {
	if opts.Interval <= 0 {
		opts.Interval = DefaultOptions().Interval
	}
	return &Loop{svc: svc, maxAgeDays: maxAgeDays, opts: opts}
}

// Observe 接上调度事件记录器。
func (l *Loop) Observe(r *scheduler.Recorder) { l.obs = r }

// Start 阻塞运行直到 ctx 取消。
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
	days := 0
	if l.maxAgeDays != nil {
		days = l.maxAgeDays()
	}
	rotated, err := l.svc.RotateIfDue(ctx, days)
	if err != nil {
		log.Printf("[authkey] 自动轮换失败: %v", err)
		if l.obs != nil {
			l.obs.Record(ctx, scheduler.Event{
				Key: l.opts.Key, Status: model.SchedulerFailed,
				Message: "会话密钥自动轮换失败：" + err.Error(),
			})
		}
		return
	}
	// 只在**真的换了**的时候记事件：绝大多数轮次里要么没开自动轮换、要么
	// 还没到期，为它们各记一条只会把调度事件页变成噪声。
	if rotated && l.obs != nil {
		l.obs.Record(ctx, scheduler.Event{
			Key: l.opts.Key, Status: model.SchedulerDone,
			Message: "会话密钥已自动轮换（超过 " + itoa(days) + " 天），全部旧会话令牌失效",
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [8]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
