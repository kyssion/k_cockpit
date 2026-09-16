package vm

import (
	"context"
	"errors"
	"strconv"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// maxBatchSize 是一次批量操作的上限。
//
// 不是性能考虑：每台都要独立探测与入队，50 台已经会产生 50 次节点往返。
// 设上限是为了让「一次提交几百台」这种操作在受理前就被拦住，而不是让
// 用户在界面上等一个几分钟都没有响应的请求。
const maxBatchSize = 50

// BatchPowerRequest 是一次批量电源操作。
type BatchPowerRequest struct {
	VMIDs  []int64
	Action string
}

// BatchPowerItem 是单台的结果。
type BatchPowerItem struct {
	VMID int64 `json:"vm_id"`
	// VMName 在受理成功时回填；失败时可能为空（比如那一台根本不属于当前用户，
	// 我们连它的名字都不应该知道）。
	VMName string `json:"vm_name,omitempty"`
	OK     bool   `json:"ok"`
	TaskID int64  `json:"task_id,omitempty"`
	// Error 是这一台失败的原因，面向用户。
	Error string `json:"error,omitempty"`
}

// BatchPowerResponse 是批量的整体结果。
type BatchPowerResponse struct {
	Items []BatchPowerItem `json:"items"`
	// Succeeded 与 Failed 是**给界面直接用的汇总**，不必让前端再数一遍
	// items——两处各数一遍迟早不一致。
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// BatchPower 受理一次批量电源操作。
//
// 核心语义：**逐台独立**。某一台失败不影响其它台，响应里逐台给出结果。
// 这是 f-2-01 Q-007 的决定——用单一批次任务的话，「失败」与「取消」的
// 粒度都无法表达：用户想取消其中一台，而那一台与另外 49 台绑在同一个
// 任务里，只能整批取消。
//
// 实现上**逐台复用 Power**，而不是在这里另写一遍校验：单台与批量必须走
// 同一条路径，否则两条路上的状态校验、审计与入队参数迟早会漂移，而
// 漂移的表现是「单台能关机、批量说状态不允许」。
//
// 代价是每台都要向节点探测一次状态。这个代价必须付：不同虚拟机的运行态
// 本来就不同，用一个共享的状态去判断会让其中一部分必然判错。
func (s *Service) BatchPower(
	ctx context.Context, req BatchPowerRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*BatchPowerResponse, error) {
	if len(req.VMIDs) == 0 {
		return nil, api.InvalidParameter("请至少选择一台虚拟机")
	}
	if len(req.VMIDs) > maxBatchSize {
		return nil, api.InvalidParameter(
			"一次最多操作 " + strconv.Itoa(maxBatchSize) + " 台，请分批执行")
	}

	// 动作本身先校验一次：如果整个动作都是非法的（比如拼错了），
	// 没有必要把它对每一台各失败一次。
	if _, err := ParsePowerAction(req.Action); err != nil {
		return nil, err
	}

	resp := &BatchPowerResponse{Items: make([]BatchPowerItem, 0, len(req.VMIDs))}
	// 去重：界面上不太可能重复传同一个 ID，但一个重复的 ID 会产生两个任务，
	// 第二个必然因为「状态已变」而失败——用户看到一条莫名的失败记录。
	seen := make(map[int64]bool, len(req.VMIDs))

	for _, id := range req.VMIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		item := BatchPowerItem{VMID: id}
		t, err := s.Power(ctx, id, req.Action, v, operatorName, clientIP)
		if err != nil {
			item.OK = false
			item.Error = userMessage(err)
			resp.Failed++
		} else {
			item.OK = true
			item.TaskID = t.ID
			resp.Succeeded++
		}
		resp.Items = append(resp.Items, item)
	}

	return resp, nil
}

// userMessage 提取面向用户的错误文案。
//
// 不是 *api.Error 的错误一律给一句通用文案：内部错误的细节只应进日志，
// 直接透出去可能包含数据库语句或路径这类不该出现在界面上的内容。
func userMessage(err error) string {
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		return apiErr.Message
	}
	return "操作失败"
}
