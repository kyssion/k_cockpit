package alert

import (
	"context"
	"log"
	"strconv"
	"time"

	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

// Options 是评估循环的参数。
type Options struct {
	// Interval 是评估间隔。
	//
	// 5 分钟：告警的价值在于"及时"，而它依赖的数据（节点心跳、采样）本身
	// 也是分钟级更新的——比这更频繁只会得到同样的结论。
	Interval time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options { return Options{Interval: 5 * time.Minute} }

// Loop 周期性评估告警。
type Loop struct {
	svc  *Service
	opts Options
	obs  *scheduler.Recorder

	stop chan struct{}
	done chan struct{}
}

// NewLoop 构造评估循环。
func NewLoop(svc *Service, opts Options) *Loop {
	if opts.Interval <= 0 {
		opts.Interval = DefaultOptions().Interval
	}
	return &Loop{svc: svc, opts: opts, stop: make(chan struct{}), done: make(chan struct{})}
}

// Observe 接入调度事件记录；为 nil 时不做任何记录。
func (l *Loop) Observe(rec *scheduler.Recorder) { l.obs = rec }

// Start 启动循环。先跑一轮：服务重启之后不必等五分钟才知道当前有没有告警。
func (l *Loop) Start(ctx context.Context) {
	go func() {
		defer close(l.done)
		l.tick(ctx)

		ticker := time.NewTicker(l.opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.tick(ctx)
			}
		}
	}()
}

// Stop 停止循环。
func (l *Loop) Stop() {
	close(l.stop)
	<-l.done
}

func (l *Loop) tick(ctx context.Context) {
	changed, err := l.svc.Evaluate(ctx)
	if err != nil {
		log.Printf("[alert] 评估告警失败: %v", err)
		return
	}
	// **有变化才记事件**：绝大多数轮次什么都没变，那些轮次不该留下东西——
	// 否则"告警评估"会每五分钟出现在事件列表里，而用户真正要看的是
	// "某天它发现了什么"。
	if changed > 0 && l.obs != nil {
		l.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyAlertEvaluate, Status: model.SchedulerDone,
			Scope:   strconv.Itoa(changed) + " 条",
			Message: "评估告警，新增或关闭了这些条目",
		})
	}
}
