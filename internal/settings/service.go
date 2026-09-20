package settings

import (
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strconv"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/mailer"
	"k_cockpit/internal/model"
)

// Source 是设置项取值的来源（R-001）。
type Source string

// 取值来源，优先级从高到低。
const (
	// SourceEnv 环境变量：部署者的意图，界面修改不得覆盖。
	SourceEnv Source = "env"
	// SourceSetting 面板设置：用户在界面上显式设置过。
	SourceSetting Source = "setting"
	// SourceDefault 代码中声明的默认值。
	SourceDefault Source = "default"
)

// Item 是一个设置项的当前状态。
type Item struct {
	Spec

	// Value 是当前生效值。
	//
	// 敏感项返回**空字符串**并置 IsSet（R-009）：明文出过一次就再也
	// 收不回来（浏览器缓存、日志、截图），而界面上只需要知道「有没有设」。
	Value string `json:"value"`
	IsSet bool   `json:"is_set,omitempty"`

	Source Source `json:"source"`

	// Locked 表示该项被环境变量锁定，界面必须只读（R-002）。
	Locked bool `json:"locked"`
	// LockedBy 是锁定的环境变量名，界面据此告诉用户「去哪儿改」。
	LockedBy string `json:"locked_by,omitempty"`

	// CanRollback 表示存在可回滚的前值。
	CanRollback bool       `json:"can_rollback"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

// UpdateResult 是单项更新的结果（R-006：部分成功）。
type UpdateResult struct {
	Key     string `json:"key"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// 单项更新的三种状态。
const (
	// StatusApplied 已生效。
	StatusApplied = "applied"
	// StatusFailed 失败，值未改变。
	StatusFailed = "failed"
	// StatusRolledBack 应用失败且已回滚到变更前的值（R-007）。
	StatusRolledBack = "rolled_back"
)

// Applier 是设置项的应用钩子。
//
// 大多数设置项**不需要**它：它们的值是从库里读出来的，「写入成功」即
// 「生效」。需要它的是那些「写入之后还得验证确实能生效」的项——例如改
// 监听端口要先确认端口可用。这类项的失败必须触发回滚（R-007），否则
// 用户会得到一个「设置成功但服务起不来」的局面。
type Applier interface {
	// Apply 应用一项设置。返回错误表示应用失败。
	Apply(ctx context.Context, key, value string) error
}

// Service 提供系统设置读写。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	// env 读取环境变量，可替换以便测试注入。
	env func(string) (string, bool)
	// appliers 是按 key 注册的应用钩子，可为空。
	appliers map[string]Applier
}

// NewService 构造设置服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{
		db:       db,
		audit:    recorder,
		env:      os.LookupEnv,
		appliers: map[string]Applier{},
	}
}

// RegisterApplier 为某个设置项注册应用钩子。
func (s *Service) RegisterApplier(key string, applier Applier) {
	if s.appliers == nil {
		s.appliers = map[string]Applier{}
	}
	s.appliers[key] = applier
}

// List 返回设置项清单与当前生效值。
func (s *Service) List(ctx context.Context) ([]Item, []GroupInfo, error) {
	stored, err := s.loadStored(ctx)
	if err != nil {
		return nil, nil, err
	}

	specs := Specs()
	items := make([]Item, 0, len(specs))
	for _, spec := range specs {
		items = append(items, s.resolve(spec, stored[spec.Key]))
	}
	return items, Groups(), nil
}

// Get 返回单个设置项。
func (s *Service) Get(ctx context.Context, key string) (*Item, error) {
	spec, ok := SpecOf(key)
	if !ok {
		return nil, api.NotFound("设置项不存在")
	}

	var row model.SystemSetting
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		item := s.resolve(spec, nil)
		return &item, nil
	case err != nil:
		log.Printf("[settings] 查询设置失败: %v", err)
		return nil, api.Internal()
	}

	item := s.resolve(spec, &row)
	return &item, nil
}

// Update 批量更新设置，逐项返回结果（R-006）。
//
// **部分成功**而不是全有全无：一次改五项、其中一项被环境变量锁定时，
// 原子语义会让用户不知道哪些生效了、哪些没有，只能逐项重试去试探。
func (s *Service) Update(
	ctx context.Context, changes map[string]string, operatorID int64, operatorName, clientIP string,
) ([]UpdateResult, error) {
	if len(changes) == 0 {
		return nil, api.InvalidParameter("没有需要更新的设置项")
	}

	// 结果按**稳定的顺序**返回，便于前端逐项对应。map 的迭代顺序是随机的，
	// 直接遍历会让同一批改动的返回顺序每次都不同。
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	results := make([]UpdateResult, 0, len(keys))
	for _, key := range keys {
		results = append(results, s.updateOne(ctx, key, changes[key], operatorID, operatorName, clientIP))
	}
	return results, nil
}

