package vm

import (
	"context"
	"strings"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// GuestRequest 是一次来宾自动化请求（F-2-10）。
type GuestRequest struct {
	// Action 取值见 agent.GuestAction*。
	Action string
	// Username / Password 仅改密动作使用。
	Username string
	Password string
	// DiskID 仅附加磁盘使用，指向要附加的磁盘。
	DiskID string
	// DiskGB 仅扩容使用，是扩到的大小。
	DiskGB int
}

// GuestView 描述的是一次来宾自动化在受理时的判定结果。
//
// 它比其它能力多返回一层信息：**这次操作会走「在线」还是「离线」路径**，
// 以及是否需要来宾里装好 Guest Agent。用户需要在下发之前知道这一点——
// 在线改密会要求机器运行中且装了 agent，而离线改密会要求关机，两者的前置
// 条件完全不同。
type GuestView struct {
	Action string `json:"action"`
	// RequiresGuestAgent 表示这个动作依赖来宾里的 QEMU Guest Agent。
	RequiresGuestAgent bool `json:"requires_guest_agent"`
	// GuestAgentReady 是**探测到的** agent 状态（取自虚拟机投影）。
	//
	// 与 RequiresGuestAgent 分开：前者是「这个动作要不要 agent」，后者是
	// 「现在有没有」。合并成一个布尔值会让界面无法区分「这个动作不需要
	// agent」与「需要但没装上」。
	GuestAgentReady bool `json:"guest_agent_ready"`
	// RequiresRunning 表示该动作要求虚拟机**运行中**。
	RequiresRunning bool `json:"requires_running"`
	// AvailableActions 列出当前状态下可以执行的动作。
	AvailableActions []string `json:"available_actions"`
}

// GuestAction 受理一次来宾自动化（F-2-10）。
//
// 三件事在受理时同步判定，因为它们都是**当下的确定事实**，排进队列再失败
// 只会让用户在几分钟后收到一条已经忘了背景的通知：
//
//   - 虚拟机有没有装 Guest Agent（在线类动作的前提）；
//   - 虚拟机是运行中还是已关机（在线改密要运行中，离线改密要关机）；
//   - 有没有别的任务在跑（同一台机器只允许一个进行中的操作）。
func (s *Service) GuestAction(
	ctx context.Context, id int64, req GuestRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, *GuestView, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, nil, err
	}
	if err := s.ensureNodeUsable(ctx, target.NodeID); err != nil {
		return nil, nil, err
	}

	// **先校验动作本身，再看状态**。
	//
	// 顺序反了会给出一个误导性的拒绝：动作名拼错时，requirements 会返回
	// 「不需要运行中」，于是状态检查报出「该操作需要虚拟机关机（当前：运行中）」
	// ——用户会去关机，然后发现还是不行。参数错误就该报参数错误。
	if err := validateGuestRequest(&req); err != nil {
		return nil, nil, err
	}

	needAgent, needRunning := guestActionRequirements(req.Action)

	// 实时探测，不用投影（f-2-01 R-002）：`guest_agent` 是**探测结果**字段，
	// 但它由 agent 周期上报，可能滞后——而「能不能进来宾执行」完全取决于
	// 此刻它是否真的在跑。
	current, err := s.probeStatus(ctx, target)
	if err != nil {
		return nil, nil, err
	}

	if needRunning && current != model.VMStatusRunning {
		return nil, nil, api.ValidationFailed(
			"该操作需要虚拟机运行中（当前：" + DescribeStatus(current) + "）")
	}
	if !needRunning && current != model.VMStatusStopped {
		return nil, nil, api.ValidationFailed(
			"该操作需要虚拟机关机（当前：" + DescribeStatus(current) + "）")
	}
	if needAgent && !target.GuestAgent {
		return nil, nil, api.ValidationFailed(
			"来宾系统里未检测到 QEMU Guest Agent，无法执行该操作；" +
				"请先在虚拟机内安装并启动它")
	}

	active, err := s.hasActiveTask(ctx, target.ID)
	if err != nil {
		return nil, nil, err
	}
	if active {
		return nil, nil, api.Conflict("该虚拟机有正在执行的任务，请先等待完成或取消")
	}

	params := map[string]any{
		"vm_id":   target.ID,
		"vm_name": target.Name,
		"action":  req.Action,
		// 探测到的状态一并入队：事后区分「受理时判定与执行时不一致」与
		// 「状态已变」时，这是唯一的线索。
		"observed_status": current,
	}
	// 密码**随任务持久化**，与 R-009「口令不落库」冲突吗？
	//
	// 不冲突的前提是它必须能被执行方取到：任务可能在几秒后才执行，而控制面
	// 不保留明文。这里的处理是：入队时写入，执行器**在成功后立即清除**该字段
	// （见 guestExecutor.Run）。审计流水里记的是「谁在什么时候改了哪个用户的
	// 密码」，**不含密码本身**。
	if req.Password != "" {
		params["password"] = req.Password
	}
	if req.Username != "" {
		params["username"] = req.Username
	}
	if req.DiskID != "" {
		params["disk_id"] = req.DiskID
	}
	if req.DiskGB > 0 {
		params["disk_gb"] = req.DiskGB
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMGuest,
		NodeID:       target.NodeID,
		ResourceType: "vm",
		ResourceID:   target.ID,
		ResourceName: target.Name,
		OwnerID:      ownerOf(target, v),
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, nil, err
	}

	// 审计只记动作与目标，**不记密码**。
	auditParams := map[string]any{"task_id": t.ID, "action": req.Action}
	if req.Username != "" {
		auditParams["username"] = req.Username
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: target.NodeID, ResourceType: "vm", ResourceID: target.ID,
		ResourceName: target.Name, Action: "vm.guest." + req.Action,
		Params: auditParams, Success: true, ClientIP: clientIP,
	})

	view := guestViewOf(target, req.Action)
	return t, view, nil
}

