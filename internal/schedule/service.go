package schedule

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// View 是定时任务的接口形态。
//
// 不直接返回 model.VMSchedule：那会把 Go 的字段名（ID / VMID / NextRunAt）
// 原样暴露成 JSON 键，而接口一旦发布就改不动了。中间隔一层还能顺手把星期
// 从 `1,3,5` 解析成数组——这个解析每个调用方各写一遍必然写出不同的边界处理。
type View struct {
	ID           int64  `json:"id"`
	Action       string `json:"action"`
	ScheduleType string `json:"schedule_type"`
	// Weekdays 是解析后的星期列表（1=周一 … 7=周日），已排序。
	Weekdays []int `json:"weekdays"`
	// TimeOfDay 形如 `03:00`。
	TimeOfDay string `json:"time_of_day"`

	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastResult 取值 success / failed / skipped；为空表示还没跑过。
	//
	// `skipped` 是一个会真实出现的值（服务停机期间错过的时间点不补执行），
	// 界面必须把它显示出来而不是当成空——否则用户会以为任务没跑过。
	LastResult *string `json:"last_result,omitempty"`
	LastTaskID *int64  `json:"last_task_id,omitempty"`

	Enabled bool `json:"enabled"`
}

// ToView 把库中的记录转成接口形态。
func ToView(row *model.VMSchedule) View {
	timeOfDay := "00:00"
	if row.RunAt != nil {
		timeOfDay = *row.RunAt
	}

	// 按 1..7 顺序输出，而不是直接遍历 map——map 的顺序随机，
	// 会让「周一、周三」在某次请求里变成「周三、周一」。
	parsed := parseWeekdays(row.Weekdays)
	days := make([]int, 0, len(parsed))
	for d := 1; d <= 7; d++ {
		if parsed[d] {
			days = append(days, d)
		}
	}

	return View{
		ID: row.ID, Action: row.Action, ScheduleType: row.ScheduleType,
		Weekdays: days, TimeOfDay: timeOfDay,
		NextRunAt: row.NextRunAt, LastRunAt: row.LastRunAt,
		LastResult: row.LastResult, LastTaskID: row.LastTaskID,
		Enabled: row.Enabled,
	}
}

// Service 提供定时任务的增删改查。
type Service struct {
	db *gorm.DB
}

// NewService 构造定时任务服务。
func NewService(db *gorm.DB) *Service { return &Service{db: db} }

// CreateRequest 是新建定时任务的请求。
// optString 空串存 NULL 而不是空串。
//
// 空串在界面上会被渲染成一个空的「快照名：」，看起来像名字丢了；NULL 让
// 界面能明确显示「未指定」。
func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

type CreateRequest struct {
	// Action 取值 start / shutdown / delete / snapshot。
	Action string
	// SnapshotName 快照名模板，仅 snapshot 动作使用。
	//
	// 留空时服务端按时间生成一个（形如 `auto-20260921-0300）。让用户填模板的
	// 用处是"在界面上一眼看出这批快照是谁建的——定时任务那份。
	SnapshotName string
	// IncludeMemory 是否保存运行现场。仅 snapshot 动作使用。
	IncludeMemory bool
	// ScheduleType 取值 once / daily / weekly。
	ScheduleType string
	// Weekdays 每周模式下要执行的日子，取值 1=周一 … 7=周日。
	Weekdays []int
	// TimeOfDay 执行时刻，形如 `03:00`。
	TimeOfDay string
	// Date 一次性任务的执行日期，形如 `2026-09-20`；仅 once 使用。
	Date string
}