// updateOne 更新单项。
func (s *Service) updateOne(
	ctx context.Context, key, value string, operatorID int64, operatorName, clientIP string,
) UpdateResult {
	spec, ok := SpecOf(key)
	if !ok {
		return UpdateResult{Key: key, Status: StatusFailed, Message: "设置项不存在"}
	}

	// 被环境变量锁定：**拒绝**而不是「接受但不生效」（R-002）。
	// 后者比直接拒绝更糟——用户以为改成功了，直到某天发现系统行为与设置不符。
	if _, locked := s.env(spec.EnvVar); locked {
		return UpdateResult{
			Key:     key,
			Status:  StatusFailed,
			Message: "该项由环境变量 " + spec.EnvVar + " 指定，请在部署配置中修改",
		}
	}

	if msg := validate(spec, value); msg != "" {
		return UpdateResult{Key: key, Status: StatusFailed, Message: msg}
	}

	// 敏感项留空表示「保持原值」。
	//
	// 界面拿不到明文（R-009），因此整张表单回传时密码那一栏必然是空的。
	// 若按普通字段处理，「改一个端口」就会顺手把密码清空，而用户要等下次
	// 发信失败才发现——那已经是很久以后的事了。
	if spec.Secret && value == "" {
		return UpdateResult{Key: key, Status: StatusApplied, Message: "留空，保持原有值不变"}
	}

	before, err := s.writeValue(ctx, spec, value, operatorID)
	if err != nil {
		return UpdateResult{Key: key, Status: StatusFailed, Message: "保存失败"}
	}

	// 应用钩子：没有注册的项视为「写入即生效」。
	if applier, ok := s.appliers[key]; ok && spec.Apply == ApplyImmediate {
		if err := applier.Apply(ctx, key, value); err != nil {
			log.Printf("[settings] 应用 %s 失败: %v", key, err)

			// 回滚到变更前的值（R-007）。不回滚的话，用户面对的是一个
			// 「设置成功但服务异常」的局面，而他没有明显的补救手段。
			if rbErr := s.restoreValue(ctx, spec, before, operatorID); rbErr != nil {
				log.Printf("[settings] 回滚 %s 失败: %v", key, rbErr)
			}
			s.record(ctx, operatorID, operatorName, clientIP, key, spec, "settings.update", before, value, false, StatusRolledBack)
			return UpdateResult{
				Key:     key,
				Status:  StatusRolledBack,
				Message: "应用失败，已回滚到变更前的值",
			}
		}
	}

	s.record(ctx, operatorID, operatorName, clientIP, key, spec, "settings.update", before, value, true, StatusApplied)
	return UpdateResult{Key: key, Status: StatusApplied}
}

// Rollback 把设置项回滚到最近一次变更前的值（API-038）。
func (s *Service) Rollback(
	ctx context.Context, key string, operatorID int64, operatorName, clientIP string,
) (*Item, error) {
	spec, ok := SpecOf(key)
	if !ok {
		return nil, api.NotFound("设置项不存在")
	}
	if !spec.Rollbackable {
		return nil, api.ValidationFailed("该设置项不支持回滚")
	}
	if _, locked := s.env(spec.EnvVar); locked {
		return nil, api.ValidationFailed("该项由环境变量 " + spec.EnvVar + " 指定，无法在面板中回滚")
	}

	var row model.SystemSetting
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, api.ValidationFailed("该项没有可回滚的变更记录")
	}
	if err != nil {
		log.Printf("[settings] 查询设置失败: %v", err)
		return nil, api.Internal()
	}
	if row.PreviousValue == nil {
		return nil, api.ValidationFailed("该项没有可回滚的变更记录")
	}

	// 回滚本身也是一次变更：它同样把当前值写入 previous_value，
	// 因此「回滚错了」还能再回滚回来。不这么做的话，一次误回滚会让原值
	// 永久消失——而它本可以被简单地恢复。
	restore := *row.PreviousValue
	before, err := s.writeValue(ctx, spec, restore, operatorID)
	if err != nil {
		return nil, err
	}

	s.record(ctx, operatorID, operatorName, clientIP, key, spec, "settings.rollback", before, restore, true, StatusApplied)

	return s.Get(ctx, key)
}

