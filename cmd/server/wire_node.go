// wire_node 装配 internal/node 域与 agent 通道（internal/agent）。
//
// agent 通道实例挂在 app 根上（a.mockAgent）：全部业务域都要向节点下发
// 操作，它和 db 一样是跨域基座；这里只装配节点服务本身。
package main

import (
	"context"
	"log"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/devdata"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
	"k_cockpit/internal/platform/config"
)

// nodeServices 承载 internal/node 域的服务实例。
type nodeServices struct {
	nodeSvc *node.Service
}

// setupAgent 装配节点通道与节点服务。
func (a *app) setupAgent() { // 装配 agent 通道。gRPC 双向流实现尚未开发，本期由 mock 直接
	// 返回结果——业务代码只依赖内部接口，替换 agent 实现时无需改动（ADR-0007）。
	switch a.cfg.Agent.Transport {
	case config.AgentTransportMock:
		// 阶段之间留一点间隔，好让任务时间线能看出推进过程。
		//
		// 测试里用的是零间隔（NewMockClient）：**测试需要确定性，不需要
		// 真实感**——每个用例多等一秒只会让人不愿跑测试。而演示时所有阶段
		// 落在同一毫秒里，时间线虽然是对的，却看不出它是一条时间线。
		a.mockAgent = agent.NewMockClient().WithStageDelay(220 * time.Millisecond)
		log.Printf("[agent] 通道 = mock：节点运行态与领域操作由 mock 提供，不与节点通信")
	default:
		log.Fatalf("AGENT_TRANSPORT=%s 尚未实现（当前仅支持 %s）",
			a.cfg.Agent.Transport, config.AgentTransportMock)
	}
	a.node.nodeSvc = node.NewService(a.db, a.mockAgent, a.recorder, a.mockAgent)
}

// seedDevData 预置开发环境的演示数据（mock 运输层专用，见 internal/devdata）：
// 模拟节点、存储池、系统网络、演示镜像与模板。双重闸门（development +
// mock）保证生产永不进入；失败只降级告警不阻断启动（Ensure 自身幂等）。
func (a *app) seedDevData() {
	if a.cfg.Env != config.EnvDevelopment || a.cfg.Agent.Transport != config.AgentTransportMock {
		return
	}
	err := devdata.Ensure(context.Background(), devdata.Deps{
		DB:      a.db,
		Nodes:   a.node.nodeSvc,
		SwitchN: a.network.networkSvc,
	})
	if err != nil {
		log.Printf("[devdata] ⚠️ 预置演示数据失败（不影响启动，可手工补齐）: %v", err)
	}
}

// hydrateMockPowerState 把虚拟机投影的当前状态喂给 mock（ADR-0011）：
// mock 重启后电源状态清零，不注水的话「已关机的虚拟机点开机」会被
// 默认值 running 拒绝。仅 mock 运输层需要；未知状态跳过（探测的默认值
// 会兜底，操作一次后自然收敛）。
func (a *app) hydrateMockPowerState() {
	if a.cfg.Agent.Transport != config.AgentTransportMock {
		return
	}
	var vms []model.VM
	if err := a.db.WithContext(context.Background()).
		Where("present = ? AND status <> ?", true, model.VMStatusUnknown).
		Find(&vms).Error; err != nil {
		log.Printf("[agent] ⚠️ 读取虚拟机投影失败，mock 电源状态未注水: %v", err)
		return
	}
	for _, v := range vms {
		a.mockAgent.SetPower(v.Name, string(v.Status))
	}
}
