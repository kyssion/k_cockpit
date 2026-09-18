// Package capture 实现抓包与网络诊断（F-4-12）。
//
// 两条约束决定了本包的大部分设计：
//
//	**抓包必须有时限。**
//	不限时的抓包会把宿主机磁盘写满，而且用户会忘记停——它的失败不是
//	"没抓到"，而是"把宿主机写挂了"，而那时它已经跑了几小时。
//
//	**抓包文件必须过期消失。**
//	文件里有完整的流量内容，包括明文密码、会话令牌、内网数据。它不是
//	一份普通日志，而是"这段时间这个网口上发生的一切"。留在宿主机上越久
//	泄露面越大，而它多半是在排查完之后就被忘掉的。
//
// 第三条是安全上的：Filter 最终会被拼进 tcpdump 的命令行，因此它是一处
// **注入面**。校验必须发生在控制面——节点侧的转义是第二道防线，不是第一道。
package capture

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// 抓包时长的边界。
//
// 上限 300 秒（5 分钟）：一个抓包文件的大小大致与时长成正比，而 5 分钟的
// 满载流量已经能产生几百 MB。再长的话，用户多半不是想"看一眼"，而是想做
// 长期记录——那是另一件事（而且不该由这个功能承担，它的文件会过期消失）。
const (
	minDurationSec = 5
	maxDurationSec = 300
	// maxConcurrent 限制同一节点上同时进行的抓包数。
	//
	// 抓包本身要占 CPU 与磁盘带宽，而它是在**生产节点**上跑。允许无限个
	// 并发的话，几个人同时点"抓包"就足以影响那台机器上虚拟机的网络性能
	// ——而他们各自的界面都显示"一切正常"。
	maxConcurrent = 3
)

// Service 提供抓包能力。
type Service struct {
	db    *gorm.DB
	agent agent.Client
	audit *audit.Recorder
	queue *task.Queue
	now   func() time.Time
}

// NewService 构造服务。
func NewService(
	db *gorm.DB, client agent.Client, recorder *audit.Recorder, queue *task.Queue,
) *Service {
	return &Service{db: db, agent: client, audit: recorder, queue: queue, now: time.Now}
}

// View 是一次抓包的对外视图。
type View struct {
	ID     int64  `json:"id"`
	NodeID int64  `json:"node_id"`
	VMID   int64  `json:"vm_id,omitempty"`
	VMName string `json:"vm_name,omitempty"`

	Interface string `json:"interface,omitempty"`
	Filter    string `json:"filter,omitempty"`
	// DurationSec 是时长。**必须有值**——不限时的抓包会把磁盘写满。
	DurationSec int `json:"duration_sec"`

	// Status 是三态：running / ready / failed。
	//
	// 抓包是**异步**的，下发之后要等 duration 秒才有文件。这期间界面必须
	// 显示"进行中"，而不是"文件不存在"——后者会让用户以为抓包失败了。
	Status string `json:"status"`
	// SecondsLeft 由服务端算好，而不是让界面拿时刻去减本地时间——
	// 客户端时钟不准是常态，而"还剩 40 秒"必须可信。
	SecondsLeft int   `json:"seconds_left,omitempty"`
	SizeBytes   int64 `json:"size_bytes"`
	// ExpiresAt 是文件在节点上保留到什么时候。下载入口据此提示。
	ExpiresAt string `json:"expires_at,omitempty"`
	CreatedAt string `json:"created_at"`

	// Hint 在文件为空或失败时给出可能的原因。
	//
	// 抓到了 0 字节是一个**看不出原因**的结果，而用户会先去怀疑抓包功能
	// 坏了。最常见的原因是过滤器没匹配到任何流量。
	Hint string `json:"hint,omitempty"`
}

// Request 是发起抓包的请求。
type Request struct {
	NodeID      int64
	VMID        int64
	Interface   string
	Filter      string
	DurationSec int
}

// List 返回节点上的抓包记录。
func (s *Service) List(ctx context.Context, nodeID int64, v authz.Viewer) ([]View, error) {
	var rows []model.NetworkCapture
	q := s.db.WithContext(ctx).Where("node_id = ?", nodeID)
	// 租户只看自己发起的：抓包内容含明文流量，而一个网口上可能同时有
	// 多个租户的机器。
	if !v.IsAdmin {
		q = q.Where("created_by = ?", v.UserID)
	}
	if err := q.Order("id DESC").Find(&rows).Error; err != nil {
		log.Printf("[capture] 查询失败: %v", err)
		return nil, api.Internal()
	}
	names := s.vmNames(ctx, rows)

	out := make([]View, 0, len(rows))
	for i := range rows {
		view := s.toView(&rows[i])
		if rows[i].VMID != nil {
			view.VMName = names[*rows[i].VMID]
		}
		out = append(out, view)
	}
	return out, nil
}

