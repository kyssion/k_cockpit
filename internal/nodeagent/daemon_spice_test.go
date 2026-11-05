package nodeagent

import (
	"slices"
	"testing"

	"k_cockpit/internal/agent"
)

func TestHeartbeatReportsSPICECapability(t *testing.T) {
	d := &Daemon{Registry: Registry{}, SPICESupported: true}
	if !slices.Contains(d.heartbeatMsg().GetHeartbeat().Capabilities, agent.CapabilitySPICE) {
		t.Fatal("节点支持 SPICE 时心跳应带出能力标记")
	}

	d.SPICESupported = false
	if slices.Contains(d.heartbeatMsg().GetHeartbeat().Capabilities, agent.CapabilitySPICE) {
		t.Fatal("节点不支持 SPICE 时心跳不应带出能力标记")
	}
}
