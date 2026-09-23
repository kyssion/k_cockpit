package storage

import (
	"context"
	"errors"
	"log"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gorm.io/gorm"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// tagRe 限定 tag 的字符集。
//
// 它会被写进 qemu 的 `-virtfs local,path=...,mount_tag=<tag>` 参数，也会
// 出现在来宾的挂载命令里。允许空白、引号、分号这类字符等于把两台机器上的
// 命令解析器都交给用户——tag 冒号、逗号、空格都会让 qemu 的参数串被拆错，
// 而拆错的结果是把后半段当成另一个参数，行为与预期完全无关。
var tagRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// ShareView 是一条目录共享的对外视图。
type ShareView struct {
	ID     int64 `json:"id"`
	VMID   int64 `json:"vm_id"`
	NodeID int64 `json:"node_id"`
	// RelPath 是**相对于用户存储根**的路径，也就是用户当初填的那个。
	RelPath string `json:"rel_path"`
	// HostPath 是拼出来的绝对路径。
	//
	// 一并返回是刻意的：用户需要在虚拟机的 fstab 或文档里写清楚这个共享
	// 实际对应宿主机的哪里，而如果只给相对路径，他得自己去拼——拼错的
	// 结果是他以为共享的是 A 目录，实际共享的是 B。
	HostPath string `json:"host_path"`
	Tag      string `json:"tag"`

	SecurityModel string `json:"security_model"`
	ReadOnly      bool   `json:"read_only"`
	Status        string `json:"status"`
	MountedAt     string `json:"mounted_at,omitempty"`
	CreatedAt     string `json:"created_at"`
}

// MountShareRequest 是挂载一个目录共享的请求。
type MountShareRequest struct {
	VMID int64
	// RelPath 是**相对于调用者存储根**的路径。
	//
	// 相对路径而不是绝对路径，是整个功能的安全前提——见 model.ShareMount
	// 的说明。接口层不接受绝对路径，因此用户根本没有机会指向 /etc。
	RelPath string
	// Tag 留空时由服务端按目录名生成。
	Tag           string
	SecurityModel string
	ReadOnly      *bool
}

// ListShares 返回一台虚拟机的目录共享。
func (s *Service) ListShares(ctx context.Context, vmID int64, v authz.Viewer) ([]ShareView, error) {
	vm, err := s.loadVMForShare(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	var rows []model.ShareMount
	if err := s.db.WithContext(ctx).
		Where("vm_id = ?", vmID).Order("created_at ASC").Find(&rows).Error; err != nil {
		log.Printf("[storage] 查询目录共享失败: %v", err)
		return nil, api.Internal()
	}

	// 存储根只查一次。取不到时 rel_path 回落到绝对路径（见 relPathOf）——
	// 列表不该因为这一个字段查不到就整个失败，而显示一个空串会让用户
	// 以为共享的是根目录，那与事实不符。
	root := ""
	if vm.OwnerID != nil {
		if r, err := s.storageRoot(ctx, *vm.OwnerID, vm.NodeID); err == nil {
			root = r
		} else {
			log.Printf("[storage] 读取存储根失败 vm=%d: %v", vm.ID, err)
		}
	}

	views := make([]ShareView, 0, len(rows))
	for i := range rows {
		views = append(views, toShareView(&rows[i], root))
	}
	return views, nil
}

// MountShare 把一个宿主机目录共享给虚拟机（F-5-06）。
func (s *Service) MountShare(
	ctx context.Context, req MountShareRequest, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.loadVMForShare(ctx, req.VMID, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}

	// 存储根必须存在：没有它就无法把相对路径拼成绝对路径，也就无法保证
	// 拼出来的东西落在用户自己的空间里。
	//
	// 用**虚拟机属主**而不是调用者的 id：管理员代管别人的机器时，共享出来
	// 的目录应当来自那台机器的属主空间，而不是管理员自己的。否则一次代管
	// 操作就能把管理员空间里的任何目录暴露给租户。
	if vm.OwnerID == nil {
		return nil, api.ValidationFailed("该虚拟机没有属主，无法确定共享目录的来源")
	}
	root, err := s.storageRoot(ctx, *vm.OwnerID, vm.NodeID)
	if err != nil {
		return nil, err
	}

	hostPath, err := resolveUnderRoot(root, req.RelPath)
	if err != nil {
		return nil, err
	}

	tag := strings.TrimSpace(req.Tag)
	if tag == "" {
		// 默认用目录名做 tag：用户在来宾里 `mount -t 9p <目录名>` 时
		// 一眼能对上是哪个共享。目录名为空或含非法字符时拒绝，而不是
		// 硬凑一个——凑出来的 tag 用户猜不到，只能去页面里查。
		// hostPath 是被管宿主机上的 POSIX 路径，所以取目录名用 path 而不是
		// filepath：后者按控制面本机的分隔符计算，Windows 上会把整串路径
		// 当成一个文件名，默认 tag 因此永远不合法。
		tag = path.Base(hostPath)
	}
	if !tagRe.MatchString(tag) {
		return nil, api.InvalidParameter(
			"tag 只能由字母、数字、下划线、点和短横线组成，且以字母或数字开头（最长 64 位）")
	}

	securityModel := req.SecurityModel
	if securityModel == "" {
		securityModel = model.ShareSecurityMapped
	}
	if !model.ValidShareSecurityModel(securityModel) {
		return nil, api.InvalidParameter("不支持的安全模型：" + securityModel)
	}
	// passthrough / none 会让来宾里的 root 直接在宿主机上写出属于 root 的文件，
	// 共享目录与宿主机之间因此不再有权限边界。这是用户能主动选择的最危险的
	// 一档，因此要显式确认一次。
	if securityModel != model.ShareSecurityMapped && !v.IsAdmin {
		return nil, api.ValidationFailed(
			"安全模型「" + securityModel + "」会让虚拟机里的 root 直接以 root 身份写宿主机文件，" +
				"仅管理员可选用；普通用户请使用 mapped")
	}

	// 只读默认 true：可写共享里，来宾的写入**不受控制面的配额约束**
	// ——配额是控制面受理请求时算的，而来宾绕过控制面直接写盘。
	readOnly := true
	if req.ReadOnly != nil {
		readOnly = *req.ReadOnly
	}

	// 同一 tag 已存在时拒绝，并把话说到「该怎么办」。
	var existing model.ShareMount
	err = s.db.WithContext(ctx).
		Where("vm_id = ? AND tag = ?", vm.ID, tag).First(&existing).Error
	if err == nil {
		return nil, api.Conflict(
			"该虚拟机已有 tag 为「" + tag + "」的共享；请换一个 tag 或先卸载它")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("[storage] 查询共享失败: %v", err)
		return nil, api.Internal()
	}

	row := model.ShareMount{
		VMID: vm.ID, NodeID: vm.NodeID, HostPath: hostPath, Tag: tag,
		SecurityModel: securityModel, ReadOnly: readOnly,
	}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		if isDuplicateKey(err) {
			return nil, api.Conflict("该 tag 已被这台虚拟机的另一个共享占用")
		}
		log.Printf("[storage] 写入共享失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskShareMount,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action": "mount", "share_id": row.ID,
			"vm_id": vm.ID, "vm_name": vm.Name,
			"host_path": hostPath, "root_path": root, "tag": tag,
			"security_model": securityModel, "read_only": readOnly,
		},
	})
	if err != nil {
		// 入队失败要把刚写的记录撤掉：留着会得到一条永远停在 pending 的
		// 共享，而重试时又会撞上「tag 已存在」——用户被卡在一个他无法
		// 理解的死循环里。
		if delErr := s.db.WithContext(ctx).Delete(&model.ShareMount{}, row.ID).Error; delErr != nil {
			log.Printf("[storage] 回滚共享记录失败 id=%d: %v", row.ID, delErr)
		}
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "share.mount",
		Params: map[string]any{
			"task_id": t.ID, "tag": tag, "rel_path": req.RelPath,
			"host_path": hostPath, "read_only": readOnly,
			"security_model": securityModel,
		},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// UnmountShare 卸载一个目录共享（F-5-06）。
//
// **不需要二次验证**：卸载不会造成不可逆的结果——目录还在宿主机上，随时
// 可以再挂回去。它影响的是来宾里那个挂载点，而失效是**立刻可见**的。
func (s *Service) UnmountShare(
	ctx context.Context, vmID int64, tag string, v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	vm, err := s.loadVMForShare(ctx, vmID, v)
	if err != nil {
		return nil, err
	}

	var row model.ShareMount
	if err := s.db.WithContext(ctx).
		Where("vm_id = ? AND tag = ?", vm.ID, tag).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("该 tag 对应的共享不存在")
		}
		log.Printf("[storage] 查询共享失败: %v", err)
		return nil, api.Internal()
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskShareMount,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"action": "unmount", "share_id": row.ID,
			"vm_id": vm.ID, "vm_name": vm.Name,
			"host_path": row.HostPath, "tag": row.Tag,
		},
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID,
		ResourceName: vm.Name, Action: "share.unmount",
		Params:  map[string]any{"task_id": t.ID, "tag": row.Tag},
		Success: true, ClientIP: clientIP,
	})
	return t, nil
}