// MailConfig 返回当前生效的 SMTP 配置。
//
// 放在这里而不是让发信方自己读环境变量，是为了让「环境变量 > 面板设置 >
// 默认值」的判定只有一份（R-001）：否则界面显示的锁定状态与实际发信用的值
// 可能来自不同来源，而两者不一致时没有任何提示——用户只会看到邮件发不出去。
func (s *Service) MailConfig(ctx context.Context) (mailer.Config, error) {
	stored, err := s.loadStored(ctx)
	if err != nil {
		return mailer.Config{}, err
	}

	// 这里取的是**明文**：敏感项的脱敏只发生在对外响应里（R-009），
	// 服务端自己要用这个值去登录 SMTP 服务器。
	get := func(key string) string {
		spec, ok := SpecOf(key)
		if !ok {
			return ""
		}
		if v, ok := s.env(spec.EnvVar); ok && v != "" {
			return v
		}
		if row, ok := stored[key]; ok && row.Value != nil {
			return *row.Value
		}
		return spec.Default
	}

	port, _ := strconv.Atoi(get(KeySMTPPort))
	timeout, _ := strconv.Atoi(get(KeySMTPTimeout))
	return mailer.Config{
		Host:     get(KeySMTPHost),
		Port:     port,
		Username: get(KeySMTPUsername),
		Password: get(KeySMTPPassword),
		Security: get(KeySMTPSecurity),
		From:     get(KeySMTPFrom),
		FromName: get(KeySMTPFromName),
		Timeout:  time.Duration(timeout) * time.Second,
	}, nil
}

// resolve 按优先级计算一个设置项的当前状态（R-001）。
func (s *Service) resolve(spec Spec, stored *model.SystemSetting) Item {
	item := Item{Spec: spec, Source: SourceDefault, Value: spec.Default}

	// 环境变量优先级最高，且**锁定**该项（R-002）。
	if raw, ok := s.env(spec.EnvVar); ok && raw != "" {
		item.Source = SourceEnv
		item.Locked = true
		item.LockedBy = spec.EnvVar
		item.Value = raw
	} else if stored != nil && stored.Value != nil {
		item.Source = SourceSetting
		item.Value = *stored.Value
		item.UpdatedAt = &stored.UpdatedAt
	}

	if stored != nil && stored.PreviousValue != nil {
		item.CanRollback = true
	}

	// 敏感项不回传明文（R-009）：只告诉界面「已设置」。
	if spec.Secret {
		item.IsSet = item.Value != ""
		item.Value = ""
	}
	return item
}

// loadStored 一次性读出所有已存储的值。
func (s *Service) loadStored(ctx context.Context) (map[string]*model.SystemSetting, error) {
	var rows []model.SystemSetting
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		log.Printf("[settings] 读取设置失败: %v", err)
		return nil, api.Internal()
	}

	out := make(map[string]*model.SystemSetting, len(rows))
	for i := range rows {
		out[rows[i].Key] = &rows[i]
	}
	return out, nil
}

// writeValue 写入新值，并把旧值存入 previous_value，返回旧值。
func (s *Service) writeValue(
	ctx context.Context, spec Spec, value string, operatorID int64,
) (*string, error) {
	now := time.Now()

	var existing model.SystemSetting
	err := s.db.WithContext(ctx).Where("key = ?", spec.Key).First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		row := model.SystemSetting{
			Key:       spec.Key,
			Value:     &value,
			UpdatedBy: &operatorID,
			UpdatedAt: now,
			CreatedAt: now,
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[settings] 创建设置失败 key=%s: %v", spec.Key, err)
			return nil, api.Internal()
		}
		return nil, nil

	case err != nil:
		log.Printf("[settings] 查询设置失败: %v", err)
		return nil, api.Internal()
	}

	before := existing.Value
	updates := map[string]any{
		"value":      value,
		"updated_by": operatorID,
		"updated_at": now,
	}
	// 只有**值真的变了**才刷新 previous_value：不改值地重复提交（例如
	// 前端把整张表单回传）会把回滚点推进到一个没有意义的位置，
	// 让「回滚」看起来什么都没做。
	if before == nil || *before != value {
		updates["previous_value"] = before
	}

	if err := s.db.WithContext(ctx).Model(&model.SystemSetting{}).
		Where("key = ?", spec.Key).Updates(updates).Error; err != nil {
		log.Printf("[settings] 更新设置失败 key=%s: %v", spec.Key, err)
		return nil, api.Internal()
	}
	return before, nil
}

