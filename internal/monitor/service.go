package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供指标历史查询。
type Service struct {
	db *gorm.DB
}

// NewService 构造服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// Series 是一个时间序列。
type Series struct {
	// Points 按时间升序。
	Points []Point `json:"points"`
	// Interval 是相邻两点的间隔秒数，供界面画图时对齐横轴。
	Interval int `json:"interval_seconds"`
}

// Point 是序列上的一个点。
type Point struct {
	At          string  `json:"at"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemUsedMB   int64   `json:"mem_used_mb"`
	MemTotalMB  int64   `json:"mem_total_mb"`
	NetInBytes  int64   `json:"net_in_bytes"`
	NetOutBytes int64   `json:"net_out_bytes"`
	// DiskReadBytes / DiskWriteBytes 与网络一样是**累计值**：速率由相邻两点
	// 相减得到（界面负责）。存累计值的理由同网络——某次采样丢了之后，速率
	// 只是那一段变长，总量仍然对。
	DiskReadBytes  int64 `json:"disk_read_bytes"`
	DiskWriteBytes int64 `json:"disk_write_bytes"`
	// DiskIOPS 仅对虚拟机有意义（宿主机侧没有按点记录的 IOPS）。
	DiskIOPS int `json:"disk_iops,omitempty"`
}

// DeviceView 是一个可筛选的物理设备。
type DeviceView struct {
	Name string `json:"name"`
	// Kind 取值 net / disk。
	Kind string `json:"kind"`
}

// 一次查询最多返回多少点。
//
// 上限存在的理由是图表本身：一条曲线上几百个点之外的信息量就不再增加，
// 而查询会随范围线性变慢。超出时**按时间降采样**而不是截断——截断会让
// 图表画出一段"最近的"数据却看起来覆盖了整个范围，那是一种误导。
const maxPoints = 720

// HostSeries 返回宿主机的指标序列（F-8-01）。
func (s *Service) HostSeries(
	ctx context.Context, nodeID int64, from, to time.Time, device string,
) (*Series, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	var rows []model.HostStatsRecord
	if err := s.db.WithContext(ctx).
		Where("node_id = ? AND at >= ? AND at <= ?", nodeID, from, to).
		Order("at ASC").Find(&rows).Error; err != nil {
		log.Printf("[monitor] 查询宿主机序列失败: %v", err)
		return nil, api.Internal()
	}

	rows = downsampleHost(rows, maxPoints)
	points := make([]Point, 0, len(rows))
	for i := range rows {
		netIn, netOut := rows[i].NetInBytes, rows[i].NetOutBytes
		read, write := rows[i].DiskReadBytes, rows[i].DiskWriteBytes
		// 按设备筛选只影响**网络与磁盘**：CPU 与内存是整机概念，一块网卡
		// 没有自己的内存。此时整机值照常返回，界面上那两张图不变。
		if device != "" {
			if d, ok := deviceOf(rows[i].DeviceStats, device); ok {
				if d.Kind == "net" {
					netIn, netOut = d.ReadBytes, d.WriteBytes
				} else {
					read, write = d.ReadBytes, d.WriteBytes
				}
			}
		}
		points = append(points, Point{
			At: rows[i].At.Format(time.RFC3339), CPUPercent: rows[i].CPUPercent,
			MemUsedMB: rows[i].MemUsedMB, MemTotalMB: rows[i].MemTotalMB,
			NetInBytes: netIn, NetOutBytes: netOut,
			DiskReadBytes: read, DiskWriteBytes: write,
		})
	}
	return &Series{Points: points, Interval: s.intervalOf(len(rows), from, to)}, nil
}

// HostDevices 列出该节点上可筛选的物理设备。
//
// 清单取自**最近一次采样**而不是再去探测一次：设备名几乎不变，而为打开
// 一个下拉框去节点上现取一次不值得（节点不可达时还会让整个面板失败）。
func (s *Service) HostDevices(ctx context.Context, nodeID int64) ([]DeviceView, error) {
	if err := s.ensureNode(ctx, nodeID); err != nil {
		return nil, err
	}
	var row model.HostStatsRecord
	err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).
		Order("at DESC").First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return []DeviceView{}, nil
	case err != nil:
		log.Printf("[monitor] 查询设备采样失败 node=%d: %v", nodeID, err)
		return nil, api.Internal()
	}
	var devices []agent.HostDeviceStat
	if row.DeviceStats == nil || *row.DeviceStats == "" {
		return []DeviceView{}, nil
	}
	if err := json.Unmarshal([]byte(*row.DeviceStats), &devices); err != nil {
		// 设备明细是**附加信息**：解析不了不能让整个接口失败，否则一个格式
		// 变更会让监控页整体打不开。
		log.Printf("[monitor] 解析设备明细失败 node=%d: %v", nodeID, err)
		return []DeviceView{}, nil
	}
	out := make([]DeviceView, 0, len(devices))
	for i := range devices {
		out = append(out, DeviceView{Name: devices[i].Name, Kind: devices[i].Kind})
	}
	return out, nil
}

// deviceOf 从一条采样的设备明细里取出指定设备。
func deviceOf(raw *string, name string) (agent.HostDeviceStat, bool) {
	if raw == nil || *raw == "" {
		return agent.HostDeviceStat{}, false
	}
	var devices []agent.HostDeviceStat
	if err := json.Unmarshal([]byte(*raw), &devices); err != nil {
		return agent.HostDeviceStat{}, false
	}
	for i := range devices {
		if devices[i].Name == name {
			return devices[i], true
		}
	}
	return agent.HostDeviceStat{}, false
}

// VMSeries 返回虚拟机的指标序列（F-8-02）。
func (s *Service) VMSeries(
	ctx context.Context, vmID int64, from, to time.Time, v authz.Viewer,
) (*Series, error) {
	if err := s.ensureVM(ctx, vmID, v); err != nil {
		return nil, err
	}
	var rows []model.VMStatsRecord
	if err := s.db.WithContext(ctx).
		Where("vm_id = ? AND at >= ? AND at <= ?", vmID, from, to).
		Order("at ASC").Find(&rows).Error; err != nil {
		log.Printf("[monitor] 查询虚拟机序列失败: %v", err)
		return nil, api.Internal()
	}

	rows = downsampleVM(rows, maxPoints)
	points := make([]Point, 0, len(rows))
	for i := range rows {
		points = append(points, Point{
			At: rows[i].At.Format(time.RFC3339), CPUPercent: rows[i].CPUPercent,
			MemUsedMB:  rows[i].MemUsedMB,
			NetInBytes: rows[i].NetInBytes, NetOutBytes: rows[i].NetOutBytes,
			DiskReadBytes:  rows[i].DiskReadBytes,
			DiskWriteBytes: rows[i].DiskWriteBytes,
			DiskIOPS:       rows[i].DiskIOPS,
		})
	}
	return &Series{Points: points, Interval: s.intervalOf(len(rows), from, to)}, nil
}

// intervalOf 推算相邻两点的间隔。
//
// 由服务端算好而不是让界面自己从时间戳推：降采样之后点的间隔不再等于采样
// 间隔，而界面画图时需要知道它才能对齐横轴。
func (s *Service) intervalOf(pointCount int, from, to time.Time) int {
	if pointCount <= 1 {
		return 0
	}
	return int(to.Sub(from).Seconds()) / (pointCount - 1)
}

// RuntimeTotal 返回一台虚拟机在某段时间内的运行秒数（F-8-06）。
func (s *Service) RuntimeTotal(
	ctx context.Context, vmID int64, from, to time.Time, v authz.Viewer,
) (int64, error) {
	if err := s.ensureVM(ctx, vmID, v); err != nil {
		return 0, err
	}
	var total int64
	if err := s.db.WithContext(ctx).Model(&model.VMRuntimeDaily{}).
		Where("vm_id = ? AND date >= ? AND date <= ?", vmID, dayOf(from), dayOf(to)).
		Select("COALESCE(SUM(seconds), 0)").Scan(&total).Error; err != nil {
		log.Printf("[monitor] 统计运行时长失败: %v", err)
		return 0, api.Internal()
	}
	return total, nil
}

// --- 内部 ---

// downsampleHost / downsampleVM 按时间等距抽稀。
//
// **等距抽稀而不是取前 N 条**：截断会让图表画出一段"最近的"数据，却看起来
// 覆盖了整个查询范围——那是一种误导，而且它不容易被发现（曲线仍然是连续的）。
func downsampleHost(rows []model.HostStatsRecord, limit int) []model.HostStatsRecord {
	if len(rows) <= limit {
		return rows
	}
	out := make([]model.HostStatsRecord, 0, limit)
	step := float64(len(rows)-1) / float64(limit-1)
	for i := 0; i < limit; i++ {
		out = append(out, rows[int(float64(i)*step)])
	}
	return out
}

func downsampleVM(rows []model.VMStatsRecord, limit int) []model.VMStatsRecord {
	if len(rows) <= limit {
		return rows
	}
	out := make([]model.VMStatsRecord, 0, limit)
	step := float64(len(rows)-1) / float64(limit-1)
	for i := 0; i < limit; i++ {
		out = append(out, rows[int(float64(i)*step)])
	}
	return out
}

func (s *Service) ensureNode(ctx context.Context, nodeID int64) error {
	var n model.Node
	if err := s.db.WithContext(ctx).Select("id").Where("id = ?", nodeID).First(&n).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		log.Printf("[monitor] 查询节点失败: %v", err)
		return api.Internal()
	}
	return nil
}

func (s *Service) ensureVM(ctx context.Context, id int64, v authz.Viewer) error {
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("虚拟机不存在")
		}
		log.Printf("[monitor] 查询虚拟机失败: %v", err)
		return api.Internal()
	}
	// 404 而非 403：403 会确认「这个 ID 存在」。
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return api.NotFound("虚拟机不存在")
	}
	return nil
}