// Create 新建一条定时任务。
func (s *Service) Create(
	ctx context.Context, vmID int64, req CreateRequest, v authz.Viewer,
) (*View, error) {
	vm, err := s.vmFor(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	action := strings.TrimSpace(req.Action)
	switch action {
	case model.ScheduleActionStart, model.ScheduleActionShutdown,
		model.ScheduleActionDelete, model.ScheduleActionSnapshot:
	default:
		return nil, api.InvalidParameter("不支持的动作，可选 开机 / 关机 / 删除 / 创建快照")
	}

	kind := strings.TrimSpace(req.ScheduleType)
	switch kind {
	case model.ScheduleTypeOnce, model.ScheduleTypeDaily, model.ScheduleTypeWeekly:
	default:
		return nil, api.InvalidParameter("不支持的调度类型，可选 一次性 / 每天 / 每周")
	}

	// **删除类仅允许一次性**（F-7-05）。
	//
	// 这条限制针对的是一类具体事故：周期性删除任务设完就被遗忘，直到某天
	// 虚拟机连同数据一起消失，而那一刻没有任何人知道发生了什么。删除是
	// 唯一一个「执行完就再没有补救机会」的定时动作，周期执行它，等于把
	// 一个不可逆操作交给一个不会提醒任何人的计时器。
	if action == model.ScheduleActionDelete && kind != model.ScheduleTypeOnce {
		return nil, api.ValidationFailed(
			"删除类定时任务仅支持「一次性」：周期删除会在无人察觉的情况下反复抹掉数据")
	}
	// 快照**允许周期执行**，这与删除相反：快照是累加的、可再删的，而它的价值恰恰在于"定期留下还原点"。
	//
	// 但周期快照会**累积**，因此这里只接受有名字模板的写法——名字里带时间，用户才能一眼看出这批是定时建的，而不是被误当成手动快照。
	if action == model.ScheduleActionSnapshot && strings.TrimSpace(req.SnapshotName) == "" {
		return nil, api.InvalidParameter("定时快照必须填快照名模板，便于区分自动快照与手动快照")
	}

	hour, minute, err := parseTimeOfDay(req.TimeOfDay)
	if err != nil {
		return nil, err
	}
	runAt := fmt.Sprintf("%02d:%02d", hour, minute)

	row := model.VMSchedule{
		// 快照动作的两项参数落库：调度器触发时直接读，不必再去推断这次要建什么。
		SnapshotName:  optString(strings.TrimSpace(req.SnapshotName)),
		IncludeMemory: req.IncludeMemory,
		VMID:          vm.ID,
		NodeID:        vm.NodeID,
		Action:        action,
		ScheduleType:  kind,
		RunAt:         &runAt,
		Enabled:       true,
		CreatedBy:     &v.UserID,
	}

	switch kind {
	case model.ScheduleTypeOnce:
		at, err := parseOnceAt(req.Date, hour, minute)
		if err != nil {
			return nil, err
		}
		row.NextRunAt = at

	case model.ScheduleTypeWeekly:
		days := normalizeWeekdays(req.Weekdays)
		if len(days) == 0 {
			// 每周模式必须勾至少一天：不勾的话这条任务永远不会执行，
			// 而用户会以为已经设好了。
			return nil, api.ValidationFailed("每周任务至少要选择一天")
		}
		row.Weekdays = &days

	case model.ScheduleTypeDaily:
		// 每天模式不需要额外字段。
	}

	// 用 NextAfter 算首次执行时刻，而不是自己再写一遍日期推算——
	// 两处算同一件事，迟早会不一致，而这类不一致只在某个特定星期几
	// 才会暴露，极难排查。
	if kind != model.ScheduleTypeOnce {
		row.NextRunAt = NextAfter(&row, time.Now())
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[schedule] 创建定时任务失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	// 返回 ToView 而不是原始行：NextRunAt 等字段由计算得出，让调用方拿到
	// 落库后的真实值，避免界面显示的与库里存的不一致。
	view := ToView(&row)
	return &view, nil
}

// List 返回某台虚拟机的定时任务。
func (s *Service) List(
	ctx context.Context, vmID int64, v authz.Viewer,
) ([]View, error) {
	if _, err := s.vmFor(ctx, vmID, v); err != nil {
		return nil, err
	}

	var rows []model.VMSchedule
	err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).
		Order("id DESC").
		Find(&rows).Error
	if err != nil {
		log.Printf("[schedule] 查询定时任务失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	out := make([]View, 0, len(rows))
	for i := range rows {
		out = append(out, ToView(&rows[i]))
	}
	return out, nil
}

// SetEnabled 启用或停用一条定时任务。
func (s *Service) SetEnabled(
	ctx context.Context, vmID, id int64, enabled bool, v authz.Viewer,
) (*View, error) {
	if _, err := s.vmFor(ctx, vmID, v); err != nil {
		return nil, err
	}

	row, err := s.load(ctx, vmID, id)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{"enabled": enabled}
	if enabled {
		// 重新启用时要重算下次执行时刻。
		//
		// 停在停用期间的那次不该被追着执行——理由与「错过不补」相同：
		// 用户重新打开这个任务，期待的是「从现在开始按计划执行」，
		// 而不是「先把停用期间的补上」。
		row.Enabled = true
		updates["next_run_at"] = NextAfter(row, time.Now())
	} else {
		// 停用时清空下次执行时刻：留着它会让调度器的扫描条件
		// （next_run_at <= now）在重新启用前一直命中，白白扫一遍。
		updates["next_run_at"] = nil
	}

	if err := s.db.WithContext(ctx).Model(&model.VMSchedule{}).
		Where("id = ?", id).Updates(updates).Error; err != nil {
		log.Printf("[schedule] 更新定时任务失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}

	updated, err := s.load(ctx, vmID, id)
	if err != nil {
		return nil, err
	}
	view := ToView(updated)
	return &view, nil
}

// Delete 删除一条定时任务。
//
// 删除的是**任务定义**，不是虚拟机——这一点在文案与界面上都要说清楚，
// 否则用户会因为「这里能删东西」而产生误解。
func (s *Service) Delete(ctx context.Context, vmID, id int64, v authz.Viewer) error {
	if _, err := s.vmFor(ctx, vmID, v); err != nil {
		return err
	}
	if _, err := s.load(ctx, vmID, id); err != nil {
		return err
	}

	err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", id, vmID).
		Delete(&model.VMSchedule{}).Error
	if err != nil {
		log.Printf("[schedule] 删除定时任务失败 id=%d: %v", id, err)
		return api.Internal()
	}
	return nil
}

// load 读取属于该虚拟机的定时任务。
func (s *Service) load(ctx context.Context, vmID, id int64) (*model.VMSchedule, error) {
	var row model.VMSchedule
	err := s.db.WithContext(ctx).
		Where("id = ? AND vm_id = ?", id, vmID).
		First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("定时任务不存在")
	case err != nil:
		log.Printf("[schedule] 查询定时任务失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}
	return &row, nil
}

// vmFor 按归属读取虚拟机。
//
// 与 vm 包同名方法有一处重复，但边界更重要：定时任务服务不该为了复用一次
// 查询就去依赖 vm 包的内部实现。两边都遵循同一条规则——不属于当前视角的
// 返回 **404 而非 403**，否则可以据此枚举他人有哪些虚拟机。
func (s *Service) vmFor(ctx context.Context, vmID int64, v authz.Viewer) (*model.VM, error) {
	query := s.db.WithContext(ctx).Where("id = ?", vmID)
	if !v.IsAdmin {
		query = query.Where("owner_id = ?", v.UserID)
	}

	var vm model.VM
	err := query.First(&vm).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, api.NotFound("虚拟机不存在")
	case err != nil:
		log.Printf("[schedule] 查询虚拟机失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}
	return &vm, nil
}

// parseTimeOfDay 解析 `HH:MM`。
func parseTimeOfDay(raw string) (hour, minute int, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, api.InvalidParameter("请指定执行时刻，形如 03:00")
	}
	hour, minute = parseRunAt(&raw)
	// parseRunAt 对非法输入静默回落到 0 点（调度循环需要它不报错），
	// 但创建时**必须**拒绝：把用户写的 `25:00` 存成 `00:00` 会让任务
	// 在一个他从未指定的时刻执行。
	if fmt.Sprintf("%02d:%02d", hour, minute) != normalize(raw) {
		return 0, 0, api.InvalidParameter("执行时刻格式不正确，应为 HH:MM（如 03:00）")
	}
	return hour, minute, nil
}

// normalize 把 `3:0` 之类的写法补成 `03:00` 以便与解析结果比较。
func normalize(raw string) string {
	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return raw
	}
	h, m := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if len(h) == 1 {
		h = "0" + h
	}
	if len(m) == 1 {
		m = "0" + m
	}
	return h + ":" + m
}

// parseOnceAt 解析一次性任务的执行日期。
func parseOnceAt(date string, hour, minute int) (*time.Time, error) {
	date = strings.TrimSpace(date)
	if date == "" {
		return nil, api.InvalidParameter("一次性任务需要指定日期")
	}
	day, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return nil, api.InvalidParameter("日期格式不正确，应为 YYYY-MM-DD")
	}

	at := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.Local)
	// 过去的时间点**拒绝**而不是静默改成明天：用户的意图是「在那个时刻执行」，
	// 而把过去的日期改成明天执行，会做出他从没要求过的操作。
	if !at.After(time.Now()) {
		return nil, api.ValidationFailed("执行时间已过去，请选择将来的时刻")
	}
	return &at, nil
}

// normalizeWeekdays 把星期列表整理成 `1,3,5` 形式。
func normalizeWeekdays(days []int) string {
	seen := map[int]bool{}
	var out []string
	for _, d := range days {
		if d < 1 || d > 7 || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, fmt.Sprintf("%d", d))
	}
	return strings.Join(out, ",")
}