// restoreValue 把值恢复到指定内容（用于应用失败后的回滚）。
func (s *Service) restoreValue(ctx context.Context, spec Spec, before *string, operatorID int64) error {
	if before == nil {
		// 此前没有记录：删除这行即可回到默认值。
		err := s.db.WithContext(ctx).Where("key = ?", spec.Key).
			Delete(&model.SystemSetting{}).Error
		if err != nil {
			return err
		}
		return nil
	}

	return s.db.WithContext(ctx).Model(&model.SystemSetting{}).
		Where("key = ?", spec.Key).
		Updates(map[string]any{
			"value":      *before,
			"updated_by": operatorID,
			"updated_at": time.Now(),
		}).Error
}

// record 写审计（R-010）。
func (s *Service) record(
	ctx context.Context, operatorID int64, operatorName, clientIP, key string,
	spec Spec, action string, before *string, after string, success bool, result string,
) {
	if s.audit == nil {
		return
	}

	params := map[string]any{"key": key, "result": result}
	afterState := map[string]any{"source": string(SourceSetting)}

	// 敏感值**一律不入审计**（R-011）：审计表的读取面比业务表宽，
	// 把凭据写进去等于给它开了一条额外的泄漏路径。
	if !spec.Secret {
		if before != nil {
			params["old_value"] = *before
		}
		afterState["value"] = after
	} else {
		params["old_value"] = "[已脱敏]"
		afterState["value"] = "[已脱敏]"
	}

	s.audit.Record(ctx, audit.Entry{
		OperatorID:   operatorID,
		OperatorName: operatorName,
		ResourceType: "setting",
		ResourceName: key,
		Action:       action,
		Params:       params,
		AfterState:   afterState,
		Success:      success,
		ClientIP:     clientIP,
	})
}

// validate 校验值的类型与范围。
func validate(spec Spec, value string) string {
	switch spec.Kind {
	case KindInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return "必须是整数"
		}
		if spec.MinValue != nil && n < *spec.MinValue {
			return "不能小于 " + strconv.Itoa(*spec.MinValue)
		}
		if spec.MaxValue != nil && n > *spec.MaxValue {
			return "不能大于 " + strconv.Itoa(*spec.MaxValue)
		}
	case KindBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return "必须是布尔值"
		}
	case KindSelect:
		for _, opt := range spec.Options {
			if opt.Value == value {
				return ""
			}
		}
		return "取值不在允许范围内"
	case KindString:
		if len(value) > 255 {
			return "长度不能超过 255 个字符"
		}
	}
	return ""
}

// --- Provider：供其他模块读取设置 ---

// Provider 提供设置项的运行时取值。
//
// 各业务模块依赖这个窄接口而不是整个 Service：它们只需要读值，不需要
// 更新与回滚能力。接口窄了，依赖关系也就清楚了。
type Provider interface {
	Int(key string, fallback int) int
	Bool(key string, fallback bool) bool
	String(key string, fallback string) string
}

// Int 读取整数设置；未设置或非法时返回 fallback。
func (s *Service) Int(key string, fallback int) int {
	spec, ok := SpecOf(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(s.currentValue(spec))
	if err != nil {
		return fallback
	}
	return n
}

// Bool 读取布尔设置；未设置或非法时返回 fallback。
func (s *Service) Bool(key string, fallback bool) bool {
	spec, ok := SpecOf(key)
	if !ok {
		return fallback
	}
	v, err := strconv.ParseBool(s.currentValue(spec))
	if err != nil {
		return fallback
	}
	return v
}

// String 读取字符串设置；未设置时返回 fallback。
func (s *Service) String(key string, fallback string) string {
	spec, ok := SpecOf(key)
	if !ok {
		return fallback
	}
	if v := s.currentValue(spec); v != "" {
		return v
	}
	return fallback
}

// currentValue 按优先级取当前值（环境变量 > 库 > 默认值）。
//
// 这里**不缓存**：一次查询就是一次主键查找，而缓存会引入「改了设置但
// 读到的还是旧值」这类问题——那正是本规格要消灭的现象。
func (s *Service) currentValue(spec Spec) string {
	if raw, ok := s.env(spec.EnvVar); ok && raw != "" {
		return raw
	}

	var row model.SystemSetting
	err := s.db.Where("key = ?", spec.Key).First(&row).Error
	if err == nil && row.Value != nil {
		return *row.Value
	}
	return spec.Default
}
