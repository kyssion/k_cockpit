package vpcacl

import (
	"context"
	"errors"
	"testing"

	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
)

// 缺 node_id 是**参数错误**，不是节点故障。
//
// 实机缺陷：`POST /vpc-acl/apply` 不带 node_id 时一路走到
// `agent.Execute(NodeID=0)` 再回落成 503「节点不支持 ACL 预览」，
// 调用方会去排查节点健康，而实际只是少了一个字段。
func TestApplyRejectsMissingNodeID(t *testing.T) {
	// 校验发生在任何依赖之前，因此零值服务即可：如果哪天把校验挪到
	// 取库/下发之后，这个测试会因为 nil panic 而失败——那正是我们想
	// 知道的。
	svc := &Service{}
	_, err := svc.Apply(context.Background(), ApplyRequest{NodeID: 0},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err == nil {
		t.Fatal("缺少 node_id 时应当拒绝")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("错误类型应为 *api.Error，实际 %T: %v", err, err)
	}
	if apiErr.Code != api.CodeInvalidParameter {
		t.Errorf("错误码 = %s，期望 %s（参数错误不能伪装成节点故障）",
			apiErr.Code, api.CodeInvalidParameter)
	}
}
