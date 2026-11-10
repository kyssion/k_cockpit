package pool

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/service/ops/scheduler"
)

// CapacityOptions 是池容量刷新循环的参数。
type CapacityOptions struct {
	// Interval 是刷新间隔。
	//
	// 池列表的空间数据标注了新鲜度（陈旧阈值默认 5 分钟，R-009），刷新
	// 间隔取它的一半：一份数据在过期之前至少被刷新过一次，而不是在
	// 「陈旧」与「新鲜」之间随相位差随机抖动。
	Interval time.Duration
}

// DefaultCapacityOptions 返回默认参数。
func DefaultCapacityOptions() CapacityOptions {
	return CapacityOptions{Interval: StaleThreshold / 2}
}

// CapacityRefreshLoop 周期刷新存储池容量（f-5-01 R-009 的落地）。
//
// 模型注释里写的「容量由 agent 随指标周期上报」此前没有任何实现——池的
// total/usable_bytes 恒为 0、last_reported_at 停在建池那一刻，列表页永远
// 显示陈旧标记（实机测试抓出）。本循环补上这条路：按节点把该节点全部
// ready 池的挂载点**批量**发给 agent（一次往返），statfs 结果写回池记录。
type CapacityRefreshLoop struct {
	db    *gorm.DB
	agent agent.Client
	rec   *scheduler.Recorder
	opts  CapacityOptions
	once  sync.Once
	stop  chan struct{}
	done  chan struct{}
}

