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

// BatchActionDelete 是批量操作里删除动作的标识。
//
// 电源动作复用 PowerAction 的取值（start / shutdown / ...），而删除不属于
// 电源动作，因此单独定义。放在同一个 action 字段里而不是拆一个接口出来：
// 对用户来说「批量选中几台，然后选做什么」是一件事，界面上的操作条也是
// 一个——拆成两个接口会让前端的批量逻辑分叉成两套。
const BatchActionDelete = "delete"

// BatchRequest 是一次批量操作。
type BatchRequest struct {
	VMIDs []int64
	// Action 取值 start / shutdown / poweroff / reset / delete。
	Action string
	// DiskAction 仅 delete 需要，取值 delete / keep。
	//
	// **不给默认值**（R-009）：连盘删除不可逆、保留磁盘会留下孤儿数据，
	// 两者代价完全不同，由服务端替用户选一个等于把这个决定藏起来。
	DiskAction string
}

// BatchItem 是单台的结果。
type BatchItem struct {
	VMID int64 `json:"vm_id"`
	// VMName 在受理成功时回填；失败时可能为空（比如那一台根本不属于当前用户，
	// 我们连它的名字都不应该知道）。
	VMName string `json:"vm_name,omitempty"`
	OK     bool   `json:"ok"`
	TaskID int64  `json:"task_id,omitempty"`
	// Error 是这一台失败的原因，面向用户。
	Error string `json:"error,omitempty"`
}

// BatchResult 是批量的整体结果。
type BatchResult struct {
	Items []BatchItem `json:"items"`
	// Succeeded 与 Failed 是**给界面直接用的汇总**，不必让前端再数一遍
	// items——两处各数一遍迟早不一致。
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

// Batch 受理一次批量操作。
//
// 核心语义：**逐台独立**。某一台失败不影响其它台，响应里逐台给出结果。
// 这是 f-2-01 Q-007 的决定——用单一批次任务的话，「失败」与「取消」的
// 粒度都无法表达：用户想取消其中一台，而那一台与另外 49 台绑在同一个
// 任务里，只能整批取消。
//
// 实现上**逐台复用单台入口**（Power / Delete），而不是在这里另写一遍校验：
// 单台与批量必须走同一条路径，否则两条路上的状态校验、审计与入队参数迟早
// 会漂移，而漂移的表现是「单台能关机、批量说状态不允许」。
//
// 代价是每台都要向节点探测一次状态。这个代价必须付：不同虚拟机的运行态
// 本来就不同，用一个共享的状态去判断会让其中一部分必然判错。
func (s *Service) Batch(
	ctx context.Context, req BatchRequest,
	v authz.Viewer, operatorName, clientIP string,
) (*BatchResult, error) {
	if len(req.VMIDs) == 0 {
		return nil, api.InvalidParameter("请至少选择一台虚拟机")
	}
	if len(req.VMIDs) > maxBatchSize {
		return nil, api.InvalidParameter(
			"一次最多操作 " + strconv.Itoa(maxBatchSize) + " 台，请分批执行")
	}

	// 动作本身先校验一次：如果整个动作都是非法的（比如拼错了），
	// 没有必要把它对每一台各失败一次。
	isDelete := req.Action == BatchActionDelete
	if isDelete {
		switch req.DiskAction {
		case DiskActionDelete, DiskActionKeep:
		default:
			return nil, api.InvalidParameter(
				"必须选择磁盘处理方式：delete（连同磁盘删除）或 keep（保留磁盘）")
		}
	} else if _, err := ParsePowerAction(req.Action); err != nil {
		return nil, err
	}

	return s.runBatch(req.VMIDs, func(id int64) (int64, error) {
		if isDelete {
			t, err := s.Delete(ctx, id, DeleteRequest{DiskAction: req.DiskAction},
				v, operatorName, clientIP)
			if err != nil {
				return 0, err
			}
			return t.ID, nil
		}
		t, err := s.Power(ctx, id, req.Action, v, operatorName, clientIP)
		if err != nil {
			return 0, err
		}
		return t.ID, nil
	})
}

// runBatch 逐台受理并汇总。
//
// 抽出来是因为开机与删除的批量语义完全一致（逐台独立、可部分成功、失败给
// 原因），差异只在「每台调用哪个单台入口」这一个函数参数上。
func (s *Service) runBatch(
	vmIDs []int64, op func(id int64) (int64, error),
) (*BatchResult, error) {
	resp := &BatchResult{Items: make([]BatchItem, 0, len(vmIDs))}
	// 去重：界面上不太可能重复传同一个 ID，但一个重复的 ID 会产生两个任务，
	// 第二个必然因为「状态已变」而失败——用户看到一条莫名的失败记录。
	seen := make(map[int64]bool, len(vmIDs))

	for _, id := range vmIDs {
		if seen[id] {
			continue
		}
		seen[id] = true

		item := BatchItem{VMID: id}
		taskID, err := op(id)
		if err != nil {
			item.Error = userMessage(err)
			resp.Failed++
		} else {
			item.OK = true
			item.TaskID = taskID
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
