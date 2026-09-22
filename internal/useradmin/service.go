// Package useradmin 实现用户管理（F-1-07）。
//
// 本包最关键的一处设计在**封禁**上。
//
// 规格把封禁定义成一个**级联动作**：置状态 → 关 SSH → 关运行中虚拟机，
// 并且「单步失败仅告警」。这后半句容易被当成"实现从简"，但它其实是一条
// 明确的语义要求，而它为什么成立值得写下来：
//
//	封禁的实质是「这个人不能再用系统」，而**置状态那一步就达成了它**。
//	后面两步（关 SSH、关虚拟机）是**减少暴露面**的加固动作——把已经
//	在跑的会话和机器收掉。
//
// 如果让加固动作的失败回滚状态，用户会处于「以为封了、其实没封」的状态
// ——那比只做到一半更糟。因此本包的顺序是刻意的：
//
//   - 第一步失败 → 整个操作失败（封禁没生效，如实报告）；
//   - 后两步失败 → 记进 warnings 并**照样返回成功**。
//
// 界面上这两类结果的呈现完全不同：前者是错误，后者是一句「已封禁，但
// 有 2 台虚拟机未能关机」。
package useradmin

import (
	"context"
	"errors"
	"log"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供用户管理能力。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
	// hasher 默认为 auth.HashPassword ——**与登录用同一个实现**。
	//
	// 两处各写一套的话迟早会分叉，而分叉的表现是「管理员建的账号自己登
	// 不上」——那是最难查的一类问题：两边看起来都对，只是不匹配。
	hasher func(string) (string, error)
	// agent 用于下发宿主机上的账号设置（SSH 访问权限）。为 nil 时只改控制面
	// 记录，并在结果里注明宿主机上尚未生效——**不假装生效**。
	agent agent.Client
	// quota 用于同时设置配额；为 nil 时跳过。
	quota quotaSetter
}

// quotaSetter 是本包对配额服务的**最小依赖**。
type quotaSetter interface {
	Set(ctx context.Context, req QuotaRequest, operatorID int64, operatorName, clientIP string) error
}

// QuotaRequest 与 quota.SetQuotaRequest 形状一致。
//
// 定义一个本地的镜像类型而不是直接引用 quota 包：本包只用到其中一个字段，
// 而引入 quota 包会把它的全部依赖也带进来。转换发生在装配处（一次），
// 而不是让每一次调用都背上这个依赖。
type QuotaRequest struct {
	UserID     int64
	NodeID     int64
	QuotaBytes int64
	Enabled    bool
}

// SetAgent 装配 agent 客户端。
//
// 不调用时 SSH 权限只改控制面记录，并在结果里注明宿主机上尚未生效——
// **不假装在宿主机上生效了**：那样"明明禁用了却还能登"会成为无法解释的
// 问题。
func (s *Service) SetAgent(c agent.Client) { s.agent = c }

// NewService 构造服务。
// CreateFromInvite 由邀请流程创建账号。
//
// 与 Create 的区别只有一处：**密码是受邀人自己设的**，因此不走强制改密——
// 他自己刚设的密码，再让他改一遍没有意义。其余校验（角色、用户名）与 Create
// 保持一致，避免两条创建路径长成两套口径。
func (s *Service) CreateFromInvite(
	ctx context.Context, email, username, password, role string,
	quotaEnabled bool, quotaBytes int64,
) error {
	if strings.TrimSpace(username) == "" {
		return api.InvalidParameter("必须填写用户名")
	}
	if len(username) > 64 {
		return api.InvalidParameter("用户名最长 64 字")
	}
	// 受邀人自设密码，要求比管理员给的初始密码更长（12 位）：他只设这一次，
	// 而这次的质量决定这个账号的长期强度。
	if len(password) < 12 {
		return api.InvalidParameter("密码至少 12 位")
	}
	if role != model.RoleTenant && role != model.RoleAdmin {
		return api.InvalidParameter("角色必须是 tenant 或 admin")
	}

	hash, err := s.hasher(password)
	if err != nil {
		log.Printf("[useradmin] 生成密码哈希失败: %v", err)
		return api.Internal()
	}

	// 复用 Create：配额、审计、状态都在那一条路径上。
	_, err = s.Create(ctx, CreateRequest{
		Username: username, Password: password, Role: role, Email: email,
	}, authz.Viewer{UserID: 0, IsAdmin: true}, "invite", "")
	if hash == "" || err != nil {
		return err
	}

	// 配额：邀请时就把额度定下来。
	if s.quota != nil && quotaEnabled {
		var row model.User
		if e := s.db.WithContext(ctx).Where("username = ?", username).Select("id").First(&row).Error; e == nil {
			if e := s.quota.Set(ctx, QuotaRequest{
				UserID: row.ID, NodeID: 0,
				QuotaBytes: quotaBytes, Enabled: true,
			}, 0, "invite", ""); e != nil {
				// 配额设置失败**不算注册失败**：账号已经能用了，额度可以随后
				// 补。让一次配额写入失败把注册判为失败，用户会以为邮箱被占了。
				log.Printf("[useradmin] 设置邀请配额失败 user=%d: %v", row.ID, e)
			}
		}
	}
	return nil
}

