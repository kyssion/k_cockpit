// Package monitor 实现指标采集与历史查询（F-8-01 / F-8-02 / F-8-05 / F-8-06）。
//
// 本包存在的理由是一句话：
//
//	**指标必须由独立采集器按固定间隔落库，而不是「用户看页面时顺便采一次」。**
//
// 把两者混起来是很自然的一步：页面已经在轮询实时指标了（详情页的 Hero 卡），
// 顺手写一条记录看起来是"免费的"。但那样得到的是一份**密度由点击行为决定的
// 伪历史**——有人看的时候一秒一条，没人看的时候一条都没有。用它算"过去一周
// 的负载"会得到与真实情况毫无关系的结果，而它看起来像一份正常的图表。
//
// 采集有它自己的节奏，与谁在看无关。
package monitor

import (
	"context"
	"log"
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/service/ops/scheduler"
)

// Options 是采集器的参数。
type Options struct {
	// Interval 是采样间隔。
	//
	// 默认 60 秒：更短会让明细表增长过快（一台机器一天 1440 条），更长则
	// 图表上的毛刺会被平均掉——而"某个时刻突然飙了一下"正是排查时要找的。
	Interval time.Duration
	// DetailRetention 是**明细**的保留时长。
	//
	// **必须有保留期**：这些表只增不减，一台机器一天 1440 条、十台机器一个
	// 月就是 43 万条。没有清理策略的话，几个月后表会大到查询都变慢，而那时
	// 数据也早已没有排查价值。
	//
	// 默认 7 天与采集间隔是一对：60 秒 × 7 天 = 10080 条/机器，够看清
	// "最近一周什么时候忙"。
	DetailRetention time.Duration
	// CleanupInterval 是清理检查的间隔。
	CleanupInterval time.Duration
}

// DefaultOptions 返回默认参数。
func DefaultOptions() Options {
	return Options{
		Interval:        60 * time.Second,
		DetailRetention: 7 * 24 * time.Hour,
		CleanupInterval: time.Hour,
	}
}

// Collector 按固定间隔采集宿主机与虚拟机的指标。
type Collector struct {
	db    *gorm.DB
	agent agent.Client
	opts  Options
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	now   func() time.Time
	// runtimeTick 控制运行态对账频率：域扫描会读每个域的 XML 与磁盘
	// 容量，比指标采集重，默认每 5 分钟一次。
	runtimeTick int

	// obs 是调度事件的记录器；为 nil 时不做任何记录。
	//
	// 做成可选而不是构造参数：观测是**附加**能力，没接上时采集器照常工作。
	obs *scheduler.Recorder
}

// NewCollector 构造采集器。
func NewCollector(db *gorm.DB, client agent.Client, opts Options) *Collector {
	if opts.Interval <= 0 {
		opts = DefaultOptions()
	}
	return &Collector{
		db: db, agent: client, opts: opts,
		stop: make(chan struct{}), done: make(chan struct{}),
		now: time.Now,
	}
}

// Observe 接入调度事件记录。为 nil 时不做任何记录。
//
// 做成可选（而不是构造参数）：观测是**附加**能力，没接上时采集器照常工作。
// 把可选的观测塞进构造签名，会让每个测试都不得不传一个 nil 进去。
func (c *Collector) Observe(rec *scheduler.Recorder) { c.obs = rec }

// Start 启动采集循环（非阻塞）。
func (c *Collector) Start(ctx context.Context) {
	go c.loop(ctx)
}

// Stop 停止采集并等待循环退出。
func (c *Collector) Stop() {
	c.once.Do(func() { close(c.stop) })
	<-c.done
}

func (c *Collector) loop(ctx context.Context) {
	defer close(c.done)

	ticker := time.NewTicker(c.opts.Interval)
	defer ticker.Stop()

	cleanup := time.NewTicker(c.opts.CleanupInterval)
	defer cleanup.Stop()

	// 启动时先立刻采一轮：等一个间隔才出第一条数据，会让刚重启的控制面在
	// 头一分钟里看起来"没有监控"。
	c.tick(ctx)

	for {
		select {
		case <-c.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick(ctx)
		case <-cleanup.C:
			c.cleanup(ctx)
		}
	}
}

// Tick 采集一轮（导出供测试直接驱动，不必依赖定时器）。
func (c *Collector) Tick(ctx context.Context) { c.tick(ctx) }

