package quotaenforce

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

// Options 是评估循环的参数。
type Options struct {
	// Interval 是评估间隔。
	//
	// 5 分钟：配额的计量单位是**月**，超限判定不需要秒级精度；而评估要扫
	// 两张按天累计的表，跑得太勤只是白耗数据库。反过来，间隔太长（比如
	// 一小时）会让用户在超限之后仍然正常使用很久——而那段时间的流量是
	// 额外产生的。
	Interval time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{Interval: 5 * time.Minute}
}

// Loop 周期性评估全部节点的配额。
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

// Observe 接入调度事件记录。为 nil 时不做任何记录。
func (l *Loop) Observe(rec *scheduler.Recorder) { l.obs = rec }

// Start 启动循环。
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

// tick 评估一轮。
func (l *Loop) tick(ctx context.Context) {
	var nodeIDs []int64
	if err := l.svc.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Distinct().Pluck("node_id", &nodeIDs).Error; err != nil {
		log.Printf("[quotaenforce] 查询待评估节点失败: %v", err)
		return
	}

	total := 0
	for _, id := range nodeIDs {
		n, err := l.svc.Evaluate(ctx, id)
		if err != nil {
			log.Printf("[quotaenforce] 评估节点 %d 的配额失败: %v", id, err)
			continue
		}
		total += n
	}

	// **有变化才记事件**（与其他周期组件同一约定）：绝大多数轮次里没有
	// 任何配额跨越阈值，那些轮次不该留下东西——否则"配额处置"会每 5 分钟
	// 出现一次，而用户真正要看的是"某天它限速了谁"。
	if total > 0 {
		l.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyQuotaEvaluate, Status: model.SchedulerDone,
			Scope:   itoa(len(nodeIDs)) + " 个节点",
			Message: "评估资源配额，" + itoa(total) + " 条状态发生变化",
		})
	}
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