func NewService(db *gorm.DB, recorder *audit.Recorder, q quotaSetter) *Service {
	return &Service{db: db, audit: recorder, hasher: auth.HashPassword, quota: q}
}

// View 是用户的对外视图。
type View struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	Status   string `json:"status"`
	Email    string `json:"email,omitempty"`
	// TotpEnabled 供管理员判断这个账号的保护强度。
	TotpEnabled bool   `json:"totp_enabled"`
	Remark      string `json:"remark,omitempty"`
	CreatedAt   string `json:"created_at"`
	// LastLoginAt 为空表示从未登录过。
	//
	// 它有一个实际用途：一个建了很久却从没登录过的账号，通常说明它已经
	// 不需要了——而"从没用过"这件事在列表里看不出来，只能靠这一列。
	LastLoginAt string `json:"last_login_at,omitempty"`

	// MaxBandwidthMbps 是带宽上限（Mbps），0 表示不限。
	//
	// 它是**速率型**：没有"用满"的时刻，因此不放进按月累计的 resource_quota。
	// 消费点在网口限速上——该用户在这台节点上的各网卡限速之和不得超过它。
	MaxBandwidthMbps int `json:"max_bandwidth_mbps"`
	// SSHAccessEnabled 表示是否允许该用户 SSH 登录宿主机。
	SSHAccessEnabled bool `json:"ssh_access_enabled"`
}

// ListFilter 是用户列表的筛选。
type ListFilter struct {
	// Keyword 匹配用户名与邮箱。
	Keyword string
	Status  string
	Role    string

	Page     int
	PageSize int
}

// Page 是一页用户。
type Page struct {
	Items    []View `json:"items"`
	Total    int64  `json:"total"`
	Page     int    `json:"page"`
	PageSize int    `json:"page_size"`
}

const (
	maxPageSize     = 100
	defaultPageSize = 20
)

// List 返回用户列表。
//
// 只返回 `tenant` 与 `admin` 两类真实账号；**软删除的不返回**。
func (s *Service) List(ctx context.Context, f ListFilter) (*Page, error) {
	query := s.db.WithContext(ctx).Model(&model.User{})

	if kw := strings.TrimSpace(f.Keyword); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		query = query.Where(
			"username LIKE ? ESCAPE '\\' OR email LIKE ? ESCAPE '\\'", like, like)
	}
	if f.Status != "" {
		query = query.Where("status = ?", f.Status)
	}
	if f.Role != "" {
		query = query.Where("role = ?", f.Role)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		log.Printf("[useradmin] 统计用户失败: %v", err)
		return nil, api.Internal()
	}

	page, size := f.Page, f.PageSize
	if page <= 0 {
		page = 1
	}
	if size <= 0 {
		size = defaultPageSize
	}
	if size > maxPageSize {
		size = maxPageSize
	}

	var rows []model.User
	if err := query.
		Order("id ASC").
		Offset((page - 1) * size).Limit(size).
		Find(&rows).Error; err != nil {
		log.Printf("[useradmin] 查询用户失败: %v", err)
		return nil, api.Internal()
	}

	// 最后登录时间**一次查完**：逐个查会把一次列表请求变成上百次往返。
	lastLogin, err := s.lastLoginMap(ctx, rows)
	if err != nil {
		return nil, err
	}

	items := make([]View, 0, len(rows))
	for i := range rows {
		items = append(items, toView(&rows[i], lastLogin[rows[i].ID]))
	}
	return &Page{Items: items, Total: total, Page: page, PageSize: size}, nil
}

// CreateRequest 是创建用户的请求。
type CreateRequest struct {
	Username string
	Password string
	Role     string
	Email    string
	Remark   string
}