// Cleanup 执行一次过期清理（导出理由同 Tick）。
func (c *Collector) Cleanup(ctx context.Context) { c.cleanup(ctx) }

// tick 采集一轮。
func (c *Collector) tick(ctx context.Context) {
	var nodes []model.Node
	if err := c.db.WithContext(ctx).
		Where("enroll_state = ? AND enabled = ?", model.NodeEnrollEnrolled, true).
		Find(&nodes).Error; err != nil {
		log.Printf("[monitor] 查询节点失败: %v", err)
		return
	}
	c.runtimeTick++
	// 每轮都对齐 VM 运行态。
	//
	// 采集周期是 60 秒、陈旧阈值 120 秒；此前这里用 `runtimeTick%5`
	// 把对账降频到 300 秒，于是两次对账之间的投影必然被判为「陈旧」，
	// 列表页长期挂着数据可能过期的提示，列表与详情在不同采样时刻还会
	// 给出相反结论（第 13 轮真机对账）。`virsh list --all` 每分钟一次的
	// 代价是可接受的——这也正是 syncRuntime 注释原本描述的设计。
	var hostCount, vmCount int
	for i := range nodes {
		// 先对齐 VM 运行态，再采指标：否则一台在面板外被 virsh 关掉的
		// 机器仍会被当成 running 去读指标，得到一串失败日志。
		c.syncRuntime(ctx, &nodes[i])
		hostOK, vms := c.collectNode(ctx, &nodes[i])
		if hostOK {
			hostCount++
		}
		vmCount += vms
	}

	// **一轮一条**，而不是一台机器一条。
	//
	// 记录的是「这一轮实际采到了东西」这件事本身——没有采到任何数据时不写，
	// 因为那正是"无事发生"，而这张表只该留下实际发生的动作。
	if hostCount > 0 {
		c.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyMetricsHost, Status: model.SchedulerDone,
			Scope:   strconv.Itoa(hostCount) + " 个节点",
			Message: "采集宿主机的 CPU / 内存 / 网络 / 磁盘指标",
		})
	}
	if vmCount > 0 {
		c.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyMetricsGuest, Status: model.SchedulerDone,
			Scope:   strconv.Itoa(vmCount) + " 台虚拟机",
			Message: "采集运行中虚拟机的指标，并累计运行时长与流量",
		})
	}
}

// syncRuntime 用节点的域列表校正 VM 投影状态。
//
// 控制面只缓存运行态；面板外通过 virsh 启停/删除不会经过任何写接口。
// 没有这一步时，投影会一直停留在上一次写操作时刻，直到用户手动刷新或
// 下一次操作 probe。每分钟一次 `virsh list --all` 的代价可以接受。
func (c *Collector) syncRuntime(ctx context.Context, node *model.Node) {
	result, err := c.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpNodeDomains, NodeID: node.ID,
	})
	if err != nil || result == nil || !result.Success {
		return
	}
	raw, ok := result.Data[agent.NodeDomainsDataKey]
	if !ok {
		// 旧版 agent / 测试替身没有这个键：不要据此把所有 VM 误判为 unknown。
		return
	}
	domains, _ := raw.([]agent.DomainInfo)
	byName := make(map[string]agent.DomainInfo, len(domains))
	for _, d := range domains {
		byName[d.Name] = d
	}

	var vms []model.VM
	// 软删除的 VM 已进回收站，不该再被域列表同步：否则它会被写成
	// present=false 并持续刷新 last_synced_at，进而在概览/告警里冒充
	// 「在管但已丢失」的机器（第 13 轮真机对账）。
	if err := c.db.WithContext(ctx).
		Where("node_id = ? AND deleted_at IS NULL", node.ID).Find(&vms).Error; err != nil {
		log.Printf("[monitor] 同步运行态查询 VM 失败 node=%d: %v", node.ID, err)
		return
	}
	now := c.now().UTC()
	for i := range vms {
		state := model.VMStatusUnknown
		d, exists := byName[vms[i].Name]
		if exists && d.State != "" {
			state = d.State
		}
		updates := map[string]any{
			"status":         state,
			"present":        exists,
			"last_synced_at": now,
		}
		// 磁盘容量也在这里对账（第 2 轮 NEW-001）。
		//
		// disk_gb 与 status 是同一类东西：**节点事实的投影**，不是控制面自己
		// 的配置。真实容量在 qemu-img 维护的镜像文件里，控制面只是记着它。
		// 以前这一轮同步只写 status，于是任何一次失败的写回都会永久留在面板
		// 上（真机复现：节点 11 GiB、面板 3 GB），而用户没有任何入口能把它
		// 改回来——投影必须能自愈，否则写入路径上的每个 bug 都会变成长期
		// 错误数据。
		//
		// 只在域确实存在且容量大于 0 时才写：域不存在时 d 是零值，写 0 会把
		// 一台机器的容量清成 0——那比不更新更坏。
		if exists && d.DiskGB > 0 {
			updates["disk_gb"] = d.DiskGB
		}
		if err := c.db.WithContext(ctx).Model(&model.VM{}).
			Where("id = ?", vms[i].ID).
			Updates(updates).Error; err != nil {
			log.Printf("[monitor] 同步 VM %d 状态失败: %v", vms[i].ID, err)
		}
	}
}