// storageRoot 取用户在指定节点上的存储根。
func (s *Service) storageRoot(ctx context.Context, userID, nodeID int64) (string, error) {
	var us model.UserStorage
	err := s.db.WithContext(ctx).
		Where("user_id = ? AND node_id = ?", userID, nodeID).First(&us).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", api.ValidationFailed("你在此节点上还没有存储空间，无法共享目录")
	case err != nil:
		log.Printf("[storage] 查询用户存储失败: %v", err)
		return "", api.Internal()
	}
	if !us.Enabled || us.RootPath == nil || *us.RootPath == "" {
		// 存储根为空时**不能退化成「拼在 / 下」**：那会让相对路径校验
		// 失去意义，用户填 "etc" 就指向了 /etc。
		return "", api.ValidationFailed("你的存储空间尚未初始化，无法共享目录")
	}
	// 存储根是**被管宿主机**上的路径，一律按 POSIX 语义处理：用 filepath
	// 会掺进控制面本机平台的差异（Windows 上会把 `/srv/users/7` 变成
	// `\srv\users\7`），而后续的 path.Base、前缀检查都只认斜杠。
	return path.Clean(*us.RootPath), nil
}

// resolveUnderRoot 把用户给的相对路径解析成存储根下的绝对路径。
//
// 这个函数是整块功能的安全边界，因此校验是**层层设防**的：
//
//  1. 拒绝绝对路径。用户拿不到拼绝对路径的机会，`..` 穿越、`/etc` 这类
//     目标从一开始就不在可表达的范围内。
//  2. Clean 之后仍不得以 `..` 开头——兜住 `a/../../etc` 这类写法。
//  3. 拼出来之后再 Clean 一次并做**前缀检查**（带分隔符），防止第 2 步
//     之外的边角情况（例如路径里含 NUL 或平台差异）。
//
// 三次检查看起来冗余，但每一次挡掉的是**不同的一类**输入：删掉任何一步
// 都会留下一个可以直接读宿主机文件系统的缺口，而那正是这个功能最需要
// 防住的事。
//
// 还有一类控制面**拦不住**：共享目录内部指向外部的符号链接。那需要真实
// 路径解析，只有节点做得到——见 agent.OpShareMount 的说明。
func resolveUnderRoot(root, rel string) (string, error) {
	raw := strings.TrimSpace(rel)
	if raw == "" {
		return "", api.InvalidParameter("必须填写要共享的目录（相对于你的存储根）")
	}
	// 反斜杠与 NUL 一律拒绝：前者在部分平台被当作分隔符，后者会截断
	// 底层的系统调用参数，两者都能让前面的校验与实际落点不一致。
	if strings.ContainsRune(raw, '\x00') || strings.ContainsRune(raw, '\\') {
		return "", api.InvalidParameter("路径中不能包含反斜杠或空字符")
	}
	// 1) 相对路径。绝对路径直接拒绝，并说清该填什么。
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(raw) || hasDrivePrefix(raw) {
		return "", api.InvalidParameter(
			"请填写**相对于你存储根**的路径（例如 data/iso），不要填绝对路径；" +
				"存储根由系统分配，不接受直接指定")
	}

	clean := path.Clean(raw)
	// 2) 清理后仍在根之内。
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", api.InvalidParameter("路径不能指向存储根之外")
	}

	// 拼接同样按 POSIX 语义：结果要原样下发给宿主机，不能带控制面本机的分隔符。
	full := path.Join(root, clean)
	// 3) 前缀检查（带分隔符，避免 /data 与 /database 这类前缀混淆）。
	if full != root && !strings.HasPrefix(full, root+"/") {
		return "", api.InvalidParameter("路径超出了你的存储根")
	}
	return full, nil
}

