package nodeagent

import (
	"testing"

	"k_cockpit/internal/agent"
)

// nvramPathOf：UEFI 域带 <nvram template=...>path</nvram>，BIOS 域无该元素。
func TestNvramPathOf(t *testing.T) {
	cases := []struct {
		name, xml, want string
	}{
		{"uefi 域", "<os><nvram template='/usr/share/AAVMF/vars.fd'>/var/lib/libvirt/qemu/nvram/kc-x_VARS.fd</nvram></os>",
			"/var/lib/libvirt/qemu/nvram/kc-x_VARS.fd"},
		{"bios 域", "<os><type>hvm</type></os>", ""},
		{"自闭合", "<os><nvram/></os>", ""},
	}
	for _, c := range cases {
		if got := nvramPathOf(c.xml); got != c.want {
			t.Errorf("%s: nvramPathOf = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMigrateStagePath(t *testing.T) {
	if got := migrateStagePath("kc-a", "kc-a.qcow2"); got != "/var/lib/k_cockpit/migrate/kc-a/kc-a.qcow2" {
		t.Fatalf("migrateStagePath = %q", got)
	}
}

// live 模式的诚实阻断：不碰任何系统状态，直接失败并可读。
func TestVMMigrateLiveBlocked(t *testing.T) {
	res, err := vmMigrate(nil, opOf("kc-x", map[string]any{"mode": "live"}), func(Stage) {})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Success {
		t.Fatal("live 模式不应成功")
	}
	if res.Message == "" {
		t.Fatal("阻断必须带可读说明")
	}
}

func opOf(target string, params map[string]any) agent.Operation {
	return agent.Operation{Target: target, Params: params}
}