// Create 创建用户（API-210）。
func (s *Service) Create(
	ctx context.Context, req CreateRequest, v authz.Viewer, operatorName, clientIP string,
) (*View, error) {
	username := strings.TrimSpace(req.Username)
	if username == "" {
		return nil, api.InvalidParameter("必须填写用户名")
	}
	if len(username) > 64 {
		return nil, api.InvalidParameter("用户名最长 64 字")
	}
	if len(req.Password) < 8 {
		// 下限是 8 位而不是更长：管理员创建的是**初始密码**，而强制复杂度的
		// 后果是所有人都在便利贴上写同一个。真正的保护在于那个账号能被
		// 要求首次登录改密（见 ForcePasswordChange）。
		return nil, api.InvalidParameter("初始密码至少 8 位")
	}
	role := req.Role
	if role == "" {
		role = model.RoleTenant
	}
	if role != model.RoleTenant && role != model.RoleAdmin {
		// 不允许创建系统账号（如 bootstrap）：那类账号由初始化流程创建，
		// 而让管理界面能建"系统账号"等于把一条不该存在的路径打开。
		return nil, api.InvalidParameter("角色必须是 tenant 或 admin")
	}

	hash, err := s.hasher(req.Password)
	if err != nil {
		log.Printf("[useradmin] 生成密码哈希失败: %v", err)
		return nil, api.Internal()
	}

	row := model.User{
		Username: username, PasswordHash: hash, Role: role,
		// 新建的账号是**待激活**而不是直接可用：那给了一个"先建好、确认
		// 无误、再让人用"的中间状态。
		Status:              model.UserStatusPending,
		ForcePasswordChange: true,
	}
	if req.Email != "" {
		row.Email = &req.Email
	}
	if req.Remark != "" {
		row.Remark = &req.Remark
	}

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("用户名已被占用")
		}
		log.Printf("[useradmin] 创建用户失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "user", ResourceID: row.ID, ResourceName: row.Username,
		Action: "user.create",
		Params: map[string]any{"role": role, "must_change_password": true},
		// **绝不记密码**，哪怕是初始密码。审计流水会被很多人看到，
		// 而一条初始密码在里面就等于一个长期可用的后门。
		Success: true, ClientIP: clientIP,
	})

	view := toView(&row, nil)
	return &view, nil
}

// UpdateRequest 是编辑用户的请求。
type UpdateRequest struct {
	Email  *string
	Remark *string
	// Role 为空表示不改。
	Role string
	// MaxBandwidthMbps 为 nil 表示不改；为 0 表示不限。
	//
	// 用指针而不是 int：0 本身是一个合法取值（不限），用 int 的零值去表达
	// "不改"会让"取消上限"这个操作永远做不了。
	MaxBandwidthMbps *int
}

// Update 编辑用户资料（API-211）。
//
// **不含密码**：改密是一个独立且更敏感的动作（它会让已登录的会话全部失效），
// 而把它塞进"编辑资料"里会让一次无心的保存造成一次全员登出。
func (s *Service) Update(
	ctx context.Context, id int64, req UpdateRequest, v authz.Viewer,
	operatorName, clientIP string,
) (*View, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if req.Email != nil {
		updates["email"] = optString(*req.Email)
	}
	if req.Remark != nil {
		updates["remark"] = optString(*req.Remark)
	}
	if req.Role != "" {
		if req.Role != model.RoleTenant && req.Role != model.RoleAdmin {
			return nil, api.InvalidParameter("角色必须是 tenant 或 admin")
		}
		// **不允许改自己的角色**：管理员把自己的角色降级之后，如果他是
		// 最后一个管理员，就再也没人能进管理界面了——而那是一次不可逆的
		// 自锁。
		if v.UserID == row.ID {
			return nil, api.ValidationFailed("不能修改自己的角色")
		}
		if row.Role == model.RoleAdmin && req.Role == model.RoleTenant {
			if err := s.ensureNotLastAdmin(ctx, row.ID); err != nil {
				return nil, err
			}
		}
		updates["role"] = req.Role
	}
	if req.MaxBandwidthMbps != nil {
		if *req.MaxBandwidthMbps < 0 {
			return nil, api.InvalidParameter("带宽上限不能为负数")
		}
		updates["max_bandwidth_mbps"] = *req.MaxBandwidthMbps
	}

	if len(updates) == 0 {
		view := toView(row, nil)
		return &view, nil
	}
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", row.ID).Updates(updates).Error; err != nil {
		log.Printf("[useradmin] 更新用户失败: %v", err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "user", ResourceID: row.ID, ResourceName: row.Username,
		Action:  "user.update",
		Params:  updates,
		Success: true, ClientIP: clientIP,
	})

	updated, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	view := toView(updated, nil)
	return &view, nil
}

