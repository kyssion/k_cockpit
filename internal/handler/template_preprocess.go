package handler

import (
	"context"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/template"
)

type preprocessRequest struct {
	InstallAgent    bool `json:"install_agent"`
	InjectSSHKey    bool `json:"inject_ssh_key"`
	ResetMachineID  bool `json:"reset_machine_id"`
	RemoveCloudInit bool `json:"remove_cloud_init"`
	Acknowledge     bool `json:"acknowledge"`
}

// Preprocess 对模板做离线预处理。
//
// 判定与执行都在节点侧——预处理要挂载镜像并改写里面的内容，控制面既没有工具链也不挂载镜像。这里只负责把选项固化进任务参数。
func (h *Template) Preprocess(ctx context.Context, c *app.RequestContext) {
	id, err := namedPathID(c, "id", "模板 ID")
	if err != nil {
		api.Fail(c, err)
		return
	}
	var req preprocessRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}
	user := auth.CurrentUser(c)
	info := auth.ClientInfoOf(c)

	t, err := h.svc.Preprocess(ctx, id, template.PreprocessOptions{
		InstallAgent:    req.InstallAgent,
		InjectSSHKey:    req.InjectSSHKey,
		ResetMachineID:  req.ResetMachineID,
		RemoveCloudInit: req.RemoveCloudInit,
	}, req.Acknowledge, authz.ViewerOf(c), user.Username, info.IP)
	if err != nil {
		api.Fail(c, err)
		return
	}
	api.OK(c, map[string]any{"task_id": t.ID, "status": t.Status})
}
