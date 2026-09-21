// Package vmtag 实现虚拟机标签（F-2-16）。
//
// 标签与分组解决的不是同一件事，两者都保留：
//
//	**分组**是互斥的（一台机器只属于一个组），回答"它在哪一批里"；
//	**标签**是非互斥的（可以有任意多个），回答"它有哪些属性"。
//
// 合并成一个字段会立刻遇到矛盾：一台机器既是"生产"又是"数据库"，而分组
// 只能放一个；全靠标签又会失去"这批机器一共几台"这种可求和的视图。
//
// 一处刻意的设计：**标签是整体替换，不是逐个增删**。
//
// 界面上编辑标签的方式是一个输入框（多个标签用逗号分隔），用户改完直接
// 保存。如果接口是 add/remove 逐个操作，界面就得为每一次输入计算差异——
// 而"加了又删、删了又加"的中间态会被如实写进数据库与审计流水，让那条
// 记录读起来很难理解。整体替换让一条审计记录就说完了一件事。
package vmtag

import (
	"context"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
)

// Service 提供标签能力。
type Service struct {
	db    *gorm.DB
	audit *audit.Recorder
}

// NewService 构造服务。
func NewService(db *gorm.DB, recorder *audit.Recorder) *Service {
	return &Service{db: db, audit: recorder}
}

// Tags 返回一台虚拟机的标签（已排序）。
// TagsOf 一次读取多台虚拟机的标签。
//
// 存在的理由就是**避免 N+1**：列表一页 20 台，用单台的 Tags 循环调用会把
// 一次列表请求变成 20 次数据库往返。(vm_id) 上有索引，一次 IN 查询的代价
// 与查一台几乎相同。
func (s *Service) TagsOf(ctx context.Context, vmIDs []int64) (map[int64][]string, error) {
	out := make(map[int64][]string)
	if len(vmIDs) == 0 {
		return out, nil
	}

	var rows []model.VMTag
	if err := s.db.WithContext(ctx).
		Where("vm_id IN ?", vmIDs).
		Order("vm_id, tag").Find(&rows).Error; err != nil {
		log.Printf("[vmtag] 批量查询标签失败: %v", err)
		return nil, api.Internal()
	}
	// 结果不包含"没有标签"的机器——调用方按 map 缺失处理，而不是塞一个空
	// 切片：空切片与"没查到"在 JSON 里都是 []，但语义不同。

	for i := range rows {
		out[rows[i].VMID] = append(out[rows[i].VMID], rows[i].Tag)
	}
	return out, nil
}

func (s *Service) Tags(ctx context.Context, vmID int64, v authz.Viewer) ([]string, error) {
	if err := s.ensureVM(ctx, vmID, v); err != nil {
		return nil, err
	}
	return s.tagsOf(ctx, vmID)
}

// SetTags 整体替换一台虚拟机的标签。
func (s *Service) SetTags(
	ctx context.Context, vmID int64, tags []string, v authz.Viewer,
	operatorName, clientIP string,
) ([]string, error) {
	vm, err := s.loadVM(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	normalized, err := normalize(tags)
	if err != nil {
		return nil, err
	}
	before, err := s.tagsOf(ctx, vmID)
	if err != nil {
		return nil, err
	}

	// 替换放在一个事务里：分两步会出现「旧的已删、新的没写」的瞬间，
	// 而那段时间里这台机器没有任何标签——从界面上看就是"标签丢了"。
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("vm_id = ?", vmID).Delete(&model.VMTag{}).Error; err != nil {
			return err
		}
		if len(normalized) == 0 {
			return nil
		}
		rows := make([]model.VMTag, 0, len(normalized))
		for _, t := range normalized {
			rows = append(rows, model.VMTag{VMID: vmID, Tag: t})
		}
		return tx.Create(&rows).Error
	})
	if err != nil {
		log.Printf("[vmtag] 保存标签失败 vm=%d: %v", vmID, err)
		return nil, api.Internal()
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "vm.tags.set",
		Params:      map[string]any{"tags": normalized},
		BeforeState: map[string]any{"tags": before},
		AfterState:  map[string]any{"tags": normalized},
		Success:     true, ClientIP: clientIP,
	})
	return normalized, nil
}

