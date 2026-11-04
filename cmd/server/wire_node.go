// wire_node 装配 internal/service/node 域与 agent 通道（internal/agent）。
//
// agent 通道实例挂在 app 根上（a.agentClient，类型 agent.Client）：全部
// 业务域都要向节点下发操作，它和 db 一样是跨域基座；这里只装配节点服务
// 本身。通道只有 gRPC 一种实现（2026-11-04 起移除 mock）。
package main

import (
	"context"
	"log"
	"net"
	"path/filepath"

	"k_cockpit/internal/agent"
	agenthub "k_cockpit/internal/agent/hub"
	noderepo "k_cockpit/internal/repository/node"
	"k_cockpit/internal/service/node"
)

// nodeServices 承载 internal/service/node 域的服务实例。
type nodeServices struct {
	nodeSvc *node.Service
}

// setupAgent 装配节点通道与节点服务。
//
// hub 的注册依赖用了**延迟解析**：Registrar 在运行期（agent 连接进来时）
// 才调 node 服务，而 node 服务此刻还没构造——闭包捕获 app，调用时
// a.node.nodeSvc 早已就绪。构造顺序因此不需要为它让路。
func (a *app) setupAgent() {
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
	log.Printf("[agent] 网关监听 %s（mTLS），PKI 目录 %s", a.cfg.Agent.ListenAddr, pkiDir)

	// SnapshotProvider 与 Client 是同一实现；接口拆分（snapshot.go 的注释）
	// 正是为了「只读运行态的调用方不拿下发能力」。装配处断言一次，失败即
	// 装配 bug。
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
