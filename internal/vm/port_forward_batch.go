package vm

import (
	"context"
	"fmt"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// MaxPortForwardBatch 是一次批量删除的上限。
//
// 与公网 IP 批量同一个理由：每条转发都要下发一条 NAT 规则，而一次几百条
// 会让宿主机的网络配置在几秒内连续变更——期间正在连的会话可能因为中间状态
// 中断，而用户只会看到「批量删了以后有些人连不上了」。
const MaxPortForwardBatch = 50

// PortForwardRef 指向一条端口转发。
//
// **必须带 vm_id**，不能只给转发 id：删除要做归属校验（租户只能删自己的），
// 而归属挂在虚拟机上。只凭转发 id 反查虚拟机也能做，但那会让"这条转发属于
// 谁"这件事在服务层多一次猜测——而猜错的方向是把别人的删掉。
type PortForwardRef struct {
	VMID int64 `json:"vm_id"`
	ID   int64 `json:"pf_id"`
}

// PortForwardBatchResult 是批量删除的结果。
type PortForwardBatchResult struct {
	Removed []PortForwardRef `json:"removed"`
	Failed  []struct {
		Ref    PortForwardRef `json:"ref"`
		Reason string         `json:"reason"`
	} `json:"failed"`
	Message string `json:"message"`
}

// RemovePortForwards 批量删除端口转发。
//
// **逐条调用单条方法**，而不是在这里重写删除逻辑：那样两处的校验迟早会分叉，
// 而分叉的表现是"单条能删、批量里删不了"——用户会以为批量功能有问题。
func (s *Service) RemovePortForwards(
	ctx context.Context, refs []PortForwardRef,
	v authz.Viewer, operatorName, clientIP string,
) (*PortForwardBatchResult, error) {
	if len(refs) == 0 {
		return nil, api.InvalidParameter("没有选中任何转发规则")
	}
	if len(refs) > MaxPortForwardBatch {
		return nil, api.ValidationFailed(fmt.Sprintf(
			"一次最多删除 %d 条。每条都要下发一条 NAT 规则，一次几百条会让宿主机的"+
				"网络配置在几秒内连续变更——期间正在连的会话可能因为中间状态中断。",
			MaxPortForwardBatch))
	}

	out := &PortForwardBatchResult{
		Removed: []PortForwardRef{},
		Failed: []struct {
			Ref    PortForwardRef `json:"ref"`
			Reason string         `json:"reason"`
		}{},
	}
	for _, ref := range refs {
		if _, err := s.RemovePortForward(ctx, ref.VMID, ref.ID, v, operatorName, clientIP); err != nil {
			out.Failed = append(out.Failed, struct {
				Ref    PortForwardRef `json:"ref"`
				Reason string         `json:"reason"`
			}{Ref: ref, Reason: pfErrText(err)})
			continue
		}
		out.Removed = append(out.Removed, ref)
	}

	switch {
	case len(out.Failed) == 0:
		out.Message = fmt.Sprintf("已提交 %d 条删除任务", len(out.Removed))
	case len(out.Removed) == 0:
		out.Message = "全部失败，没有产生任何变更"
	default:
		out.Message = fmt.Sprintf("成功 %d 条、失败 %d 条。**已成功的那几条不会被撤销**"+
			"——它们已经生效了，撤销意味着再做一次网络变更。失败的那些请按原因处理后重试。",
			len(out.Removed), len(out.Failed))
	}
	return out, nil
}

func pfErrText(err error) string {
	if apiErr, ok := err.(*api.Error); ok {
		return apiErr.Message
	}
	return err.Error()
}
