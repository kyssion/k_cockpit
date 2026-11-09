package template

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/task"
)

// PreprocessOptions 是要执行的预处理项。
//
// 用显式开关而不是一个"模式"下拉：这几项可以任意组合，而模式（standard / full）表达力不够——每次要做什么取决于那个镜像本身缺什么。
type PreprocessOptions struct {
	// InstallAgent 安装来宾代理（qemu-guest-agent）。
	InstallAgent bool
	// InjectSSHKey 注入 SSH 公钥。
	InjectSSHKey bool
	// SSHPublicKey 是选中 InjectSSHKey 时必填的公钥内容。
	SSHPublicKey string
	// ResetMachineID 重置 machine-id。
	//
	// 克隆出来的机器如果不重置，它们在网络里会被当成同一台（DHCP 拿到同一个地址、systemd 的 ID 冲突）——这是克隆场景里最常见的隐性故障。
	ResetMachineID bool
	// RemoveCloudInit 清理 cloud-init 残留，让下次开机重新走初始化。
	RemoveCloudInit bool
}

// Steps 把选项展开成下发给节点的步骤名。
func (o PreprocessOptions) Steps() []string {
	out := []string{}
	if o.InstallAgent {
		out = append(out, "install_agent")
	}
	if o.InjectSSHKey {
		out = append(out, "inject_ssh_key")
	}
	if o.ResetMachineID {
		out = append(out, "reset_machine_id")
	}
	if o.RemoveCloudInit {
		out = append(out, "remove_cloud_init")
	}
	return out
}

// Preprocess 受理一次离线预处理。
//
// 判定与执行都在节点侧：预处理要挂载镜像并改写里面的内容，控制面既没有工具链也不挂载镜像。这里只负责校验"至少选了一项"、把选项固化进任务参数、以及事后把 preprocessed_at 记上。
func (s *Service) Preprocess(
	ctx context.Context, id int64, opts PreprocessOptions, acknowledge bool,
	v authz.Viewer, operatorName, clientIP string,
) (*model.Task, error) {
	tpl, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if tpl.Status != model.TemplateReady {
		return nil, api.ValidationFailed("模板尚未就绪，不能预处理")
	}
	if len(opts.Steps()) == 0 {
		return nil, api.InvalidParameter("请至少选择一项预处理内容")
	}
	if opts.InjectSSHKey && strings.TrimSpace(opts.SSHPublicKey) == "" {
		return nil, api.InvalidParameter("已选择注入 SSH 公钥，但公钥内容为空")
	}
	if opts.InjectSSHKey {
		// 格式必须在**受理阶段**判死：预处理要等在机器上挂载并改写镜像，
		// 一个畸形的公钥会在几分钟后以 virt-customize 的底层报错收场
		// （`failed to parse public key`），用户既看不懂也不知道该改什么。
		if err := validateSSHPublicKey(opts.SSHPublicKey); err != nil {
			return nil, err
		}
	}
	if !acknowledge {
		return nil, api.Conflict("预处理会改写模板镜像内容（失败时由节点还原），确认请勾选")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskTemplatePreprocess,
		NodeID:       tpl.NodeID,
		ResourceType: "template",
		ResourceID:   tpl.ID,
		ResourceName: tpl.Name,
		OwnerID:      v.UserID,
		CreatedBy:    v.UserID,
		Params: preprocessParams{
			TemplateID:   tpl.ID,
			DiskPath:     tpl.DiskPathOf(),
			Steps:        opts.Steps(),
			SSHPublicKey: strings.TrimSpace(opts.SSHPublicKey),
		},
	})
	if err != nil {
		return nil, err
	}
	s.record(ctx, v.UserID, operatorName, clientIP, "template.preprocess", tpl.NodeID, tpl.Name, t.ID)
	return t, nil
}

type preprocessParams struct {
	TemplateID   int64    `json:"template_id"`
	DiskPath     string   `json:"disk_path"`
	Steps        []string `json:"steps"`
	SSHPublicKey string   `json:"ssh_public_key,omitempty"`
}