// Start 发起一次抓包。
func (s *Service) Start(
	ctx context.Context, req Request, v authz.Viewer, operatorName, clientIP string,
) (*View, *model.Task, error) {
	if err := s.validate(ctx, req); err != nil {
		return nil, nil, err
	}

	row := model.NetworkCapture{
		NodeID: req.NodeID, Interface: &req.Interface,
		DurationSec: req.DurationSec, CreatedBy: &v.UserID,
	}
	if req.VMID > 0 {
		row.VMID = &req.VMID
	}
	if req.Filter != "" {
		row.Filter = &req.Filter
	}
	expires := s.now().Add(s.retention())
	row.ExpiresAt = &expires

	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[capture] 创建失败: %v", err)
		return nil, nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskNetworkCapture,
		NodeID:       req.NodeID,
		ResourceType: "network_capture",
		ResourceID:   row.ID,
		ResourceName: req.Interface,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"capture_id":   row.ID,
			"interface":    req.Interface,
			"filter":       req.Filter,
			"duration_sec": req.DurationSec,
		},
	})
	if err != nil {
		log.Printf("[capture] 入队失败: %v", err)
		return nil, nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: req.NodeID, ResourceType: "network_capture",
		ResourceID: row.ID, ResourceName: req.Interface,
		Action: "network_capture.start",
		Params: map[string]any{
			"interface": req.Interface, "filter": req.Filter,
			"duration_sec": req.DurationSec,
			// 抓包会读到一个网口上的**全部流量**，包括别人的。谁在什么时候
			// 抓了哪个网口，是这块功能最需要留痕的一件事。
			"vm_id": req.VMID,
		},
		Success: true, ClientIP: clientIP,
	})

	view := s.toView(&row)
	return &view, t, nil
}

// Delete 删除一次抓包（文件与控制面记录）。
func (s *Service) Delete(
	ctx context.Context, id int64, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	var row model.NetworkCapture
	if err := s.db.WithContext(ctx).First(&row, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("抓包记录不存在")
		}
		return nil, api.Internal()
	}
	// 租户只能删自己发起的。
	if !v.IsAdmin && (row.CreatedBy == nil || *row.CreatedBy != v.UserID) {
		return nil, api.NotFound("抓包记录不存在")
	}

	params := map[string]any{
		"capture_id": row.ID,
		"file_path":  derefStr(row.FilePath),
	}
	// 文件还没生成时不必让节点去删——记录直接删掉即可。
	// 但**文件已生成时一定要下发删除**：只删控制面记录会让一份含明文流量
	// 的文件永远留在宿主机上，而界面上已经看不到它了。
	if !row.Ready() {
		if err := s.db.WithContext(ctx).Delete(&model.NetworkCapture{}, row.ID).Error; err != nil {
			return nil, api.Internal()
		}
		s.record(ctx, audit.Entry{
			OperatorID: v.UserID, OperatorName: operatorName,
			NodeID: row.NodeID, ResourceType: "network_capture",
			ResourceID: row.ID, Action: "network_capture.delete",
			Success: true, ClientIP: clientIP,
		})
		return nil, nil
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskNetworkCaptureDelete,
		NodeID:       row.NodeID,
		ResourceType: "network_capture",
		ResourceID:   row.ID,
		ResourceName: derefStr(row.Interface),
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params:       params,
	})
	if err != nil {
		return nil, api.Internal()
	}
	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: row.NodeID, ResourceType: "network_capture",
		ResourceID: row.ID, Action: "network_capture.delete",
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// --- 内部 ---

func (s *Service) validate(ctx context.Context, req Request) error {
	// 节点必须存在。**不能只靠 agent 报错**：它不知道控制面的节点表，
	// 对不存在的节点也会照常返回，于是抓包会"成功"打到一台并不存在的机器上。
	var node model.Node
	if err := s.db.WithContext(ctx).Select("id").First(&node, req.NodeID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return api.NotFound("节点不存在")
		}
		return api.Internal()
	}
	if strings.TrimSpace(req.Interface) == "" {
		return api.InvalidParameter("必须指定要抓包的网口")
	}
	if req.DurationSec <= 0 {
		return api.InvalidParameter("必须指定抓包时长")
	}
	if req.DurationSec < minDurationSec {
		return api.InvalidParameter(fmt.Sprintf("抓包时长不能少于 %d 秒——太短的抓包多半什么都抓不到", minDurationSec))
	}
	if req.DurationSec > maxDurationSec {
		return api.InvalidParameter(fmt.Sprintf(
			"抓包时长不能超过 %d 秒。更长时间的记录请用别的方式："+
				"抓包文件会把宿主机磁盘写满，而它的失败不是「没抓到」而是「把宿主机写挂了」",
			maxDurationSec))
	}

	// **过滤器校验是这里最要紧的一步。**
	//
	// Filter 最终会被拼进 tcpdump 的命令行，因此它是一处注入面。校验放在
	// 控制面而不是只靠节点侧转义：节点侧的转义是第二道防线，不是第一道，
	// 而"只靠下游小心处理"这种安排，在下游某次重构之后就悄悄失效了。
	if err := validateFilter(req.Filter); err != nil {
		return err
	}

	// 节点上同时进行的抓包数。
	var running int64
	if err := s.db.WithContext(ctx).Model(&model.NetworkCapture{}).
		Where("node_id = ? AND (file_path IS NULL OR file_path = '')",
			req.NodeID).Count(&running).Error; err != nil {
		log.Printf("[capture] 统计并发抓包失败: %v", err)
		return api.Internal()
	}
	if running >= maxConcurrent {
		return api.Conflict(fmt.Sprintf(
			"该节点上已有 %d 个抓包在进行中，同时抓包会占用节点 CPU 与磁盘带宽，"+
				"进而影响那台机器上虚拟机的网络性能。请等其中一个完成。", running))
	}
	return nil
}

