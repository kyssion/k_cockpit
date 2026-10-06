// wire_network 装配 internal/network 域：虚拟交换机、网桥、双层防火墙、
// 公网 IP、安全组、端口安全、端口镜像与抓包。
package main

import (
	"k_cockpit/internal/network/bridge"
	"k_cockpit/internal/network/capture"
	"k_cockpit/internal/network/firewall"
	"k_cockpit/internal/network/hostfirewall"
	"k_cockpit/internal/network/portmirror"
	"k_cockpit/internal/network/portsecurity"
	"k_cockpit/internal/network/publicip"
	"k_cockpit/internal/network/securitygroup"
	"k_cockpit/internal/network/vpcacl"
	"k_cockpit/internal/network/vswitch"
)

// networkServices 承载 internal/network 域的服务实例。
type networkServices struct {
	// networkSvc 是虚拟交换机（VPC）服务；netSvc 是宿主机网桥服务。
	networkSvc *vswitch.Service
	netSvc     *bridge.Service

	firewall     *firewall.Service
	hostFirewall *hostfirewall.Service
	publicIP     *publicip.Service
	sg           *securitygroup.Service
	portSecurity *portsecurity.Service
	mirror       *portmirror.Service
	capture      *capture.Service
	vpcACL       *vpcacl.Service
}

// setupNetworkServices 装配网络域全部服务。
//
// 依赖：platform 的设置（交换机的带宽总限从设置读取）；不依赖 compute——
// publicIP 与计算配额的接线在 setupCrossWiring（那时 compute 已就绪）。
func (a *app) setupNetworkServices() {
	db, queue, mockAgent := a.db, a.queue, a.mockAgent

	a.network.publicIP = publicip.NewService(db, queue, mockAgent, a.recorder)
	a.network.sg = securitygroup.NewService(db, queue, mockAgent, a.recorder)
	a.network.firewall = firewall.NewService(db, mockAgent, a.recorder)
	a.network.mirror = portmirror.NewService(db, mockAgent, a.recorder)
	a.network.netSvc = bridge.NewService(db, mockAgent, a.recorder)
	a.network.portSecurity = portsecurity.NewService(db, mockAgent, a.recorder, queue)
	a.network.capture = capture.NewService(db, mockAgent, a.recorder, queue)
	// 面板与 SSH 端口作为**合成的保护规则**传给服务——它们跟着配置走，
	// 不存进表：端口改了而表里那条还在，它会保护一个不再监听的端口。
	a.network.hostFirewall = hostfirewall.NewService(
		db, queue, mockAgent, a.recorder, a.cfg.HTTP.Port, nil)
	a.network.vpcACL = vpcacl.NewService(db, mockAgent, queue, a.recorder)

	a.network.networkSvc = vswitch.NewService(db, mockAgent, queue, a.recorder)
	// 全局带宽总限（G-44）从设置读取：下发时现取值，改设置不需要重启。
	a.network.networkSvc.SetSettingsProvider(a.platform.settings)
}
