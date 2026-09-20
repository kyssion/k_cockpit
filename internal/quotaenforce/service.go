// Package quotaenforce 实现资源配额的超限判定与处置（F-4-10 / F-1-08）。
//
// 它补上的是「采了但不用」的那一步：`traffic_stat_daily` 与
// `vm_runtime_daily` 两张表一直在按天累计，而**没有任何地方读它们做判断**。
// 数据就在库里，缺的只是"拿它做判断"。
//
// 三条设计决定：
//
//  1. **用量不另存一份。** 判定时从统计表现算。再存一份会出现两个数据源，
//     而两者对不上时无法判断哪个是真的——那种分歧不会报错，只会让用户
//     看到的数字与处置依据不一致。
//
//  2. **默认可用的处置是限速，不是断网。** 断网会让业务直接中断；选它的
//     人应当是有意为之，而不是"没注意默认值"。
//
//  3. **跨月必须重置。** 不重置的话，上个月被限速的用户在新的一月里仍然
//     限着，而那时他的用量是 0、界面上显示"已超限"，没有任何地方能解释。
package quotaenforce

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// bytesPerGB 与 hoursPerSecond 用于把统计值换算成配额的计量单位。
const (
	bytesPerGB     = 1024 * 1024 * 1024
	secondsPerHour = 3600
)

// Service 提供配额的设置、查询与处置。
type Service struct {
	db    *gorm.DB
	queue *task.Queue
	agent agent.Client
	audit *audit.Recorder
	now   func() time.Time
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, queue *task.Queue, client agent.Client, recorder *audit.Recorder,
) *Service {
	return &Service{db: db, queue: queue, agent: client, audit: recorder, now: time.Now}
}

