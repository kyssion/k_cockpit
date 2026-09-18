// Package accesscontrol 实现公网访问与开发模式开关（F-10-06）。
//
// 这一项**不是一个普通设置项**，尽管它看起来像。它有几个别的设置都没有的
// 性质，而每一条都影响设计：
//
//  1. **改它会切断你自己。** 用户正是从公网访问时关掉公网访问，那一刻他的
//     连接就断了——而界面还没来得及显示"已保存"。因此这个操作需要一个明确
//     的确认，且**拒绝的理由要说清会发生什么**。
//
//  2. **判断"是不是公网"要靠请求方地址**，而那个地址可能是代理的。判错了
//     方向相反：把代理地址当成公网，会让用户在一个其实安全的连接上被拦；
//     把公网当成内网，则会让他在真的会被切断时毫无准备。
//
//  3. **它是一道安全边界**，因此环境变量优先于面板设置——与 F-9-01 的
//     优先级规则一致。部署方在启动参数里关掉公网访问之后，面板上那个开关
//     不该能把它打开。
package accesscontrol

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// 设置键。
//
// 存进 system_setting 而不是新建一张表：它是两个布尔值，而"一张只有两列的
// 表"会让 schema 上多一处需要维护的东西。
const (
	KeyPublicEnabled = "access.public_enabled"
	KeyDevMode       = "access.dev_mode"
)

// Service 提供公网访问开关。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	// envLocked 为 true 表示这两项**由环境变量锁定**，面板改不动。
	//
	// 部署方在启动参数里关掉公网访问之后，面板上那个开关不该能把它打开
	// ——否则那道边界形同虚设。
	envLocked bool
	envPublic bool
	envDev    bool
}

// Options 是装配参数。
type Options struct {
	// EnvPublicEnabled / EnvDevMode 为 nil 表示未由环境变量指定。
	EnvPublicEnabled *bool
	EnvDevMode       *bool
}

// NewService 构造服务。
func NewService(db *gorm.DB, recorder *audit.Recorder, opts Options) *Service {
	s := &Service{db: db, audit: recorder}
	if opts.EnvPublicEnabled != nil || opts.EnvDevMode != nil {
		s.envLocked = true
		if opts.EnvPublicEnabled != nil {
			s.envPublic = *opts.EnvPublicEnabled
		}
		if opts.EnvDevMode != nil {
			s.envDev = *opts.EnvDevMode
		}
	}
	return s
}

// View 是当前状态。
type View struct {
	PublicEnabled bool `json:"public_enabled"`
	DevMode       bool `json:"dev_mode"`
	// EnvLocked 为 true 时界面上的开关应当禁用并说明原因。
	EnvLocked bool `json:"env_locked"`
	// CallerIsPublic 表示**当前这个请求**来自公网。
	//
	// 它决定了两件事：关掉公网访问会不会立刻切断本人，以及界面上要不要
	// 提前把这件事说出来。
	CallerIsPublic bool `json:"caller_is_public"`
	// CallerIP 是面板看到的地址（可能是代理的）。
	CallerIP string `json:"caller_ip"`
	// EffectedBy 说明当前值来自哪里（环境变量 / 面板）。
	EffectedBy string `json:"effected_by"`
}

// Get 返回当前状态。
func (s *Service) Get(ctx context.Context, callerIP string) (*View, error) {
	public, dev, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	v := &View{
		PublicEnabled:  public,
		DevMode:        dev,
		EnvLocked:      s.envLocked,
		CallerIP:       callerIP,
		CallerIsPublic: IsPublicIP(callerIP),
	}
	if s.envLocked {
		v.EffectedBy = "环境变量"
	} else {
		v.EffectedBy = "面板"
	}
	return v, nil
}

// Request 是修改请求。
type Request struct {
	PublicEnabled *bool
	DevMode       *bool
}