// GuestCapabilities 返回当前状态下可用的来宾自动化动作（API-088）。
func (s *Service) GuestCapabilities(
	ctx context.Context, id int64, v authz.Viewer,
) (*GuestView, error) {
	target, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	return guestViewOf(target, ""), nil
}

// guestActionRequirements 返回某动作是否需要 Guest Agent、是否需要运行中。
//
// 集中成一张表而不是散在 if 里：这组对应关系是**接口契约的一部分**
// （界面据此决定按钮可不可点），散开写迟早会有一处与另一处不一致。
func guestActionRequirements(action string) (needAgent, needRunning bool) {
	switch action {
	case agentGuestActionPasswordOnline:
		return true, true
	case agentGuestActionPasswordOffline:
		// 离线改密**不用** agent（那正是它存在的意义：来宾起不来的时候用），
		// 但必须关机——挂载一块正在被写入的磁盘会同时损坏两侧看到的内容。
		return false, false
	case agentGuestActionDiskAttach:
		return true, true
	case agentGuestActionExpandDisk:
		// 宿主机侧加长磁盘要关机；来宾侧扩文件系统要 agent。整体按
		// 「关机 + 用 agent」处理：先关机加长，再开机进来宾扩。
		return true, false
	default:
		return false, false
	}
}

func guestViewOf(target *model.VM, action string) *GuestView {
	view := &GuestView{
		Action:          action,
		GuestAgentReady: target.GuestAgent,
	}
	if action != "" {
		view.RequiresGuestAgent, view.RequiresRunning = guestActionRequirements(action)
	}

	// 可用动作按**当前状态**算：运行中只给在线类的，关机给离线类的。
	// 让界面据此禁用按钮，而不是让用户点下去才知道不行。
	if target.GuestAgent && target.Status == model.VMStatusRunning {
		view.AvailableActions = append(view.AvailableActions, agentGuestActionPasswordOnline)
		view.AvailableActions = append(view.AvailableActions, agentGuestActionDiskAttach)
	}
	if target.Status == model.VMStatusStopped {
		// 离线改密不需要 agent，因此不受 GuestAgent 限制。
		view.AvailableActions = append(view.AvailableActions, agentGuestActionPasswordOffline)
	}
	if target.GuestAgent && target.Status == model.VMStatusStopped {
		view.AvailableActions = append(view.AvailableActions, agentGuestActionExpandDisk)
	}
	return view
}

func validateGuestRequest(req *GuestRequest) error {
	switch req.Action {
	case agentGuestActionPasswordOnline, agentGuestActionPasswordOffline:
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" {
			return api.InvalidParameter("必须指定要改密的用户名")
		}
		if len(req.Password) < 8 {
			// 长度下限是**本面板的**约束，不是替代来宾的密码策略：
			// 来宾自己的 pam 规则仍然会生效，那里拒绝时会给出更具体的原因。
			return api.InvalidParameter("密码长度至少 8 位")
		}
		if strings.ContainsAny(req.Username, " :\n\t") || strings.ContainsAny(req.Password, "\n") {
			// 用户名与密码会被拼进 shell 命令交给来宾执行，含空白或换行
			// 的输入会让命令被拆开——那是注入，不只是格式问题。
			return api.InvalidParameter("用户名或密码包含非法字符")
		}
	case agentGuestActionDiskAttach:
		if strings.TrimSpace(req.DiskID) == "" {
			return api.InvalidParameter("必须指定要附加的磁盘")
		}
	case agentGuestActionExpandDisk:
		if req.DiskGB <= 0 {
			return api.InvalidParameter("必须指定扩容后的磁盘大小")
		}
	default:
		return api.InvalidParameter("不支持的来宾自动化动作")
	}
	return nil
}

// 常量与 agent 包保持一致，但用一个本地别名避免本文件到处都是包名前缀。
const (
	agentGuestActionPasswordOnline  = "password_online"
	agentGuestActionPasswordOffline = "password_offline"
	agentGuestActionDiskAttach      = "disk_attach"
	agentGuestActionExpandDisk      = "expand_disk"
)
