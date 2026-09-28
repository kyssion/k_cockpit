package storage

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/scheduler"
)

// TrimOptions 是自动 trim 循环的参数。
type TrimOptions struct {
	// Interval 是检查间隔。trim 的粒度是"天"（discard 的收益不会在几分钟
	// 内积累出来），一天检查一次足够。
	Interval time.Duration
	// Enabled 返回自动回收开关；每次执行前现读，改设置立即生效。
	Enabled func() bool
}

// TrimLoop 周期对全部在线节点执行 trim（G-52）。
//
// 手动 trim 一直都在（存储池页的按钮），缺的是"不用记得去点"的那一路：
// 删盘之后的可回收块会随时间积累，自动回收让这件事有一个确定的节奏。
// 执行结果落在调度事件里——那本来就是"周期动作做过什么"的归属地。
type TrimLoop struct {
	db    *gorm.DB
	agent agent.Client
	rec   *scheduler.Recorder
	opts  TrimOptions
	once  sync.Once
	stop  chan struct{}
	done  chan struct{}
}

// NewTrimLoop 构造自动 trim 循环。
func NewTrimLoop(db *gorm.DB, client agent.Client, rec *scheduler.Recorder, opts TrimOptions) *TrimLoop {
	if opts.Interval <= 0 {
		opts.Interval = 24 * time.Hour
	}
	return &TrimLoop{db: db, agent: client, rec: rec, opts: opts,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Start 启动循环。
func (l *TrimLoop) Start(ctx context.Context) {
	go l.loop(ctx)
}

// Stop 停止并等待退出。
func (l *TrimLoop) Stop() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

func (l *TrimLoop) loop(ctx context.Context) {
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
			// 单轮失败只记事件继续跑：trim 是自愈类动作，失败（比如数据库
			// 恰好重启）等下一轮即可，停掉循环反而让"自动回收"悄悄失效。
			if err := l.RunOnce(ctx); err != nil {
				log.Printf("[storage] 自动 trim 失败: %v", err)
			}
		}
	}
}

// RunOnce 对全部在线节点执行一轮 trim（导出供测试驱动）。
//
// 逐节点执行、单节点失败不阻断其余：一个节点离线不该让其它节点的回收
// 也停摆。结果聚成**一条**调度事件——按节点拆开会一天产生 N 条，而它们
// 都是"同一轮回收"这一个动作。
func (l *TrimLoop) RunOnce(ctx context.Context) error {
	if l.opts.Enabled != nil && !l.opts.Enabled() {
		return nil
	}

	var nodes []model.Node
	if err := l.db.WithContext(ctx).
		Where("status = ? AND maintenance_mode = ?", model.NodeStatusOnline, false).
		Find(&nodes).Error; err != nil {
		return err
	}

	var (
		okCount, failCount, devices int
		reclaimed                   int64
		failures                    []string
	)
	for i := range nodes {
		result, err := l.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpStorageTrim,
			NodeID: nodes[i].ID,
			Target: "storage",
		})
		if err != nil || !result.Success {
			failCount++
			reason := "节点不可达"
			if result != nil && result.Message != "" {
				reason = result.Message
			}
			failures = append(failures, nodes[i].Name+"（"+reason+"）")
			continue
		}
		info := decodeTrim(result.Data)
		okCount++
		devices += info.Devices
		reclaimed += info.ReclaimedBytes
	}

	if l.rec != nil {
		status, message := model.SchedulerDone, ""
		var b strings.Builder
		b.WriteString("自动回收完成：")
		if failCount == 0 {
			message = "全部节点 trim 成功"
		} else {
			status = model.SchedulerFailed
			message = "部分节点 trim 失败：" + strings.Join(failures, "、")
		}
		l.rec.Record(ctx, scheduler.Event{
			Key:    scheduler.KeyStorageTrim,
			Status: status,
			Scope:  "全部节点",
			Message: message + "（成功 " + itoa(int64(okCount)) + " 台 / 共 " +
				itoa(int64(len(nodes))) + " 台，设备 " + itoa(int64(devices)) + " 个，回收 " +
				itoa(reclaimed) + " 字节）",
		})
	}
	return nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