// View 是一条配额的对外视图。
type View struct {
	ID     int64 `json:"id"`
	NodeID int64 `json:"node_id"`
	UserID int64 `json:"user_id"`
	// Username 让用户能对上是哪个人——只给 ID 还得再去查一次用户列表。
	Username string `json:"username,omitempty"`

	Dimension  string `json:"dimension"`
	DimLabel   string `json:"dimension_label"`
	DimUnit    string `json:"dimension_unit"`
	LimitValue int64  `json:"limit_value"`
	Action     string `json:"action"`

	// UsedValue 是**当前周期的实际用量**，判定时现算。
	UsedValue int64 `json:"used_value"`
	// UsedPercent 是占用百分比（未限时按上限算）。
	UsedPercent int `json:"used_percent"`

	Status   string `json:"status"`
	Period   string `json:"period,omitempty"`
	WarnedAt string `json:"warned_at,omitempty"`
	// LimitedAt 是**处置发生的时刻**，而不只是"超了"。
	//
	// 二者不同：用户问"我的网什么时候开始变慢的"，答案在这里；而"什么时候
	// 超的"要去看用量曲线。把处置时刻记下来，那个问题才有唯一答案。
	LimitedAt string `json:"limited_at,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// Request 是设置配额的请求。
type Request struct {
	NodeID     int64
	UserID     int64
	Dimension  string
	LimitValue int64
	Action     string
}

// List 返回节点上的配额，附当前周期的实际用量。
func (s *Service) List(ctx context.Context, nodeID int64) ([]View, error) {
	var rows []model.ResourceQuota
	if err := s.db.WithContext(ctx).
		Where("node_id = ?", nodeID).
		Order("user_id ASC, dimension ASC").Find(&rows).Error; err != nil {
		log.Printf("[quotaenforce] 查询配额失败: %v", err)
		return nil, api.Internal()
	}

	period := s.currentPeriod()
	names := s.usernames(ctx, rows)
	// 每个用户只算一次用量（同一用户的三个维度共用同一份统计）。
	usageCache := map[int64]map[string]int64{}

	out := make([]View, 0, len(rows))
	for i := range rows {
		q := &rows[i]
		if usageCache[q.UserID] == nil {
			u, err := s.usageOf(ctx, q.NodeID, q.UserID, period)
			if err != nil {
				return nil, err
			}
			usageCache[q.UserID] = u
		}
		v := toView(q, usageCache[q.UserID][q.Dimension], period)
		v.Username = names[q.UserID]
		out = append(out, v)
	}
	return out, nil
}

// Set 设置或更新一条配额。
//
// 改上限时**清掉处置状态**：把上限从 100 GB 提到 200 GB 之后，那条"已限速"
// 的记录就不再成立——不清的话，用户明明已经合规，网络却还是慢的，而界面上
// 显示"已超限"。
func (s *Service) Set(
	ctx context.Context, req Request, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	if !model.ValidQuotaDim(req.Dimension) {
		return nil, api.InvalidParameter("未知的配额维度")
	}
	if req.LimitValue < 0 {
		return nil, api.InvalidParameter("上限不能为负")
	}
	if req.Action == "" {
		req.Action = model.QuotaActionThrottle
	}
	if !model.ValidQuotaAction(req.Action) {
		return nil, api.InvalidParameter("处置方式只能是限速或断网")
	}
	// 现在不存在的用户不该有配额：一条指向空用户的配额，界面上无法显示
	// 是谁，而判定时也永远不会命中。
	var user model.User
	if err := s.db.WithContext(ctx).Select("id", "username").
		First(&user, req.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("用户不存在")
		}
		return nil, api.Internal()
	}

	period := s.currentPeriod()
	var row model.ResourceQuota
	err := s.db.WithContext(ctx).
		Where("node_id = ? AND user_id = ? AND dimension = ?",
			req.NodeID, req.UserID, req.Dimension).First(&row).Error

	updates := map[string]any{
		"limit_value": req.LimitValue,
		"action":      req.Action,
		// 改上限即清处置：见方法注释。
		"status":     model.QuotaStatusOK,
		"period":     period,
		"warned_at":  nil,
		"limited_at": nil,
		"detail":     nil,
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = model.ResourceQuota{
			NodeID: req.NodeID, UserID: req.UserID, Dimension: req.Dimension,
			LimitValue: req.LimitValue, Action: req.Action,
			Status: model.QuotaStatusOK, Period: &period,
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[quotaenforce] 创建配额失败: %v", err)
			return nil, api.Internal()
		}
	} else if err != nil {
		log.Printf("[quotaenforce] 查询配额失败: %v", err)
		return nil, api.Internal()
	} else {
		if err := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
			Where("id = ?", row.ID).Updates(updates).Error; err != nil {
			log.Printf("[quotaenforce] 更新配额失败: %v", err)
			return nil, api.Internal()
		}
		// 清 nil 字段需要单独一条语句（GORM 的 map Updates 会跳过 nil）。
		if err := s.clearEnforcement(ctx, row.ID); err != nil {
			return nil, err
		}
		if err := s.db.WithContext(ctx).First(&row, row.ID).Error; err != nil {
			return nil, api.Internal()
		}
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: req.NodeID, ResourceType: "resource_quota",
			ResourceID: row.ID, ResourceName: user.Username,
			Action: "quota.set",
			Params: map[string]any{
				"dimension":    req.Dimension,
				"limit_value":  req.LimitValue,
				"action":       req.Action,
				"period":       period,
				"clear_notice": "提高上限会清除该维度的处置状态",
			},
			Success: true, ClientIP: clientIP,
		})
	}

	usage, err := s.usageOf(ctx, req.NodeID, req.UserID, period)
	if err != nil {
		return nil, err
	}
	view := toView(&row, usage[req.Dimension], period)
	view.Username = user.Username
	return &view, nil
}

// Delete 删除一条配额（回到「不限」）。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	var row model.ResourceQuota
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("配额不存在")
		}
		return api.Internal()
	}
	if err := s.db.WithContext(ctx).Delete(&model.ResourceQuota{}, row.ID).Error; err != nil {
		return api.Internal()
	}
	// 删除配额意味着**回到不限**，因此必须撤掉已经生效的处置——否则用户
	// 在界面上看到"没有配额"，而节点上那条限速规则还挂着。
	s.revert(ctx, &row)

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: row.NodeID, ResourceType: "resource_quota",
			ResourceID: row.ID, Action: "quota.delete",
			Params:  map[string]any{"dimension": row.Dimension, "was_limited": row.Limited()},
			Success: true, ClientIP: clientIP,
		})
	}
	return nil
}

// ResetUsage 清空某条配额**当前周期**的累计用量，并撤销已生效的处置。
//
// 与"改上限"清状态的区别是本质的：改上限时累计确实该从那一刻重新算起，
// 而这里**累计本身**被清零。只清状态不清累计的话，下一次评估（五分钟内）
// 会立刻把它再次判为超限，用户看到的将是"点了重置，五分钟后又被限速"——
// 比没有这个功能更让人困惑。
//
// 清的是当前周期：历史周期的行留着，它们是"上个月用了多少"的唯一证据，
// 而删除历史会让"这个用户一直超量"这件事无从追溯。
func (s *Service) ResetUsage(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	var row model.ResourceQuota
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("配额不存在")
		}
		log.Printf("[quotaenforce] 查询配额失败 id=%d: %v", id, err)
		return nil, api.Internal()
	}

	period := s.currentPeriod()
	start, end := periodRange(period)

	// 两个维度共用一张累计表，因此按维度清：清流量不该顺手把运行时长也
	// 抹掉——那是另一个承诺。
	switch row.Dimension {
	case model.QuotaDimTrafficIn, model.QuotaDimTrafficOut:
		if err := s.db.WithContext(ctx).
			Where("owner_id = ? AND date >= ? AND date < ?", row.UserID, start, end).
			Delete(&model.TrafficStatDaily{}).Error; err != nil {
			log.Printf("[quotaenforce] 清理流量累计失败 user=%d: %v", row.UserID, err)
			return nil, api.Internal()
		}
	case model.QuotaDimRuntime:
		if err := s.db.WithContext(ctx).
			Where("owner_id = ? AND date >= ? AND date < ?", row.UserID, start, end).
			Delete(&model.VMRuntimeDaily{}).Error; err != nil {
			log.Printf("[quotaenforce] 清理运行时长累计失败 user=%d: %v", row.UserID, err)
			return nil, api.Internal()
		}
	}

	wasLimited := row.Limited()
	if err := s.reset(ctx, &row); err != nil {
		return nil, err
	}

	if s.audit != nil {
		s.audit.Record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: row.NodeID, ResourceType: "resource_quota",
			ResourceID: row.ID, Action: "quota.reset_usage",
			Params: map[string]any{
				"dimension": row.Dimension, "period": period, "was_limited": wasLimited,
			},
			Success: true, ClientIP: clientIP,
		})
	}
	return s.View(ctx, row.ID)
}

// View 返回单条配额的当前状态（含用量与百分比）。
func (s *Service) View(ctx context.Context, id int64) (*View, error) {
	var row model.ResourceQuota
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("配额不存在")
		}
		return nil, api.Internal()
	}
	used := s.usageOfQuiet(ctx, &row)[row.Dimension]
	view := toView(&row, used, s.currentPeriod())
	view.Username = s.usernames(ctx, []model.ResourceQuota{row})[row.UserID]
	return &view, nil
}

// usageOfQuiet 取用量但**不因失败中断**：展示路径不该因为一次统计失败
// 就把整个页面变成错误页，用量那一列写成 0 即可（界面上会标注"。
func (s *Service) usageOfQuiet(ctx context.Context, q *model.ResourceQuota) map[string]int64 {
	period := s.currentPeriod()
	used, err := s.usageOf(ctx, q.NodeID, q.UserID, period)
	if err != nil {
		log.Printf("[quotaenforce] 读取用量失败 id=%d: %v", q.ID, err)
		return map[string]int64{}
	}
	return used
}

// Evaluate 对节点上的全部配额做一次判定并按需处置。
//
// 它由采样器周期调用（与其他周期工作一样登记在调度器注册表里），因此
// **幂等**：状态没变时不产生任何动作，也不重复下发。
func (s *Service) Evaluate(ctx context.Context, nodeID int64) (int, error) {
	period := s.currentPeriod()

	var rows []model.ResourceQuota
	if err := s.db.WithContext(ctx).Where("node_id = ?", nodeID).Find(&rows).Error; err != nil {
		return 0, api.Internal()
	}

	changed := 0
	usageCache := map[int64]map[string]int64{}
	for i := range rows {
		q := &rows[i]
		if q.Unlimited() {
			// 上限为 0 = 不限。**已处置的要撤销**：用户可能刚刚把上限改成
			// "不限"，而节点上那条限速还挂着。
			if q.Status != model.QuotaStatusOK {
				if err := s.reset(ctx, q); err != nil {
					return changed, err
				}
				changed++
			}
			continue
		}

		if usageCache[q.UserID] == nil {
			u, err := s.usageOf(ctx, q.NodeID, q.UserID, period)
			if err != nil {
				return changed, err
			}
			usageCache[q.UserID] = u
		}
		used := usageCache[q.UserID][q.Dimension]

		// 跨月：周期变了就把状态清掉再看。**先重置再判定**——直接用旧状态
		// 比较会让上个月被限速的用户在新的一月里继续限着。
		if q.Period == nil || *q.Period != period {
			if err := s.reset(ctx, q); err != nil {
				return changed, err
			}
			changed++
		}

		want, detail := s.decide(q, used, period)
		if want == q.Status {
			continue
		}
		if err := s.apply(ctx, q, want, used, detail); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// decide 纯函数式地给出应有的状态与说明，便于单独测试。
func (s *Service) decide(q *model.ResourceQuota, used int64, period string) (string, string) {
	if q.Unlimited() {
		return model.QuotaStatusOK, ""
	}
	if used >= q.LimitValue {
		return model.QuotaStatusLimited, fmt.Sprintf(
			"周期 %s 内已用 %d %s，超出上限 %d %s，已按「%s」处置",
			period, used, model.QuotaDimUnit(q.Dimension),
			q.LimitValue, model.QuotaDimUnit(q.Dimension), actionLabel(q.Action))
	}
	if used*100 >= q.LimitValue*model.WarnRatioPercent {
		return model.QuotaStatusWarned, fmt.Sprintf(
			"周期 %s 内已用 %d %s，达到上限 %d %s 的 %d%%",
			period, used, model.QuotaDimUnit(q.Dimension),
			q.LimitValue, model.QuotaDimUnit(q.Dimension), used*100/q.LimitValue)
	}
	return model.QuotaStatusOK, ""
}

// apply 落库并下发处置。
func (s *Service) apply(
	ctx context.Context, q *model.ResourceQuota, status string, used int64, detail string,
) error {
	now := s.now()
	updates := map[string]any{
		"status": status,
		"period": s.currentPeriod(),
		"detail": detail,
	}
	// **时刻只在首次进入该状态时写**。
	//
	// 每轮都覆盖的话，"什么时候被限速的"会变成"最后一次判定的时间"——
	// 而用户问的恰恰是前者，那个值应当是稳定的。
	if status == model.QuotaStatusWarned && q.WarnedAt == nil {
		updates["warned_at"] = now
	}
	if status == model.QuotaStatusLimited && q.LimitedAt == nil {
		updates["limited_at"] = now
	}
	if err := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Where("id = ?", q.ID).Updates(updates).Error; err != nil {
		log.Printf("[quotaenforce] 更新配额状态失败 id=%d: %v", q.ID, err)
		return api.Internal()
	}

	// 下发到节点。
	if status == model.QuotaStatusLimited {
		s.enqueue(ctx, q, true)
	} else {
		// 从 limited 回到 ok/warned：必须**撤销**节点上的处置。
		// 不撤销的话，用户在界面上看到"已恢复"，而网络还是慢的。
		s.enqueue(ctx, q, false)
	}
	return nil
}

func (s *Service) reset(ctx context.Context, q *model.ResourceQuota) error {
	period := s.currentPeriod()
	if err := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Where("id = ?", q.ID).
		Updates(map[string]any{
			"status": model.QuotaStatusOK, "period": period, "detail": nil,
		}).Error; err != nil {
		return api.Internal()
	}
	if err := s.clearEnforcement(ctx, q.ID); err != nil {
		return err
	}
	// 原本处于处置中的，要把节点上的规则撤掉。
	if q.Limited() {
		s.enqueue(ctx, q, false)
	}
	q.Status = model.QuotaStatusOK
	q.Period = &period
	q.WarnedAt = nil
	q.LimitedAt = nil
	return nil
}

// clearEnforcement 清空 warned_at / limited_at。
//
// 单独一条语句：GORM 的 `Updates(map)` 会跳过值为 nil 的项，而这里恰恰
// 必须写成 NULL。
func (s *Service) clearEnforcement(ctx context.Context, id int64) error {
	if err := s.db.WithContext(ctx).Model(&model.ResourceQuota{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"warned_at":  gorm.Expr("NULL"),
			"limited_at": gorm.Expr("NULL"),
		}).Error; err != nil {
		log.Printf("[quotaenforce] 清空处置时刻失败 id=%d: %v", id, err)
		return api.Internal()
	}
	return nil
}

// revert 撤销一条配额在节点上的处置（删除配额时用）。
func (s *Service) revert(ctx context.Context, q *model.ResourceQuota) {
	if q.Limited() {
		s.enqueue(ctx, q, false)
	}
}

// enqueue 下发或撤销处置。
func (s *Service) enqueue(ctx context.Context, q *model.ResourceQuota, enforce bool) {
	if s.queue == nil {
		return
	}
	_, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskQuotaEnforce,
		NodeID:       q.NodeID,
		ResourceType: "resource_quota",
		ResourceID:   q.ID,
		ResourceName: q.Dimension,
		Params: map[string]any{
			"quota_id": q.ID, "user_id": q.UserID,
			"dimension": q.Dimension,
			"enforce":   enforce,
			"action":    q.Action,
		},
	})
	if err != nil {
		// 入队失败**不改状态**的时机已经过去了（状态先落库），因此这里
		// 只能记日志。下一次判定会重试——它天然幂等。
		log.Printf("[quotaenforce] 处置任务入队失败 id=%d: %v", q.ID, err)
	}
}

// usageOf 从统计表现算某用户在当前周期的用量（单位与配额一致）。
func (s *Service) usageOf(
	ctx context.Context, nodeID, userID int64, period string,
) (map[string]int64, error) {
	start, end := periodRange(period)
	out := map[string]int64{
		model.QuotaDimTrafficIn: 0, model.QuotaDimTrafficOut: 0, model.QuotaDimRuntime: 0,
	}

	// 流量：按天累计表求和。**按 UTC 分桶**——与运行时长同一口径，
	// 否则跨时区结算会对不上。
	// 别名**不能用 in / out**：它们是 SQL 保留字，SQLite 会直接报语法错误。
	// 这类错误的消息（near "in": syntax error）指向的是 SQL 结构，而不是
	// "你把保留字当别名了"，因此值得在代码里留一句。
	var traffic struct {
		BytesInTotal  int64
		BytesOutTotal int64
	}
	if err := s.db.WithContext(ctx).Model(&model.TrafficStatDaily{}).
		Select("COALESCE(SUM(bytes_in),0) as bytes_in_total, "+
			"COALESCE(SUM(bytes_out),0) as bytes_out_total").
		Where("owner_id = ? AND date >= ? AND date < ?", userID, start, end).
		Scan(&traffic).Error; err != nil {
		log.Printf("[quotaenforce] 统计流量失败: %v", err)
		return nil, api.Internal()
	}
	// 换算成 GB：配额以 GB 计，而表里是字节。
	out[model.QuotaDimTrafficIn] = traffic.BytesInTotal / bytesPerGB
	out[model.QuotaDimTrafficOut] = traffic.BytesOutTotal / bytesPerGB

	var runtimeSeconds struct {
		Total int64
	}
	if err := s.db.WithContext(ctx).Model(&model.VMRuntimeDaily{}).
		Select("COALESCE(SUM(seconds),0) as total").
		Where("owner_id = ? AND date >= ? AND date < ?", userID, start, end).
		Scan(&runtimeSeconds).Error; err != nil {
		log.Printf("[quotaenforce] 统计运行时长失败: %v", err)
		return nil, api.Internal()
	}
	out[model.QuotaDimRuntime] = runtimeSeconds.Total / secondsPerHour

	_ = nodeID // 统计表按 owner 维度，节点在配额行上
	return out, nil
}

func (s *Service) currentPeriod() string {
	return s.now().UTC().Format("2006-01")
}

// periodRange 返回某个周期（YYYY-MM）的起止时刻（UTC，左闭右开）。
func periodRange(period string) (time.Time, time.Time) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		now := time.Now().UTC()
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
			time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return start, start.AddDate(0, 1, 0)
}

func (s *Service) usernames(ctx context.Context, rows []model.ResourceQuota) map[int64]string {
	ids := make([]int64, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].UserID)
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var users []model.User
	if err := s.db.WithContext(ctx).Select("id", "username").
		Where("id IN ?", ids).Find(&users).Error; err != nil {
		log.Printf("[quotaenforce] 查询用户名失败: %v", err)
		return out
	}
	for _, u := range users {
		out[u.ID] = u.Username
	}
	return out
}

func toView(q *model.ResourceQuota, used int64, period string) View {
	v := View{
		ID: q.ID, NodeID: q.NodeID, UserID: q.UserID,
		Dimension: q.Dimension, DimLabel: model.QuotaDimLabel(q.Dimension),
		DimUnit:    model.QuotaDimUnit(q.Dimension),
		LimitValue: q.LimitValue, Action: q.Action,
		UsedValue: used, Status: q.Status,
	}
	if q.Period != nil {
		v.Period = *q.Period
	} else {
		v.Period = period
	}
	if q.WarnedAt != nil {
		v.WarnedAt = q.WarnedAt.Format(time.RFC3339)
	}
	if q.LimitedAt != nil {
		v.LimitedAt = q.LimitedAt.Format(time.RFC3339)
	}
	if q.Detail != nil {
		v.Detail = *q.Detail
	}
	if q.LimitValue > 0 {
		v.UsedPercent = int(used * 100 / q.LimitValue)
	}
	return v
}

func actionLabel(a string) string {
	if a == model.QuotaActionBlock {
		return "断网"
	}
	return "限速"
}

// SortedDims 返回全部维度（稳定顺序），供界面渲染固定列。
func SortedDims() []string {
	out := []string{model.QuotaDimTrafficIn, model.QuotaDimTrafficOut, model.QuotaDimRuntime}
	sort.Strings(out)
	return out
}