// Set 修改开关。
//
// `confirm` 为 false 且这次修改会切断调用方自己的连接时，**不执行、也不报错**，
// 而是把当前状态原样返回——与项目里其它"需要用户做决定"的操作同一套语义。
// 那不是一次失败，而是一个岔路口：他知道会发生什么，然后决定要不要继续。
func (s *Service) Set(
	ctx context.Context, req Request, confirm bool, callerIP string,
	v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	if s.envLocked {
		// **环境变量优先**。这与 F-9-01 的优先级规则一致，而且在这里尤其
		// 重要：部署方关掉公网访问之后，面板上那个开关不该能把它打开——
		// 那样的话"已在启动参数里关闭"只是一句建议。
		return nil, api.Conflict("该项由环境变量指定，面板无法修改（环境变量优先于面板设置）")
	}
	if req.PublicEnabled == nil && req.DevMode == nil {
		return nil, api.InvalidParameter("没有要修改的项")
	}

	current, _, err := s.read(ctx)
	if err != nil {
		return nil, err
	}

	// **关掉公网访问而调用方自己就在公网上**：这会立刻切断他。
	// 不确认就返回现状，而不是闷头执行。
	cutting := req.PublicEnabled != nil && !*req.PublicEnabled &&
		current && IsPublicIP(callerIP)
	if cutting && !confirm {
		view, err := s.Get(ctx, callerIP)
		if err != nil {
			return nil, err
		}
		return view, nil
	}

	if req.PublicEnabled != nil {
		if err := s.write(ctx, KeyPublicEnabled, boolStr(*req.PublicEnabled)); err != nil {
			return nil, err
		}
	}
	if req.DevMode != nil {
		if err := s.write(ctx, KeyDevMode, boolStr(*req.DevMode)); err != nil {
			return nil, err
		}
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "access_control", Action: "access_control.update",
		Params: map[string]any{
			"public_enabled": req.PublicEnabled,
			"dev_mode":       req.DevMode,
			// 「这次修改会不会切断调用方自己」要记下来：事后看审计时，
			// 一条"改了公网开关"和一个"人突然掉线了"能否对上，靠的就是它。
			"cut_own_connection": cutting,
			"caller_is_public":   IsPublicIP(callerIP),
		},
		Success: true, ClientIP: clientIP,
	})

	return s.Get(ctx, callerIP)
}

// --- 内部 ---

func (s *Service) read(ctx context.Context) (public, dev bool, err error) {
	if s.envLocked {
		// 环境变量给了值，它就是权威——**不再去读库**。读库再比较会让
		// "明明锁了却显示库里的旧值"这种不一致出现。
		return s.envPublic, s.envDev, nil
	}
	var rows []model.SystemSetting
	if e := s.db.WithContext(ctx).
		Where("key IN ?", []string{KeyPublicEnabled, KeyDevMode}).Find(&rows).Error; e != nil {
		log.Printf("[accesscontrol] 读取设置失败: %v", e)
		return false, false, api.Internal()
	}
	for i := range rows {
		val := ""
		if rows[i].Value != nil {
			val = *rows[i].Value
		}
		switch rows[i].Key {
		case KeyPublicEnabled:
			public = val == "true" || val == "1"
		case KeyDevMode:
			dev = val == "true" || val == "1"
		}
	}
	return public, dev, nil
}

func (s *Service) write(ctx context.Context, key, value string) error {
	var row model.SystemSetting
	err := s.db.WithContext(ctx).Where("key = ?", key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s.db.WithContext(ctx).Create(&model.SystemSetting{
			Key: key, Value: &value,
		}).Error
	}
	if err != nil {
		log.Printf("[accesscontrol] 查询设置失败: %v", err)
		return api.Internal()
	}
	if err := s.db.WithContext(ctx).Model(&model.SystemSetting{}).
		Where("key = ?", key).Update("value", value).Error; err != nil {
		log.Printf("[accesscontrol] 写入设置失败: %v", err)
		return api.Internal()
	}
	return nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// IsPublicIP 报告一个地址是否来自公网。
//
// **判错的方向是相反的**，因此两端都要保守：
//
//	把代理地址当成公网 → 用户在其实安全的连接上被拦一道
//	把公网当成内网     → 他在真的会被切断时毫无准备
//
// 因此这里对"不确定"的处理是：**解析不出来的地址按内网算**。理由是被切断
// 的代价（会话中断、可能再也连不上）明显大于多问一句的代价。
func IsPublicIP(raw string) bool {
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// 解析不出来（可能是主机名，或格式奇怪）——**按内网算**，见上。
		return false
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}