// hasDrivePrefix 识别 `C:` 这类盘符前缀。
//
// 在 Linux 上它只是一个合法文件名的一部分，但共享路径可能被同步到别的
// 平台或被脚本消费——放行一个在另一种解释下具有「绝对」含义的写法，
// 等于给校验留一个取决于执行环境的缺口。
func hasDrivePrefix(p string) bool {
	return len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}

// relPathOf 从绝对路径反推相对于存储根的路径，供界面显示。
//
// 反推不出来时返回绝对路径本身：那说明记录是在存储根变更之前写的，
// 显示一个空串会让用户以为共享的根目录，而那与事实不符。
func relPathOf(root, hostPath string) string {
	if root == "" {
		return hostPath
	}
	rel, err := filepath.Rel(root, hostPath)
	if err != nil || strings.HasPrefix(rel, "..") {
		return hostPath
	}
	return filepath.ToSlash(rel)
}

// loadVMForShare 读取并校验目标虚拟机。
func (s *Service) loadVMForShare(ctx context.Context, vmID int64, v authz.Viewer) (*model.VM, error) {
	var vm model.VM
	if err := s.db.WithContext(ctx).Where("id = ?", vmID).First(&vm).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, api.NotFound("虚拟机不存在")
		}
		log.Printf("[storage] 查询虚拟机失败: %v", err)
		return nil, api.Internal()
	}
	// 404 而非 403：403 会确认「这个 ID 存在」，让租户能通过枚举推断出
	// 别人有多少虚拟机。
	if !v.IsAdmin && (vm.OwnerID == nil || *vm.OwnerID != v.UserID) {
		return nil, api.NotFound("虚拟机不存在")
	}
	return &vm, nil
}

// shareStatus 由 MountedAt 派生出对外的状态字符串。
//
// 只有两种：已生效与生效中。没有「失败」——见 model.ShareMount.MountedAt
// 的说明：挂载失败的记录不会留存。
func shareStatus(m *model.ShareMount) string {
	if m.IsActive() {
		return "active"
	}
	return "pending"
}

// toShareView 组装对外视图。root 可能为空（见 ListShares）。
func toShareView(m *model.ShareMount, root string) ShareView {
	view := ShareView{
		ID: m.ID, VMID: m.VMID, NodeID: m.NodeID,
		HostPath: m.HostPath, Tag: m.Tag,
		SecurityModel: m.SecurityModel, ReadOnly: m.ReadOnly,
		Status:    shareStatus(m),
		CreatedAt: m.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	view.RelPath = relPathOf(root, m.HostPath)
	if m.MountedAt != nil {
		view.MountedAt = m.MountedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	return view
}
