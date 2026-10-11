// wire_node 装配 internal/service/node 域与 agent 通道（internal/agent）。
//
// agent 通道实例挂在 app 根上（a.agentClient，类型 agent.Client）：全部
// 业务域都要向节点下发操作，它和 db 一样是跨域基座；这里只装配节点服务
// 本身，并按 AGENT_TRANSPORT 选择通道实现（mock / grpc，见 ADR-0007 与
// f-6-01 §5.2）。
package main

import (
	"context"
	"log"
	"net"
	"path/filepath"
	"time"

	"k_cockpit/internal/agent"
	agenthub "k_cockpit/internal/agent/hub"
	"k_cockpit/internal/devdata"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/config"
	noderepo "k_cockpit/internal/repository/node"
	"k_cockpit/internal/service/node"
)

// nodeServices 承载 internal/service/node 域的服务实例。
type nodeServices struct {
	nodeSvc *node.Service
}

// setupAgent 装配节点通道与节点服务。
//
// grpc 分支的注册依赖用了**延迟解析**：hub 的 Registrar 在运行期（agent
// 连接进来时）才调 node 服务，而 node 服务此刻还没构造——闭包捕获 app，
// 调用时 a.node.nodeSvc 早已就绪。构造顺序因此不需要为它让路。
func (a *app) setupAgent() {
	switch a.cfg.Agent.Transport {
	case config.AgentTransportMock:
		// 阶段之间留一点间隔，好让任务时间线能看出推进过程。
		//
		// 测试里用的是零间隔（NewMockClient）：**测试需要确定性，不需要
		// 真实感**——每个用例多等一秒只会让人不愿跑测试。而演示时所有阶段
		// 落在同一毫秒里，时间线虽然是对的，却看不出它是一条时间线。
		a.agentClient = agent.NewMockClient().WithStageDelay(220 * time.Millisecond)
		log.Printf("[agent] 通道 = mock：节点运行态与领域操作由 mock 提供，不与节点通信")

	case config.AgentTransportGRPC:
		pkiDir := a.cfg.Agent.TLSDir
		if pkiDir == "" {
			// 控制面持久化的东西都在数据库同级的 data 目录下，PKI 也不例外。
			pkiDir = filepath.Join(filepath.Dir(a.cfg.DB.Path), "agent-pki")
		}
		h, err := agenthub.New(agenthub.Deps{
			Repo:      noderepo.NewRepo(a.db),
			Registrar: lazyRegistrar{a: a},
			TLSDir:    pkiDir,
		})
		if err != nil {
			log.Fatalf("[agent] 初始化网关 PKI 失败: %v", err)
		}
		ln, err := net.Listen("tcp", a.cfg.Agent.ListenAddr)
		if err != nil {
			log.Fatalf("[agent] agent 网关监听 %s 失败: %v", a.cfg.Agent.ListenAddr, err)
		}
		go func() {
			// Serve 阻塞到监听关闭；失败（端口被抢等）直接终止进程——
			// agent 通道起不来的控制面对这套部署而言没有意义。
			if serveErr := h.Serve(ln); serveErr != nil {
				log.Fatalf("[agent] agent 网关退出: %v", serveErr)
			}
		}()
		a.agentGatewayStop = h.Stop
		a.agentClient = h
		log.Printf("[agent] 通道 = grpc：网关监听 %s（mTLS），PKI 目录 %s",
			a.cfg.Agent.ListenAddr, pkiDir)

	default:
		log.Fatalf("AGENT_TRANSPORT=%s 尚未实现（当前支持 %s / %s）",
			a.cfg.Agent.Transport, config.AgentTransportMock, config.AgentTransportGRPC)
	}
	// SnapshotProvider 与 Client 在两个实现上都并存；接口拆分（snapshot.go
	// 的注释）正是为了「只读运行态的调用方不拿下发能力」。装配处断言一次，
	// 失败即装配 bug（两个内置实现都满足）。
	runtime, ok := a.agentClient.(agent.SnapshotProvider)
	if !ok {
		log.Fatalf("[agent] 通道实现未提供 SnapshotProvider（装配错误）")
	}
	a.node.nodeSvc = node.NewService(noderepo.NewRepo(a.db), runtime, a.recorder, a.agentClient)
}

// lazyRegistrar 把 hub 的注册调用延迟解析到 node 服务。
//
// 注册的业务语义（令牌校验、状态翻转、审计）属于 node 域，hub 只是传输；
// 闭包在 agent 真正连进来时才解引用 nodeSvc，绕开「hub 先于 node 服务
// 构造」的顺序约束。
type lazyRegistrar struct{ a *app }

func (l lazyRegistrar) Register(
	ctx context.Context, token, agentID, agentVersion string,
) (nodeID int64, nodeName, confirmedAgentID string, err error) {
	v, err := l.a.node.nodeSvc.Register(ctx, token, agentID, agentVersion)
	if err != nil {
		return 0, "", "", err
	}
	return v.ID, v.Name, agentID, nil
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
	mc, ok := a.agentClient.(*agent.MockClient)
	if !ok {
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
		mc.SetPower(v.Name, string(v.Status))
	}
}