// filterForbidden 是过滤器中**不允许出现**的字符。
//
// BPF 表达式本身不需要它们中的任何一个，而它们在命令行与 shell 语境下
// 都有特殊含义。**取交集而不是逐个判断危险性**：少一种可表达的写法，
// 就少一整类绕过方式（与目录共享只收相对路径是同一个思路）。
var filterForbidden = []string{
	"`", "$", ";", "\\", "'", "\"", "\n", "\r", "\t",
	">", "<", "|", "&", "(", ")", "{", "}", "*", "?", "!",
}

func validateFilter(expr string) error {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		// 空的过滤器意味着**抓全部流量**。这是合法且常见的用法
		// （"我就想看看这个网口上到底有什么"），因此不拒绝。
		return nil
	}
	if len(expr) > 200 {
		return api.InvalidParameter("过滤器最长 200 字符")
	}
	for _, bad := range filterForbidden {
		if strings.Contains(expr, bad) {
			return api.InvalidParameter(
				"过滤器不能包含 " + bad + " —— BPF 表达式不需要这个字符，" +
					"而它在命令上下文里有特殊含义")
		}
	}
	// 只允许「字母数字 + 空白 + 少量分隔符」，黑白名单同时用：
	// 黑名单防的是已知的注入写法，白名单防的是还没想到的那些。
	for _, r := range expr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == ' ' || r == '.' || r == ':' || r == '/' || r == '-' || r == '_' || r == ',':
		default:
			return api.InvalidParameter(fmt.Sprintf(
				"过滤器不允许出现 %q。只支持 tcpdump 的基本表达式，"+
					"例如 tcp port 80、host 10.0.0.5、udp portrange 1000-2000", r))
		}
	}
	return nil
}

// retention 是抓包文件的保留时长。
//
// 24 小时：足够"今天抓、明天上班看"，又不会让含明文流量的文件长期留在
// 宿主机上。它多半是在排查完之后被忘掉的，因此这个期限必须存在，
// 而且**由节点侧强制执行**——控制面说"该删了"而文件在别人的磁盘上，
// 那只是一句期望。
func (s *Service) retention() time.Duration { return 24 * time.Hour }

func (s *Service) toView(c *model.NetworkCapture) View {
	v := View{
		ID: c.ID, NodeID: c.NodeID, DurationSec: c.DurationSec,
		SizeBytes: c.SizeBytes,
		CreatedAt: c.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if c.VMID != nil {
		v.VMID = *c.VMID
	}
	v.Interface = derefStr(c.Interface)
	v.Filter = derefStr(c.Filter)
	if c.ExpiresAt != nil {
		v.ExpiresAt = c.ExpiresAt.Format("2006-01-02T15:04:05Z07:00")
	}

	if c.Ready() {
		v.Status = "ready"
		if c.SizeBytes == 0 {
			// 抓到了 0 字节是一个**看不出原因**的结果，而用户会先去怀疑
			// 抓包功能坏了。把最常见的原因直接说出来。
			v.Hint = "文件为空：过滤器可能没有匹配到任何流量。可以先用空过滤器确认这个网口上有没有流量。"
		}
	} else {
		// 抓包是异步的，还在跑。
		v.Status = "running"
		elapsed := s.now().Sub(c.CreatedAt)
		left := time.Duration(c.DurationSec)*time.Second - elapsed
		if left < 0 {
			left = 0
		}
		v.SecondsLeft = int(left.Seconds())
	}
	return v
}

func (s *Service) vmNames(ctx context.Context, rows []model.NetworkCapture) map[int64]string {
	ids := []int64{}
	for i := range rows {
		if rows[i].VMID != nil {
			ids = append(ids, *rows[i].VMID)
		}
	}
	out := map[int64]string{}
	if len(ids) == 0 {
		return out
	}
	var vms []model.VM
	if err := s.db.WithContext(ctx).Select("id", "name").
		Where("id IN ?", ids).Find(&vms).Error; err != nil {
		log.Printf("[capture] 查询虚拟机名失败: %v", err)
		return out
	}
	for _, vm := range vms {
		out[vm.ID] = vm.Name
	}
	return out
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
