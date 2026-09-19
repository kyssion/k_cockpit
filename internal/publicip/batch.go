package publicip

import (
	"context"
	"fmt"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// MaxBatch 是一次批量操作的上限。
//
// 50：每个地址都要下发一条 NAT 规则，而**一次几百条**会让这台宿主机上的
// 网络配置在几秒内连续变更几百次——期间正在连的会话可能因为中间状态而中断，
// 而用户只会看到"批量操作以后有些人连不上了"，看不出与这次操作的关系。
const MaxBatch = 50

// BatchItemResult 是批量里单条的结果。
type BatchItemResult struct {
	ID int64 `json:"id"`
	// Address 是这一条的地址，让用户能对上号。
	//
	// 只给 ID 的话，失败清单里一列数字他看不出是哪几个——而那正是他要处理
	// 的东西。
	Address string `json:"address,omitempty"`
	// TaskID 为 0 表示这一条没有成功。
	TaskID int64  `json:"task_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// BatchResult 是批量操作的结果。
type BatchResult struct {
	OK      []BatchItemResult `json:"ok"`
	Failed  []BatchItemResult `json:"failed"`
	Message string            `json:"message"`
}

// BatchUnbind 批量解绑。
//
// **逐条调用单条方法**，而不是在这里重写一遍解绑逻辑：那样两处的校验迟早
// 会分叉，而分叉的表现是"单个能解绑、批量里解不了"——用户会以为批量功能
// 有问题。
func (s *Service) BatchUnbind(
	ctx context.Context, ids []int64,
	v authz.Viewer, operatorName, clientIP string,
) (*BatchResult, error) {
	if len(ids) == 0 {
		return nil, api.InvalidParameter("没有选中任何地址")
	}
	if len(ids) > MaxBatch {
		return nil, api.ValidationFailed(fmt.Sprintf(
			"一次最多操作 %d 个地址。每个地址都要下发一条 NAT 规则，一次几百条会让"+
				"宿主机的网络配置在几秒内连续变更——期间正在连的会话可能因为中间状态"+
				"而中断，而那时你只会看到「批量操作之后有些人连不上了」。", MaxBatch))
	}

	out := &BatchResult{OK: []BatchItemResult{}, Failed: []BatchItemResult{}}
	for _, id := range ids {
		t, err := s.Unbind(ctx, id, v, operatorName, clientIP)
		item := BatchItemResult{ID: id}
		if err != nil {
			item.Reason = errText(err)
			out.Failed = append(out.Failed, item)
			continue
		}
		item.TaskID = t.ID
		out.OK = append(out.OK, item)
	}
	out.Message = batchMessage("解绑", len(out.OK), len(out.Failed))
	return out, nil
}

// BatchBind 把一个或多个地址绑到**同一台**虚拟机上。
//
// 只支持"多个地址 → 一台机器"这一个方向，而不是任意组合：绑定需要地址与
// 虚拟机**在同一节点**，而任意组合会让用户在面对一台失败时无法判断
// 是"地址不对"还是"机器不对"。
func (s *Service) BatchBind(
	ctx context.Context, ids []int64, vmID int64, mode string,
	v authz.Viewer, operatorName, clientIP string,
) (*BatchResult, error) {
	if len(ids) == 0 {
		return nil, api.InvalidParameter("没有选中任何地址")
	}
	if vmID <= 0 {
		return nil, api.InvalidParameter("必须指定目标虚拟机")
	}
	if len(ids) > MaxBatch {
		return nil, api.ValidationFailed(fmt.Sprintf("一次最多操作 %d 个地址", MaxBatch))
	}

	out := &BatchResult{OK: []BatchItemResult{}, Failed: []BatchItemResult{}}
	for _, id := range ids {
		t, err := s.Bind(ctx, BindRequest{PublicIPID: id, VMID: vmID, Mode: mode},
			v, operatorName, clientIP)
		item := BatchItemResult{ID: id}
		if err != nil {
			item.Reason = errText(err)
			out.Failed = append(out.Failed, item)
			continue
		}
		item.TaskID = t.ID
		out.OK = append(out.OK, item)
	}
	out.Message = batchMessage("绑定", len(out.OK), len(out.Failed))
	return out, nil
}

// batchMessage 给出一句总结。
//
// **部分失败时要说清"已经做好的那几条怎么办"**：用户看到"部分失败"时第一个
// 问题就是"那前面那几条还算数吗"。答案是不回滚——回滚意味着把已经下发好的
// 规则再撤掉，而那会让一次失败的批量操作变成两次网络变更。
func batchMessage(verb string, ok, failed int) string {
	switch {
	case failed == 0:
		return fmt.Sprintf("%d 个地址已提交%s", ok, verb)
	case ok == 0:
		return "全部失败，没有产生任何变更"
	default:
		return fmt.Sprintf("%s成功 %d 个、失败 %d 个。**已成功的那几条不会被撤销**"+
			"——它们已经生效了，撤销意味着再做一次网络变更。失败的那些请按原因处理后重试。",
			verb, ok, failed)
	}
}

func errText(err error) string {
	if apiErr, ok := err.(*api.Error); ok {
		return apiErr.Message
	}
	return err.Error()
}