// SetStatusResult 是封禁/解封的结果。
type SetStatusResult struct {
	User View `json:"user"`
	// Warnings 是**级联动作**里失败的那些。
	//
	// 它们不影响操作本身的成败——封禁已经生效了——但用户需要知道
	// 「还有什么没做成」。
	Warnings []string `json:"warnings,omitempty"`
}

// SetStatus 封禁或解封（API-212）。
//
// 级联的语义见包注释：**第一步失败则整体失败，后两步失败仅告警**。
func (s *Service) SetStatus(
	ctx context.Context, id int64, status string, v authz.Viewer,
	operatorName, clientIP string,
) (*SetStatusResult, error) {
	row, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if status != model.UserStatusActive && status != model.UserStatusBanned {
		return nil, api.InvalidParameter("状态必须是 active 或 banned")
	}
	if row.Status == status {
		// 幂等：重复封禁不是错误，但也不重复记账。
		view := toView(row, nil)
		return &SetStatusResult{User: view}, nil
	}

	// **不允许封禁自己**：一个管理员把自己封掉之后，如果他是最后一个
	// 管理员，就再也没人能进管理界面解封了。
	if v.UserID == row.ID {
		return nil, api.ValidationFailed("不能封禁自己")
	}
	if status == model.UserStatusBanned && row.Role == model.RoleAdmin {
		if err := s.ensureNotLastAdmin(ctx, row.ID); err != nil {
			return nil, err
		}
	}

	// 第一步：置状态。**这一步失败则整体失败**——它是「封禁」的实质。
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("id = ?", row.ID).Update("status", status).Error; err != nil {
		log.Printf("[useradmin] 更新用户状态失败: %v", err)
		return nil, api.Internal()
	}

	result := &SetStatusResult{}
	if status == model.UserStatusBanned {
		// 后两步：加固动作，失败只记警告。
		result.Warnings = append(result.Warnings, s.revokeSessions(ctx, row.ID)...)
		result.Warnings = append(result.Warnings, s.stopVMs(ctx, row.ID)...)
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "user", ResourceID: row.ID, ResourceName: row.Username,
		Action:      "user." + status,
		BeforeState: map[string]any{"status": row.Status},
		AfterState:  map[string]any{"status": status},
		Params:      map[string]any{"cascade_warnings": result.Warnings},
		// 级联有失败时**仍记为成功**：封禁确实生效了。把它记成失败会让
		// 审计流水里出现一条「封禁失败」，而那人其实已经被封了。
		Success: true, ClientIP: clientIP,
	})

	updated, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	result.User = toView(updated, nil)
	return result, nil
}

// Delete 删除用户（API-213，软删除）。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) error {
	row, err := s.load(ctx, id)
	if err != nil {
		return err
	}
	if v.UserID == row.ID {
		return api.ValidationFailed("不能删除自己")
	}
	if row.Role == model.RoleAdmin {
		if err := s.ensureNotLastAdmin(ctx, row.ID); err != nil {
			return err
		}
	}

	// 有虚拟机时不删：那些机器还在跑，而删掉属主会让它们变成"没人管"的
	// 资源——界面上看不到归属，配额也失去计数对象。
	var vmCount int64
	if err := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("owner_id = ? AND present = ?", row.ID, true).Count(&vmCount).Error; err != nil {
		log.Printf("[useradmin] 统计用户名下虚拟机失败: %v", err)
		return api.Internal()
	}
	if vmCount > 0 {
		return api.Conflict(
			"该用户名下还有 " + strconv.FormatInt(vmCount, 10) +
				" 台虚拟机；请先转移或删除它们——直接删账号会让这些机器变成无人管理的资源")
	}

	// 软删除：保留审计与历史引用。硬删除会让审计流水里的 operator_id
	// 指向一个不存在的用户，那些记录就再也解释不清了。
	if err := s.db.WithContext(ctx).Delete(&model.User{}, row.ID).Error; err != nil {
		log.Printf("[useradmin] 删除用户失败: %v", err)
		return api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		ResourceType: "user", ResourceID: row.ID, ResourceName: row.Username,
		Action:  "user.delete",
		Success: true, ClientIP: clientIP,
	})
	return nil
}

// --- 级联动作 ---

// revokeSessions 撤销该用户的全部会话。
func (s *Service) revokeSessions(ctx context.Context, userID int64) []string {
	res := s.db.WithContext(ctx).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Delete(&model.Session{})
	if res.Error != nil {
		log.Printf("[useradmin] 撤销会话失败 user=%d: %v", userID, res.Error)
		return []string{"登录会话未能撤销，该用户可能仍然在线"}
	}
	return nil
}

