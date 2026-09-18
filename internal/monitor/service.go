package monitor

import (
	"context"
	"errors"
	"log"
	"time"

	"gorm.io/gorm"

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
}

// 一次查询最多返回多少点。
//
// 上限存在的理由是图表本身：一条曲线上几百个点之外的信息量就不再增加，
// 而查询会随范围线性变慢。超出时**按时间降采样**而不是截断——截断会让
// 图表画出一段"最近的"数据却看起来覆盖了整个范围，那是一种误导。
const maxPoints = 720

// HostSeries 返回宿主机的指标序列（F-8-01）。
func (s *Service) HostSeries(ctx context.Context, nodeID int64, from, to time.Time) (*Series, error) {
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
		points = append(points, Point{
			At: rows[i].At.Format(time.RFC3339), CPUPercent: rows[i].CPUPercent,
			MemUsedMB: rows[i].MemUsedMB, MemTotalMB: rows[i].MemTotalMB,
			NetInBytes: rows[i].NetInBytes, NetOutBytes: rows[i].NetOutBytes,
		})
	}
	return &Series{Points: points, Interval: s.intervalOf(len(rows), from, to)}, nil
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