// sshKeyTypes 是 authorized_keys 认得的公钥类型前缀。
//
// 用白名单而不是"能解析就行"：这段内容会被写进节点上的临时文件、再由
// virt-customize 落到 /root/.ssh/authorized_keys。放行未知前缀只会把一个
// 看不懂的失败推迟到镜像改写阶段，而那时用户已经在等结果了。
var sshKeyTypes = []string{
	"ssh-rsa", "ssh-ed25519", "ssh-dss",
	"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	"sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com",
}

// validateSSHPublicKey 校验一行 OpenSSH 公钥（`<type> <base64> [comment]`）。
//
// 只校验形态，不解析密钥本身：控制面不需要用它做任何密码学操作，而形态
// 校验足以挡住「把私钥贴进来」「只贴了一半」「贴了 ssh-rsa 这个前缀」这三类
// 最常见的手误。
func validateSSHPublicKey(raw string) error {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) < 2 {
		return api.ValidationFailed(
			"SSH 公钥格式不正确：应是一整行 `ssh-ed25519 AAAAC3Nza… 备注`（不是文件路径，也不是 `ssh-rsa` 这样的前缀）")
	}
	known := false
	for _, want := range sshKeyTypes {
		if fields[0] == want {
			known = true
			break
		}
	}
	if !known {
		return api.ValidationFailed(
			"不支持的 SSH 公钥类型「" + fields[0] +
				"」：支持 ssh-rsa / ssh-ed25519 / ecdsa-sha2-*（请复制完整的公钥行）")
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return api.ValidationFailed("SSH 公钥的密钥体不是合法 base64，请重新复制整行公钥")
	}
	return nil
}

// PreprocessExecutor 执行模板预处理。
type PreprocessExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPreprocessExecutor 构造执行器。
func NewPreprocessExecutor(db *gorm.DB, client agent.Client) *PreprocessExecutor {
	return &PreprocessExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PreprocessExecutor) Type() string { return model.TaskTemplatePreprocess }

// Run 下发预处理，并在成功后记上 preprocessed_at。
//
// 不直接改模板的其它字段：镜像变了不代表规格变了，把预处理结果写成"模板变了"会让用户在对比备份时对不上。
func (e *PreprocessExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p preprocessParams
	if !decode(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpTemplatePreprocess,
		NodeID: *t.NodeID,
		Target: p.DiskPath,
		Params: map[string]any{
			"template_id": p.TemplateID, "steps": p.Steps,
			"ssh_public_key": p.SSHPublicKey,
		},
	})
	if err != nil {
		return api.Unavailable("节点不可达，预处理未执行")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}
	// 节点缺工具链时会返回 Unavailable：那与"做完了"必须分开——把它当成功会让用户以为镜像已经处理过，而实际上一项都没做。
	info := decodePreprocessInfo(result.Data)
	if info.Unavailable != "" {
		return api.Unavailable(info.Unavailable)
	}
	now := time.Now()
	if err := e.db.WithContext(ctx).Model(&model.Template{}).
		Where("id = ?", p.TemplateID).
		Updates(map[string]any{"preprocessed_at": now, "updated_at": now}).Error; err != nil {
		// 镜像已经改好了：标记失败只记日志。让一次时间戳写入失败把整个任务判为失败，用户会以为预处理没生效而再跑一次——那才是真的浪费时间。
		log.Printf("[template] 记录预处理时间失败 id=%d: %v", p.TemplateID, err)
	}
	return nil
}

// decodePreprocessInfo 从结果里取预处理结果。
//
// 走一遍 JSON：同进程拿到的是结构体、跨进程是 map，两种形状都要能读。
func decodePreprocessInfo(data map[string]any) agent.TemplatePreprocessInfo {
	if data == nil {
		return agent.TemplatePreprocessInfo{}
	}
	raw, ok := data[agent.TemplatePreprocessDataKey]
	if !ok {
		return agent.TemplatePreprocessInfo{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		return agent.TemplatePreprocessInfo{}
	}
	var info agent.TemplatePreprocessInfo
	if err := json.Unmarshal(blob, &info); err != nil {
		log.Printf("[template] 解析预处理结果失败: %v", err)
		return agent.TemplatePreprocessInfo{}
	}
	return info
}
