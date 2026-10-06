// wire_compute 装配 internal/service/compute 域：虚拟机、计算配额、标签、模板、
// 镜像导入与 PCIe 直通。
//
// 依赖 storage 域（模板与镜像导入的存储配额判定）。createExec / guestExec
// 在 setupTaskQueue 阶段就构造并注册（队列先于服务装配），它们的加密密钥
// 由 setupCredentials 在受理任何任务之前补注入。
package main

import (
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/service/compute/computequota"
	"k_cockpit/internal/service/compute/importer"
	"k_cockpit/internal/service/compute/passthrough"
	"k_cockpit/internal/service/compute/template"
	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/compute/vmtag"
)

// computeServices 承载 internal/service/compute 域的服务实例。
type computeServices struct {
	vmSvc *vm.Service
	// createExec / guestExec 单独留字段：它们要在 setupCredentials 里补注入
	// 加密密钥，而那发生在队列注册之后。
	createExec *vm.CreateExecutor
	guestExec  *vm.GuestExecutor

	computeQuota *computequota.Service
	tag          *vmtag.Service
	template     *template.Service
	importer     *importer.Service
	passthrough  *passthrough.Service
}

// setupComputeServices 装配计算域全部服务。
func (a *app) setupComputeServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	a.compute.computeQuota = computequota.NewService(db, a.recorder)
	a.compute.tag = vmtag.NewService(db, a.recorder)
	// vm 与 storage 都要读设置里的陈旧阈值：把 settings 作为 Provider
	// 注入，让「面板上改的阈值」真的影响业务行为——否则那两个设置项就是
	// 摆设，而「改了不生效」正是 f-9-01 R-002 要消灭的现象。
	a.compute.vmSvc = vm.NewService(db, queue, a.recorder, mockAgent,
		a.platform.settings, a.storage.quota)
	a.compute.vmSvc.SetComputeQuota(a.compute.computeQuota)
	// 列表要带标签与最近占用，两者都是**一次批量查询**；不装配则列表不带这两列。
	a.compute.vmSvc.SetTagProvider(a.compute.tag)
	a.compute.template = template.NewService(db, queue, a.recorder, mockAgent, a.storage.quota)
	a.compute.importer = importer.NewService(db, queue, mockAgent, a.recorder, a.storage.quota)
	a.compute.passthrough = passthrough.NewService(db, queue, mockAgent, a.recorder)
}

// setupCredentials 给需要可逆凭据的服务注入加密密钥。
//
// 必须在受理任何任务之前完成：创建与来宾改密都要用它加密初始凭据。
func (a *app) setupCredentials() {
	// 控制台密码需要可逆加密（f-2-08 R-005）：它要交给 agent 参与 VNC 认证，
	// 因此不能用单向哈希。用途标签与会话签名分开派生。
	// 控制台密码与初始登录密码共用同一把派生密钥：它们都是"交给节点或展示
	// 给用户的可逆凭据"，分开派生只会让"忘了配哪一个"的排查面翻倍。
	credKey := cryptoutil.DeriveKey(
		[]byte(a.cfg.Session.Secret), "k_cockpit/vm/credential/v1")
	a.compute.vmSvc.SetEncryptionKey(credKey)
	a.compute.createExec.SetEncryptionKey(credKey)
	a.compute.guestExec.SetEncryptionKey(credKey)
}
