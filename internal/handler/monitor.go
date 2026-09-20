package handler

import (
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/monitor"
)

// Monitor 提供指标历史查询（F-8-01 / F-8-02）。
type Monitor struct {
	svc *monitor.Service
}

// NewMonitor 构造接口。
func NewMonitor(svc *monitor.Service) *Monitor { return &Monitor{svc: svc} }

// rangeOf 解析查询时间范围。
//
// 默认最近 1 小时：不给默认值的话，一次不带参数的调用会扫全表——而那在
// 数据攒了几个月之后会慢到让人以为接口挂了。
func rangeOf(c *app.RequestContext) (time.Time, time.Time) {
	to := time.Now().UTC()
	from := to.Add(-time.Hour)
	if v := c.Query("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	if v := c.Query("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	return from, to
}

// HostSeries 宿主机的指标序列（API-230）。
//
// 可选 `device` 按物理设备筛选：它只影响网络与磁盘两条曲线（CPU 与内存是
// 整机概念）。「整体流量涨了，是哪一块涨的」只有这一条路径能回答。
func (h *Monitor) HostSeries(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	from, to := rangeOf(c)

	series, err := h.svc.HostSeries(ctx, nodeID, from, to, c.Query("device"))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, series)
}

// HostDevices 该节点上可筛选的物理设备（API-230b）。
func (h *Monitor) HostDevices(ctx context.Context, c *app.RequestContext) {
	nodeID := int64(queryInt(c, "node_id"))
	if nodeID <= 0 {
		api.Fail(c, api.InvalidParameter("必须指定 node_id"))
		return
	}
	devices, err := h.svc.HostDevices(ctx, nodeID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"devices": devices})
}

// VMSeries 虚拟机的指标序列（API-231）。
func (h *Monitor) VMSeries(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	from, to := rangeOf(c)

	series, err := h.svc.VMSeries(ctx, vmID, from, to, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, series)
}

// Runtime 虚拟机的运行时长（API-232）。
//
// 数据来自采样器按天累计的 `vm_runtime_daily`——它是 F-8-06 的超限处置与
// F-1-08 配额里「运行时长」维度的共同数据源。
func (h *Monitor) Runtime(ctx context.Context, c *app.RequestContext) {
	vmID, err := namedPathID(c, "id", "虚拟机 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	from, to := rangeOf(c)

	total, err := h.svc.RuntimeTotal(ctx, vmID, from, to, authz.ViewerOf(c))
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"seconds": total})
}