// collectNode 采集一台宿主机及其上的虚拟机，返回宿主机是否采到、
// 以及采到了几台虚拟机的指标。
func (c *Collector) collectNode(ctx context.Context, node *model.Node) (bool, int) {
	at := c.now().UTC()

	// 1) 宿主机指标。
	//
	// **采集失败时不写记录**，而不是写一条全 0 的。写 0 会让图表显示
	// "这台宿主机 CPU 为 0%"，而那与"没采到"是两回事——前者看起来正常，
	// 后者需要人去查节点。一张混了假 0 的曲线比一段空白更危险。
	result, err := c.agent.Execute(ctx, agent.Operation{
		Kind: agent.OpHostStats, NodeID: node.ID, Target: node.Name,
	})
	if err != nil {
		log.Printf("[monitor] 采集宿主机指标失败 node=%d: %v", node.ID, err)
		return false, 0
	}
	if !result.Success {
		return false, 0
	}
	host, ok := result.Data[agent.HostStatsDataKey].(agent.HostStats)
	if !ok {
		return false, 0
	}

	rec := model.HostStatsRecord{
		NodeID: node.ID, At: at,
		CPUPercent: host.CPUPercent, CPUCores: host.CPUCores,
		MemUsedMB: host.MemUsedMB, MemTotalMB: host.MemTotalMB,
		SwapUsedMB: host.SwapUsedMB,
		Load1:      host.Load1, Load5: host.Load5, Load15: host.Load15,
		NetInBytes: host.NetInBytes, NetOutBytes: host.NetOutBytes,
		DiskReadBytes: host.DiskReadBytes, DiskWriteBytes: host.DiskWriteBytes,
		UptimeSeconds: host.UptimeSeconds,
	}
	if host.Devices != "" {
		rec.DeviceStats = &host.Devices
	}
	hostOK := true
	if err := c.db.WithContext(ctx).Create(&rec).Error; err != nil {
		log.Printf("[monitor] 写入宿主机指标失败 node=%d: %v", node.ID, err)
		hostOK = false
	}

	// 2) 该节点上运行中的虚拟机。
	return hostOK, c.collectNodeVMs(ctx, node, at)
}