// VMsByTag 返回带某个标签的虚拟机 id。
//
// 之所以要这个接口而不是让界面自己过滤：标签是服务端数据，而"哪些机器带
// 这个标签"在机器很多时不可能靠前端筛——列表是分页的，前端只拿得到当前页。
func (s *Service) VMsByTag(ctx context.Context, tag string, v authz.Viewer) ([]int64, error) {
	t := strings.TrimSpace(tag)
	if t == "" {
		return nil, api.InvalidParameter("必须指定标签")
	}

	query := s.db.WithContext(ctx).Model(&model.VMTag{}).Where("tag = ?", t)
	if !v.IsAdmin {
		// 租户只能看到自己名下机器的标签——否则他能通过标签反查出别人的
		// 机器数量与分布。
		query = query.Where(
			"vm_id IN (?)",
			s.db.Model(&model.VM{}).Select("id").Where("owner_id = ?", v.UserID))
	}

	var ids []int64
	if err := query.Pluck("vm_id", &ids).Error; err != nil {
		log.Printf("[vmtag] 按标签查询失败: %v", err)
		return nil, api.Internal()
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids, nil
}

// AllTags 返回该调用者**可见范围内**出现过的全部标签及使用次数。
//
// 返回计数而不是纯列表：界面上"生产 (12)"比"生产"有用得多——它能让人
// 判断这个标签是不是一个只有一台机器的孤儿标签。
func (s *Service) AllTags(ctx context.Context, v authz.Viewer) ([]TagCount, error) {
	var rows []TagCount
	query := s.db.WithContext(ctx).Model(&model.VMTag{}).
		Select("tag, count(*) AS count").Group("tag").Order("count DESC, tag ASC")
	if !v.IsAdmin {
		query = query.Where(
			"vm_id IN (?)",
			s.db.Model(&model.VM{}).Select("id").Where("owner_id = ?", v.UserID))
	}
	if err := query.Scan(&rows).Error; err != nil {
		log.Printf("[vmtag] 查询标签汇总失败: %v", err)
		return nil, api.Internal()
	}
	if rows == nil {
		rows = []TagCount{}
	}
	return rows, nil
}

// TagCount 是一个标签及其使用次数。
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// --- 内部 ---

// normalize 规范化传入的标签列表。
func normalize(tags []string) ([]string, error) {
	out := make([]string, 0, len(tags))
	seen := map[string]bool{}
	for _, raw := range tags {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if len([]rune(t)) > model.MaxTagLen {
			return nil, api.InvalidParameter(
				"标签「" + t + "」过长（最多 " +
					strconv.Itoa(model.MaxTagLen) + " 个字符）")
		}
		// 逗号是界面上用来分隔多个标签的字符，让标签自己含逗号会让"一个
		// 标签"与"两个标签"在回显时分不清。
		if strings.ContainsAny(t, ",\n\r") {
			return nil, api.InvalidParameter("标签不能包含逗号或换行：" + t)
		}
		// 去重：`uniq_vm_tag_vm_tag` 会拦住重复，但那时错误信息是一句
		// 唯一约束冲突，与"你写了两个一样的标签"联系不起来。
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	// 排序让同一组标签每次渲染一致——顺序会变会让界面看起来在闪烁。
	sort.Strings(out)
	return out, nil
}

func (s *Service) tagsOf(ctx context.Context, vmID int64) ([]string, error) {
	var tags []string
	if err := s.db.WithContext(ctx).Model(&model.VMTag{}).
		Where("vm_id = ?", vmID).Order("tag ASC").Pluck("tag", &tags).Error; err != nil {
		log.Printf("[vmtag] 查询标签失败: %v", err)
		return nil, api.Internal()
	}
	if tags == nil {
		tags = []string{}
	}
	return tags, nil
}

func (s *Service) loadVM(ctx context.Context, id int64, v authz.Viewer) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", id).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("虚拟机不存在")
		}
		log.Printf("[vmtag] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	// 404 而非 403：403 会确认「这个 ID 存在」，让租户能通过枚举推断出
	// 别人有多少台机器。
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("虚拟机不存在")
	}
	return &vm, nil
}

func (s *Service) ensureVM(ctx context.Context, id int64, v authz.Viewer) error {
	_, err := s.loadVM(ctx, id, v)
	return err
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.audit == nil {
		return
	}
	s.audit.Record(ctx, e)
}
