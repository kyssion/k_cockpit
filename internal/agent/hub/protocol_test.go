package hub

import "testing"

func TestProtocolVersionSupported(t *testing.T) {
	cases := []struct {
		name string
		v    int32
		want bool
	}{
		{"当前版本", 1, true},
		{"未声明版本", 0, false},
		{"未来版本", 2, false},
		{"非法值", -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := protocolVersionSupported(c.v); got != c.want {
				t.Fatalf("protocolVersionSupported(%d) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

func TestValidNodeName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"kvm-host-1", true},
		{"host.example.com", true},
		{"", false},
		{"-leading", false},
		{"has space", false},
		{"../../etc", false},
		{string(make([]byte, 65)), false},
	}
	for _, c := range cases {
		if got := validNodeName(c.name); got != c.want {
			t.Errorf("validNodeName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
