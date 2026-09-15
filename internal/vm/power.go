package vm

import (
	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/model"
)

// PowerAction 是可执行的电源操作。
type PowerAction string

// 电源动作。
//
// shutdown 与 poweroff 是**两个独立操作**而非同一个动作的两种强度：
// 前者请求来宾配合关机（可能超时），后者直接断电。中断等待、改为强制断电
// 是用户的决定，系统不代做（f-2-01 R-006）——静默强杀可能造成来宾文件系统
// 损坏，而这个代价不可逆。
const (
	PowerStart    PowerAction = "start"
	PowerShutdown PowerAction = "shutdown"
	PowerPoweroff PowerAction = "poweroff"
	PowerReboot   PowerAction = "reboot"
	PowerReset    PowerAction = "reset"
)

// allowedSourceStates 定义每个动作可用的**来源状态**。
//
// 这里限制的是「虚拟化层的真实状态」，而不是投影状态：投影可能滞后，
// 凭它判断可行性就可能在虚拟机实际运行时执行危险操作（f-2-01 R-002）。
//
// 几点刻意的取舍：
//   - 已暂停的虚拟机**不能**直接关机：来宾已停止调度，shutdown 请求没有
//     对象响应，只会白白等到超时。正确路径是先恢复再关机（或强制断电）。
//   - 已暂停的虚拟机**可以**硬重置（R-007）——这是把它从暂停中拉回来的手段。
//   - unknown 不在任何动作的可用范围内：状态未知时无法判定操作是否安全，
//     此时应当拒绝，而不是猜一个（猜错的方向可能是对运行中的虚拟机断电）。
var allowedSourceStates = map[PowerAction][]string{
	PowerStart:    {model.VMStatusStopped},
	PowerShutdown: {model.VMStatusRunning},
	PowerPoweroff: {model.VMStatusRunning},
	PowerReboot:   {model.VMStatusRunning},
	PowerReset:    {model.VMStatusPaused},
}

// targetStatus 是动作成功后期望的状态，用于回写投影。
//
// 它只是**期望值**：agent 若在结果中返回了真实状态，以 agent 的为准
// （见 PowerExecutor.Run）。
var targetStatus = map[PowerAction]string{
	PowerStart:    model.VMStatusRunning,
	PowerShutdown: model.VMStatusStopped,
	PowerPoweroff: model.VMStatusStopped,
	PowerReboot:   model.VMStatusRunning,
	PowerReset:    model.VMStatusRunning,
}

// actionLabel 是动作的中文名，用于提示文案。
var actionLabel = map[PowerAction]string{
	PowerStart:    "开机",
	PowerShutdown: "关机",
	PowerPoweroff: "强制断电",
	PowerReboot:   "重启",
	PowerReset:    "重置",
}

// statusLabel 把虚拟化层状态翻译成用户能读懂的说法。
//
// unknown 写作「状态未知」而非「已停止」：离线 ≠ 关机，把两者混为一谈会
// 诱导用户执行无意义的操作（f-2-01 R-003）。
var statusLabel = map[string]string{
	model.VMStatusRunning:   "运行中",
	model.VMStatusStopped:   "已关机",
	model.VMStatusPaused:    "已暂停",
	model.VMStatusSuspended: "已挂起",
	model.VMStatusError:     "错误",
	model.VMStatusUnknown:   "状态未知",
}

// ParsePowerAction 解析动作名；未登记的动作直接拒绝。
func ParsePowerAction(raw string) (PowerAction, error) {
	action := PowerAction(raw)
	if _, ok := allowedSourceStates[action]; !ok {
		return "", api.InvalidParameter("不支持的电源操作")
	}
	return action, nil
}

// Label 返回动作的中文名。
func (a PowerAction) Label() string {
	if label, ok := actionLabel[a]; ok {
		return label
	}
	return string(a)
}

// Op 返回动作对应的领域操作标识。
func (a PowerAction) Op() agent.OpKind {
	return agent.OpKind("vm." + string(a))
}

// Validate 校验动作能否用于给定的真实状态。
//
// 错误文案里带上**当前真实状态**：只说「操作不被允许」会让用户以为是权限
// 问题，而真正的原因是状态不匹配。
func (a PowerAction) Validate(current string) error {
	allowed := allowedSourceStates[a]
	for _, s := range allowed {
		if s == current {
			return nil
		}
	}
	return api.ValidationFailed(
		"虚拟机当前为" + DescribeStatus(current) + "，无法执行" + a.Label(),
	)
}

// AvailableActions 返回给定状态下界面应展示为**可用**的动作。
//
// 供前端渲染按钮用。后端在受理请求时仍会基于实时探测重新校验——前端禁用
// 只是体验优化，不构成安全边界（f-2-01 R-004）。
func AvailableActions(current string) []string {
	actions := make([]string, 0, len(allowedSourceStates))
	// 按固定顺序输出，避免 map 迭代顺序随机导致前端按钮每次渲染都变位置。
	for _, a := range []PowerAction{PowerStart, PowerShutdown, PowerReboot, PowerPoweroff, PowerReset} {
		for _, s := range allowedSourceStates[a] {
			if s == current {
				actions = append(actions, string(a))
				break
			}
		}
	}
	return actions
}

// DescribeStatus 把状态翻译为中文。
func DescribeStatus(status string) string {
	if label, ok := statusLabel[status]; ok {
		return label
	}
	// 未登记的状态原样显示：比笼统的「未知状态」更有助于排查。
	return status
}
