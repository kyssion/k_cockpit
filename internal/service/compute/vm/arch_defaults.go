package vm

import (
	"context"
	"encoding/json"
	"log"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// 架构默认值集中在这里，避免「表单元数据」与「入队校验」各写一份。
//
// 动因（实机测试抓出）：默认值原先是硬编码的 x86_64 + q35 + bios，与节点
// 架构无关。在 aarch64 宿主上，用户什么都不改直接提交，会被组合校验拒成
// 400（virt 机型 / UEFI 固件），每次创建都要手动改三项。后台的 agent 早就
// 按宿主架构兜底了（xmlbuilder 的 libvirtArch(runtime.GOARCH)），缺的是
// 控制面把「这台节点的架构」告诉用户与校验。

// archDefaults 返回某架构下自洽的一组默认值。
//
// 只覆盖**互相约束**的四项：arch 决定 machine_type，machine_type 决定
// firmware，显示设备随 ARM 走 ramfb（固件帧缓冲）。其余默认值仍来自
// 编辑矩阵，与架构无关。
func archDefaults(arch string) map[string]string {
	if arch == "aarch64" {
		return map[string]string{
			"arch":         "aarch64",
			"machine_type": "virt",
			"firmware":     "uefi",
			"video_model":  "ramfb",
		}
	}
	return map[string]string{
		"arch":         "x86_64",
		"machine_type": "q35",
		"firmware":     "bios",
		"video_model":  "virtio",
	}
}

// archOfCapabilities 从节点自报的能力清单里取宿主架构；没有则返回空。
func archOfCapabilities(caps []string) string {
	for _, c := range caps {
		if len(c) > len(agent.CapabilityArchPrefix) && c[:len(agent.CapabilityArchPrefix)] == agent.CapabilityArchPrefix {
			return c[len(agent.CapabilityArchPrefix):]
		}
	}
	return ""
}

// nodeArch 读节点上报的架构。
//
// 失败只记日志并返回空：这是一项「锦上添花」的默认值，读不到时按矩阵的
// 通用默认走（并在组合校验里被拦成明确错误），不该让整个表单打不开。
func (s *Service) nodeArch(ctx context.Context, nodeID int64) string {
	var n model.Node
	if err := s.db.WithContext(ctx).Select("id", "capabilities").
		Where("id = ?", nodeID).First(&n).Error; err != nil {
		log.Printf("[vm] 读取节点架构失败 node=%d: %v", nodeID, err)
		return ""
	}
	if n.Capabilities == nil || *n.Capabilities == "" {
		return ""
	}
	var caps []string
	if err := json.Unmarshal([]byte(*n.Capabilities), &caps); err != nil {
		// 老 agent 或能力清单损坏：按未上报处理，不影响创建。
		return ""
	}
	return archOfCapabilities(caps)
}

// applyNodeArchDefaults 用节点架构补齐创建请求里空缺的架构相关字段。
//
// 只填空值，不覆盖用户显式选择：用户把 x86_64 改成 aarch64 是合法诉求
// （跑 ARM 镜像），由组合校验去判断这个组合是否成立。
func (s *Service) applyNodeArchDefaults(ctx context.Context, req CreateRequest) CreateRequest {
	arch := req.Arch
	if arch == "" {
		arch = s.nodeArch(ctx, req.NodeID)
		if arch == "" {
			// 节点尚未上报（老 agent / 刚接入）：保留矩阵默认，让组合
			// 校验给出可读的拒绝理由，而不是把一个空架构下发给 agent。
			return req
		}
		req.Arch = arch
	}
	d := archDefaults(arch)
	if req.MachineType == "" {
		req.MachineType = d["machine_type"]
	}
	if req.Firmware == "" {
		req.Firmware = d["firmware"]
	}
	return req
}