// collectNodeVMs 采集节点上运行中的虚拟机。
//
// 只采**运行中**的：停机机器没有指标可读，而给它们写记录会让图表上出现
// 一条贴着 0 的线——那看起来像"这台机器很闲"，而不是"它没在跑"。
func (c *Collector) collectNodeVMs(ctx context.Context, node *model.Node, at time.Time) int {
	written := 0
	var vms []model.VM
	if err := c.db.WithContext(ctx).
		Where("node_id = ? AND status = ? AND present = ?",
			node.ID, model.VMStatusRunning, true).
		Find(&vms).Error; err != nil {
		log.Printf("[monitor] 查询运行中虚拟机失败 node=%d: %v", node.ID, err)
		return 0
	}

	interval := int64(c.opts.Interval.Seconds())
	date := dayOf(at)

	for i := range vms {
		vm := &vms[i]

		result, err := c.agent.Execute(ctx, agent.Operation{
			Kind: agent.OpVMStats, NodeID: node.ID, Target: vm.Name,
			Params: map[string]any{"vm_id": vm.ID},
		})
		if err != nil || !result.Success {
			// 单台采不到不影响别的机器，也不写记录（理由同上）。
			continue
		}
		s, ok := result.Data[agent.StatsDataKey].(agent.VMStats)
		if !ok {
			continue
		}

		// 协议层给的是**速率**（Kbps），而记录列是字节。换算成「本区间的
		// 字节数」而不是原样存速率：后者会让这一列的含义随实现而变，而
		// 按天聚合流量时又得再乘一次间隔——两处各算一次迟早对不上。
		bytesIn := kbpsToBytes(s.NetRxKbps, interval)
		bytesOut := kbpsToBytes(s.NetTxKbps, interval)

		rec := model.VMStatsRecord{
			VMID: vm.ID, NodeID: node.ID, At: at,
			CPUPercent: s.CPUPercent,
			MemUsedMB:  int64(s.MemUsedMB),
			MemPercent: memPercent(int64(s.MemUsedMB), int64(s.MemTotalMB)),
			NetInBytes: bytesIn, NetOutBytes: bytesOut,
			DiskReadBytes:  kbpsToBytes(s.DiskReadKbps, interval),
			DiskWriteBytes: kbpsToBytes(s.DiskWriteKbps, interval),
			UptimeSeconds:  s.UptimeSeconds,
		}
		if err := c.db.WithContext(ctx).Create(&rec).Error; err != nil {
			log.Printf("[monitor] 写入虚拟机指标失败 vm=%d: %v", vm.ID, err)
		} else {
			written++
		}

		// 3) 来宾代理可达性写进投影（第 6 轮 DEF-R6-01）。
		//
		// 这个字段以前**没有任何写入方**，恒为 false，于是 `GET /vms/:id` 的
		// `init.ready` 永远翻不过来——真机上 agent 在 t=40s 就连通了，面板
		// 240s 里 20 次采样全是 agent_unavailable。
		//
		// 写在这里而不是运行态扫描（node.domains）里：那一轮每 5 个节拍才
		// 跑一次（=5 分钟），而本函数**每轮都对每台运行中的 VM 跑一次**
		// （60s）。用户在等的是一个"它到底起来没有"的结论，5 分钟的滞后
		// 与 4 分钟就放弃的耐心相比没有意义。
		//
		// 只在指标采到时写：采不到说明这台机器当前联系不上，那时把
		// guest_agent 写成 false 是拿"采不到"冒充"agent 不可用"。
		if err := c.db.WithContext(ctx).Model(&model.VM{}).
			Where("id = ?", vm.ID).
			Update("guest_agent", s.GuestAgent).Error; err != nil {
			log.Printf("[monitor] 写入来宾代理状态失败 vm=%d: %v", vm.ID, err)
		}

		// 4) 累计运行时长与流量。
		//
		// 这两件事只能在"采到指标的那一刻"做：它们是**区间增量**，而区间
		// 长度就是采样间隔。放进别的流程里会让增量与实际经过的时间对不上。
		c.accumulate(ctx, vm.ID, vm.NodeID, vm.OwnerID, date, interval, &s)
	}
	return written
}