// NewCapacityRefreshLoop 构造容量刷新循环。
func NewCapacityRefreshLoop(db *gorm.DB, client agent.Client, rec *scheduler.Recorder, opts CapacityOptions) *CapacityRefreshLoop {
	if opts.Interval <= 0 {
		opts.Interval = DefaultCapacityOptions().Interval
	}
	return &CapacityRefreshLoop{db: db, agent: client, rec: rec, opts: opts,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Observe 接入调度事件记录；为 nil 时不做任何记录。
func (l *CapacityRefreshLoop) Observe(rec *scheduler.Recorder) { l.rec = rec }

// Start 启动循环。先跑一轮：服务重启之后不必等一个周期池容量才恢复正常。
func (l *CapacityRefreshLoop) Start(ctx context.Context) {
	go func() {
		defer close(l.done)
		l.RunOnce(ctx)

		ticker := time.NewTicker(l.opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 单轮失败只记日志继续跑：容量数据下一轮就会重刷，
				// 停掉循环反而让「池容量上报」悄悄失效。
				if err := l.RunOnce(ctx); err != nil {
					log.Printf("[storage] 池容量刷新失败: %v", err)
				}
			}
		}
	}()
}

// Stop 停止并等待退出。
func (l *CapacityRefreshLoop) Stop() {
	l.once.Do(func() { close(l.stop) })
	<-l.done
}

// RunOnce 执行一轮刷新（导出供测试驱动）。
//
// 只碰 ready / unmounted 且未删除的池；离线节点一次失败的往返就跳过整
// 节点的池，下一轮再试。失败才记调度事件——正常轮次没有可说的，每三分钟
// 一条「刷新成功」只会把有价值的事件埋掉。
//
// unmounted 也要继续轮询：它是「设备暂时不在」的降级态，挂载恢复后要能
// 自动回到 ready；只查 ready 会让池一旦降级就永远出不来。
func (l *CapacityRefreshLoop) RunOnce(ctx context.Context) error {
	var pools []model.StoragePool
	if err := l.db.WithContext(ctx).
		Where("status IN ? AND deleted_at IS NULL",
			[]string{model.StoragePoolReady, model.StoragePoolUnmounted}).
		Find(&pools).Error; err != nil {
		return err
	}
	if len(pools) == 0 {
		return nil
	}

	byNode := make(map[int64][]model.StoragePool)
	for i := range pools {
		if derefStr(pools[i].MountPath) == "" {
			continue // 没有挂载点就没有可探测的东西
		}
		byNode[pools[i].NodeID] = append(byNode[pools[i].NodeID], pools[i])
	}

	for nodeID, list := range byNode {
		if err := refreshNodePools(ctx, l.db, l.agent, nodeID, list); err != nil {
			log.Printf("[storage] 节点 %d 池容量刷新失败: %s", nodeID, err)
			l.recordFailure(ctx, nodeID, err.Error())
			continue
		}

		// 同一轮里把「我的存储」根收敛到默认池（幂等）。放在这里而不是
		// 只在建池时做，是因为默认池可能被改、被卸载、被删——收敛类配置
		// 由一个周期循环兜底，比在每个入口各写一次更不容易漏。
		if err := SyncUserdataRoot(ctx, l.db, l.agent, nodeID); err != nil {
			log.Printf("[storage] 节点 %d 用户存储根同步失败: %v", nodeID, err)
		}
	}
	return nil
}

// RefreshNodeCapacity 立即刷新一个节点上全部就绪池的容量。
//
// 建池/改池任务结束时调用：否则从「池建好」到下一轮定时刷新之间（默认
// 2.5 分钟）列表里的容量是 0，用户会以为池没建成功（实机测试抓出）。
// 失败只作为错误返回，由调用方决定是否记日志——刷新失败不该让一个已经
// 成功的建池任务变成失败。
func RefreshNodeCapacity(ctx context.Context, db *gorm.DB, client agent.Client, nodeID int64) error {
	var pools []model.StoragePool
	if err := db.WithContext(ctx).
		Where("node_id = ? AND status = ? AND deleted_at IS NULL", nodeID, model.StoragePoolReady).
		Find(&pools).Error; err != nil {
		return err
	}
	ready := make([]model.StoragePool, 0, len(pools))
	for i := range pools {
		if derefStr(pools[i].MountPath) != "" {
			ready = append(ready, pools[i])
		}
	}
	if len(ready) == 0 {
		return nil
	}
	return refreshNodePools(ctx, db, client, nodeID, ready)
}

// refreshNodePools 批量探测一个节点上若干池的容量并写回。
//
// 一次往返取回该节点全部挂载点的 statfs 结果，而不是每个池一往返：
// 池数量随磁盘数增长，逐个探测会让一轮刷新变成 N 次 RPC。
func refreshNodePools(
	ctx context.Context, db *gorm.DB, client agent.Client, nodeID int64, list []model.StoragePool,
) error {
	paths := make([]string, 0, len(list))
	for i := range list {
		paths = append(paths, derefStr(list[i].MountPath))
	}
	result, err := client.Execute(ctx, agent.Operation{
		Kind:   agent.OpStoragePoolStat,
		NodeID: nodeID,
		Target: "storage",
		Params: map[string]any{"paths": paths},
	})
	if err != nil {
		return err
	}
	if !result.Success {
		if result.Message != "" {
			return errors.New(result.Message)
		}
		return errors.New("节点未返回容量数据")
	}

	stats, _ := result.Data[agent.PoolStatListKey].([]agent.PoolStat)
	if len(stats) == 0 {
		// 节点一条都没上报：可能是旧版 agent，也可能是这一轮探测整体失败。
		// 既不能据此刷新时间戳（那会说谎），也不能据此断定池未挂载
		// （那会把每个旧 agent 节点的池全部降级）。保持原状，下轮再试。
		return nil
	}
	byPath := make(map[string]agent.PoolStat, len(stats))
	for _, st := range stats {
		byPath[st.Path] = st
	}

	now := time.Now()
	for i := range list {
		mountPath := derefStr(list[i].MountPath)
		st, found := byPath[mountPath]
		// stale 不是存储列——toPoolView 按 last_reported_at 现算。
		// 只有节点确认「这是真挂载点」时才刷新时间戳与容量：否则宁可让
		// 数据自然变陈旧，也不能把根分区的容量刷成「刚刚上报的池容量」。
		updates := map[string]any{}
		switch {
		case found && st.Mounted:
			updates["last_reported_at"] = now
			updates["total_bytes"] = st.TotalBytes
			updates["usable_bytes"] = st.FreeBytes
			if list[i].Status == model.StoragePoolUnmounted {
				updates["status"] = model.StoragePoolReady
			}
		case list[i].Status == model.StoragePoolReady:
			// 节点说这个挂载点不在挂载表里（或压根没返回）：设备多半在
			// 重启后丢了。降级为 unmounted，界面给出「需要先挂载」。
			updates["status"] = model.StoragePoolUnmounted
			log.Printf("[storage] 池 %d 挂载点 %s 已不是独立挂载，状态降级为 unmounted",
				list[i].ID, mountPath)
		default:
			continue
		}
		if err := db.WithContext(ctx).Model(&model.StoragePool{}).
			Where("id = ?", list[i].ID).Updates(updates).Error; err != nil {
			log.Printf("[storage] 写回池 %d 容量失败: %v", list[i].ID, err)
		}
	}
	return nil
}

// recordFailure 把一轮里的节点级失败记成一条调度事件（存在失败才记）。
func (l *CapacityRefreshLoop) recordFailure(ctx context.Context, nodeID int64, reason string) {
	if l.rec == nil {
		return
	}
	var name string
	if err := l.db.WithContext(ctx).Model(&model.Node{}).
		Select("name").Where("id = ?", nodeID).Scan(&name).Error; err != nil || name == "" {
		name = "节点 " + itoa(nodeID)
	}
	l.rec.Record(ctx, scheduler.Event{
		Key:    scheduler.KeyStorageCapacityRefresh,
		Status: model.SchedulerFailed,
		Scope:  name,
		Message: "池容量刷新失败：" + reason +
			"（列表页的空间数据将持续显示为陈旧，节点恢复后自动重试）",
	})
}
