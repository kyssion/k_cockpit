package vm

import (
	"context"
	"fmt"
	"strings"

	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
)

// MaxBatchClone 是一次能克隆的最大台数。
//
// 5 台是一个**与存储 IO 有关**的限制，不是拍脑袋：克隆一台 40 GB 的机器要
// 完整读一遍父盘再写一份新的，而用户点一次「克隆 20 台」会让这台宿主机上的
// 存储被同一批任务持续占满几分钟到几十分钟。表现在界面上是**什么都变慢了**
// ——虚拟机 IO 迟滞、别的任务的进度条不动——而用户**不会把这两件事联系
// 起来**，因为"我只是点了克隆"。
//
// 因此上限之外还有一条：这些任务在队列里是**串行**的（同一资源的锁），
// 不会同时打满。
const MaxBatchClone = 5

// BatchCloneRequest 是批量克隆的请求。
//
// 它复用 CreateRequest 的全部字段：批量克隆就是"同一套参数建 N 台"，
// 而另写一份字段会让两处的校验规则分叉——那时单一克隆能过的参数在批量里
// 被拒，而报错说不清为什么。
type BatchCloneRequest struct {
	CreateRequest
	// NamePrefix 是名称前缀，实际名称为 `前缀-1`、`前缀-2`…
	//
	// 必须有可预期的规则：不编号的话（比如都叫同名 + 系统自动去重），
	// 用户克隆 5 台之后**分不清哪台是哪台**。
	NamePrefix string
	// Count 是要克隆的台数。
	Count int
}

// BatchCloneItem 是单台的结果。
type BatchCloneItem struct {
	Name string `json:"name"`
	// TaskID 为 0 表示这一台没有建成。
	TaskID int64 `json:"task_id,omitempty"`
	// Reason 说明失败原因（仅失败时有）。
	Reason string `json:"reason,omitempty"`
}

// BatchCloneResult 是批量克隆的结果。
type BatchCloneResult struct {
	Created []BatchCloneItem `json:"created"`
	Failed  []BatchCloneItem `json:"failed"`
	// Message 是一句总结。
	Message string `json:"message"`
}

// BatchClone 一次克隆多台虚拟机。
//
// 三处刻意的处理：
//
//  1. **先整批校验名称，再逐台创建。** 逐台"遇到重名就跳过"会建出
//     `前缀-1`、`前缀-3`、`前缀-4` 这种**带洞**的结果——而用户看不出
//     少了哪一台，只会觉得"数量不对"。
//
//  2. **部分失败如实报告**，而不是整批回滚。第 3 台因为配额不足失败时，
//     前两台**已经建好了**——而"回滚"意味着删掉它们，那会连带删掉可能已经
//     分发给用户的机器。报告比回滚更安全。
//
//  3. **任务逐台入队**。队列按资源锁串行化同节点上的操作，因此这 N 台不会
//     同时打满存储——复用现成的机制比在这里自己写并发控制可靠。
func (s *Service) BatchClone(
	ctx context.Context, req BatchCloneRequest,
	owner authz.Viewer, operatorName, clientIP string,
) (*BatchCloneResult, error) {
	prefix := strings.TrimSpace(req.NamePrefix)
	if prefix == "" {
		return nil, api.InvalidParameter("必须填写名称前缀")
	}
	if req.Count <= 0 {
		return nil, api.InvalidParameter("克隆台数必须大于 0")
	}
	if req.Count > MaxBatchClone {
		// 理由要说出来，而不只是拒绝：用户不知道为什么是 5。
		return nil, api.ValidationFailed(fmt.Sprintf(
			"一次最多克隆 %d 台。每台都要完整读一遍父盘再写一份新的，同时进行的"+
				"台数越多，这台宿主机上的存储被占得越久——表现为**所有**虚拟机的"+
				"IO 都变慢，而那时很难把它和「我刚才点了克隆」联系起来。"+
				"需要更多台请分批做。", MaxBatchClone))
	}

	names := make([]string, 0, req.Count)
	for i := 1; i <= req.Count; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		if !namePattern.MatchString(name) {
			return nil, api.InvalidParameter(fmt.Sprintf(
				"生成的名称 %q 不合法：虚拟机名需为 1-63 位字母、数字或连字符，"+
					"且以字母或数字开头。请换一个更短的前缀", name))
		}
		names = append(names, name)
	}

	// 见过约束一的说明：整批先查重。
	var existing []string
	if err := s.db.WithContext(ctx).Model(&modelVMForBatch{}).
		Where("name IN ?", names).Pluck("name", &existing).Error; err != nil {
		return nil, api.Internal()
	}
	if len(existing) > 0 {
		return nil, api.Conflict(fmt.Sprintf(
			"名称已被占用：%s。批量克隆要求这一批名称**整批可用**——"+
				"逐台跳过重名会建出带洞的结果（比如只有 1、3、4 号），"+
				"而你看不出少了哪一台。请换一个前缀。",
			strings.Join(existing, "、")))
	}

	out := &BatchCloneResult{Created: []BatchCloneItem{}, Failed: []BatchCloneItem{}}
	for _, name := range names {
		// 每台单独构造请求：CreateRequest 里有 Name，而其余参数整批相同。
		one := req.CreateRequest
		one.Name = name

		t, err := s.Create(ctx, one, owner, operatorName, clientIP)
		if err != nil {
			// 见约束二：**部分失败如实报告**，不回滚前面已经成功的。
			out.Failed = append(out.Failed, BatchCloneItem{Name: name, Reason: errMessage(err)})
			continue
		}
		out.Created = append(out.Created, BatchCloneItem{Name: name, TaskID: t.ID})
	}

	switch {
	case len(out.Failed) == 0:
		out.Message = fmt.Sprintf("已提交 %d 台的克隆任务，它们在队列里依次执行", len(out.Created))
	case len(out.Created) == 0:
		out.Message = "全部失败，没有产生任何任务"
	default:
		// **这句必须说清"已经建好的那几台怎么办"**：用户看到"部分失败"时
		// 第一个问题是"那前面那几台还算数吗"。
		out.Message = fmt.Sprintf("成功 %d 台、失败 %d 台。**已成功的那几台不会被撤销**"+
			"——它们已经建好了，撤销意味着删掉可能已经分发出去了的机器。"+
			"失败的那些请按原因处理后重试。", len(out.Created), len(out.Failed))
	}
	return out, nil
}

// modelVMForBatch 只用于查名——避免为一个 count 查询引入整表模型。
type modelVMForBatch struct {
	Name string `gorm:"column:name"`
}

// TableName 固定表名。
func (modelVMForBatch) TableName() string { return "vm" }

func errMessage(err error) string {
	if apiErr, ok := err.(*api.Error); ok {
		return apiErr.Message
	}
	return err.Error()
}