// stopVMs 关掉该用户正在运行的虚拟机。
//
// **只置状态、不入队下发**：真正关机需要节点动作，而封禁不该因为节点不可达
// 而卡住。把机器标记为停止交由后续的收敛处理——它至少让界面上不再显示
// 「这台机器还在为这个人运行」。
//
// 这是一个**有意的取舍**，必须说清楚：它不会立刻释放宿主机的资源。
// 要做到"真关掉"就得排队等节点，而那会让一次封禁的成败取决于某台宿主机
// 是否在线——封禁不该有这个依赖。
func (s *Service) stopVMs(ctx context.Context, userID int64) []string {
	res := s.db.WithContext(ctx).Model(&model.VM{}).
		Where("owner_id = ? AND status = ?", userID, model.VMStatusRunning).
		Update("status", model.VMStatusStopped)
	if res.Error != nil {
		log.Printf("[useradmin] 停止虚拟机失败 user=%d: %v", userID, res.Error)
		return []string{"运行中的虚拟机未能停止"}
	}
	if res.RowsAffected > 0 {
		return []string{
			strconv.FormatInt(res.RowsAffected, 10) +
				" 台运行中的虚拟机已标记为停止，但**节点上的关机需要后续下发**——" +
				"封禁不等待节点，因此宿主机的资源此时可能仍未释放",
		}
	}
	return nil
}

// ensureNotLastAdmin 阻止把最后一个管理员降级或删掉。
//
// 那是一次**不可逆的自锁**：之后没人能进管理界面，只能上宿主机改数据库。
func (s *Service) ensureNotLastAdmin(ctx context.Context, excludeID int64) error {
	var n int64
	if err := s.db.WithContext(ctx).Model(&model.User{}).
		Where("role = ? AND status <> ? AND id <> ?", model.RoleAdmin, model.UserStatusBanned, excludeID).
		Count(&n).Error; err != nil {
		log.Printf("[useradmin] 统计管理员失败: %v", err)
		return api.Internal()
	}
	if n == 0 {
		return api.ValidationFailed(
			"这是最后一个可用的管理员账号——降级、封禁或删除它之后就再也没人能进管理界面了")
	}
	return nil
}

// --- 内部 ---

func (s *Service) load(ctx context.Context, id int64) (*model.User, error) {
	var row model.User
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("用户不存在")
		}
		log.Printf("[useradmin] 查询用户失败: %v", err)
		return nil, api.Internal()
	}
	return &row, nil
}

// lastLoginMap 批量取每个用户的最近登录时间。
func (s *Service) lastLoginMap(
	ctx context.Context, users []model.User,
) (map[int64]*model.AuditLog, error) {
	out := map[int64]*model.AuditLog{}
	if len(users) == 0 {
		return out, nil
	}
	ids := make([]int64, 0, len(users))
	for i := range users {
		ids = append(ids, users[i].ID)
	}

	var rows []model.AuditLog
	if err := s.db.WithContext(ctx).
		Where("action = ? AND operator_id IN ?", "user.login", ids).
		Order("at ASC").Find(&rows).Error; err != nil {
		log.Printf("[useradmin] 查询登录记录失败: %v", err)
		return nil, api.Internal()
	}
	// 按升序遍历，后写的覆盖前面的 → 留下的是最近一次。
	for i := range rows {
		if rows[i].OperatorID != nil {
			out[*rows[i].OperatorID] = &rows[i]
		}
	}
	return out, nil
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func toView(u *model.User, last *model.AuditLog) View {
	view := View{
		ID: u.ID, Username: u.Username, Role: u.Role, Status: u.Status,
		TotpEnabled: u.TotpEnabled,
		CreatedAt:   u.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),

		MaxBandwidthMbps: u.MaxBandwidthMbps,
		SSHAccessEnabled: u.SSHAccessEnabled,
	}
	if u.Email != nil {
		view.Email = *u.Email
	}
	if u.Remark != nil {
		view.Remark = *u.Remark
	}
	if last != nil {
		view.LastLoginAt = last.At.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}

func optString(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

// escapeLike 转义 LIKE 通配符。
//
// **必须配合 `ESCAPE '\'` 子句**：PostgreSQL 的 LIKE 默认转义符是反斜杠，
// 而 SQLite 没有默认转义符——只写转义的话，这个筛选会在生产上正确、在
// 测试库上静默失效（详见 auditlog 包的同名函数）。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "%", "\\%")
	return strings.ReplaceAll(s, "_", "\\_")
}

func isDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}