// accumulate 把本轮的运行时长与流量增量累加到当天。
//
// 用 upsert（`ON CONFLICT ... DO UPDATE`）而不是"先查再改"：并发采集（多台
// 机器、以及将来可能的多个采集器）下，"先查再改"会让两次读到的都是旧值，
// 后写的覆盖先写的——而**少算的量没有任何地方会发现**，它只表现为"统计
// 出来的时长比实际短"。
func (c *Collector) accumulate(
	ctx context.Context, vmID, nodeID int64, ownerID *int64,
	date time.Time, deltaSeconds int64, s *agent.VMStats,
) {
	runtime := model.VMRuntimeDaily{
		VMID: vmID, NodeID: nodeID, OwnerID: ownerID,
		Date: date, Seconds: int(deltaSeconds),
	}
	if err := c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "vm_id"}, {Name: "date"}},
		DoUpdates: clause.Assignments(map[string]any{
			"seconds": gorm.Expr("vm_runtime_daily.seconds + ?", deltaSeconds),
			// owner 可能变过（机器转手），以最新的为准。
			"owner_id":   ownerID,
			"updated_at": c.now(),
		}),
	}).Create(&runtime).Error; err != nil {
		log.Printf("[monitor] 累计运行时长失败 vm=%d: %v", vmID, err)
	}

	// 流量：本轮的**增量**。
	//
	// 这里用的速率（Kbps）乘采样间隔换算成字节——因为协议层给的
	// 是速率而非累计值。真实实现里如果节点给的是累计字节，这里应当改成
	// "与上一条记录相减"，那更准（不受采样抖动影响）。
	inBytes := kbpsToBytes(s.NetRxKbps, deltaSeconds)
	outBytes := kbpsToBytes(s.NetTxKbps, deltaSeconds)
	if inBytes == 0 && outBytes == 0 {
		return
	}

	traffic := model.TrafficStatDaily{
		ScopeType: model.TrafficScopeVM, ScopeID: vmID, OwnerID: ownerID,
		Date: date, BytesIn: inBytes, BytesOut: outBytes,
	}
	if err := c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "scope_type"}, {Name: "scope_id"}, {Name: "date"}},
		DoUpdates: clause.Assignments(map[string]any{
			"bytes_in":   gorm.Expr("traffic_stat_daily.bytes_in + ?", inBytes),
			"bytes_out":  gorm.Expr("traffic_stat_daily.bytes_out + ?", outBytes),
			"owner_id":   ownerID,
			"updated_at": c.now(),
		}),
	}).Create(&traffic).Error; err != nil {
		log.Printf("[monitor] 累计流量失败 vm=%d: %v", vmID, err)
	}
}

// cleanup 清理过期的明细。
//
// 只清**明细**，不动聚合表：`vm_runtime_daily` 与 `traffic_stat_daily` 是
// 按天一行，一台机器一年也才 365 行——它们才是长期要留的东西（配额按月算、
// 运行时长要能回溯），而明细只在排查最近几天时有用。
func (c *Collector) cleanup(ctx context.Context) {
	cutoff := c.now().UTC().Add(-c.opts.DetailRetention)
	deleted := int64(0)

	res := c.db.WithContext(ctx).
		Where("at < ?", cutoff).Delete(&model.HostStatsRecord{})
	if res.Error != nil {
		log.Printf("[monitor] 清理宿主机指标失败: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Printf("[monitor] 清理了 %d 条过期宿主机指标", res.RowsAffected)
		deleted += res.RowsAffected
	}

	res = c.db.WithContext(ctx).
		Where("at < ?", cutoff).Delete(&model.VMStatsRecord{})
	if res.Error != nil {
		log.Printf("[monitor] 清理虚拟机指标失败: %v", res.Error)
	} else if res.RowsAffected > 0 {
		log.Printf("[monitor] 清理了 %d 条过期虚拟机指标", res.RowsAffected)
		deleted += res.RowsAffected
	}

	// **删了才记。** 清理器每天都在跑，而绝大多数时候它一条都删不掉——
	// 那些轮次不该留下任何东西，否则"清理"这件事会在事件列表里天天出现，
	// 而用户真正需要看到的是"某天它删掉了 50 万条"。
	if deleted > 0 {
		c.obs.Record(ctx, scheduler.Event{
			Key: scheduler.KeyMetricsDaily, Status: model.SchedulerDone,
			Scope:   strconv.FormatInt(deleted, 10) + " 条明细",
			Message: "清理超过保留期的指标明细（按天聚合的数据保留）",
		})
	}
}

// dayOf 把时刻归一化到 UTC 当天零点。
//
// **统一用 UTC**：按本地时区切天会让"这一天的运行时长"随服务器的时区设置
// 而变，而配额是按月结算的——时区一改，历史数据的分桶就全错了。
func dayOf(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// kbpsToBytes 把 Kbps 速率换算成「在这段时间里传了多少字节」。
//
// Kbps 是**千比特每秒**（网络速率的通行口径），因此先乘 1000 得到 bit/s、
// 再除以 8 得到 B/s、最后乘秒数。三处换算里任何一处写错都会得到一个数量级
// 错误的数字，而它看起来完全正常。
func kbpsToBytes(kbps float64, seconds int64) int64 {
	if kbps <= 0 || seconds <= 0 {
		return 0
	}
	return int64(kbps * 1000 / 8 * float64(seconds))
}

func memPercent(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}
