// reconcile：网络域看门狗到期对账的周期循环。
//
// 镜像看门狗（portmirror.Reconcile）与上行口确认窗口（bridge.Reconcile）
// 的**执行者**都在节点侧——到期撤销由节点自己完成。控制面这两个 Reconcile
// 只做记录对齐（把「节点已做完的事」反映到库里，让面板不说谎）。
// 它们此前写好了却没有任何调用方（review 抓出）：面板上镜像会永远停在
// 「已启用」，直到有人手动 disable。
package reconcile

import (
	"context"
	"log"
	"time"
)

// Target 是一个可对账的网络域资源类型。
type Target struct {
	Name string
	Run  func(ctx context.Context) (int, error)
}

// Options 是循环参数。
type Options struct {
	// Interval 对账间隔。看门狗最短 60s，一分钟对一次足够跟得上。
	Interval time.Duration
}

func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = time.Minute
	}
	return o
}

// Loop 周期对账全部注册的网络域看门狗。
type Loop struct {
	targets []Target
	opts    Options
}

// NewLoop 构造循环；targets 为空时 Start 直接返回（防御装配遗漏，但那种
// 情况本身就该在装配层修）。
func NewLoop(targets []Target, opts Options) *Loop {
	return &Loop{targets: targets, opts: opts.withDefaults()}
}

// Start 阻塞运行直到 ctx 取消。单轮失败只记日志——对账是自愈类动作，
// 一次失败（比如数据库重启）下一轮再试即可。
func (l *Loop) Start(ctx context.Context) {
	ticker := time.NewTicker(l.opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.RunOnce(ctx)
		}
	}
}

// RunOnce 执行一轮对账（导出供测试驱动）。
func (l *Loop) RunOnce(ctx context.Context) {
	for _, t := range l.targets {
		n, err := t.Run(ctx)
		if err != nil {
			log.Printf("[netreconcile] %s 对账失败: %v", t.Name, err)
			continue
		}
		if n > 0 {
			log.Printf("[netreconcile] %s 对齐 %d 条到期记录", t.Name, n)
		}
	}
}
