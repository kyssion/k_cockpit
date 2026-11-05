package sys

import "testing"

func TestSpiceListed(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{"列出 spice", "<graphics supported='yes'><value>spice</value></graphics>", true},
		{"只有 vnc", "<graphics supported='yes'><value>vnc</value></graphics>", false},
		{"空输出", "", false},
	}
	for _, c := range cases {
		if got := spiceListed(c.out); got != c.want {
			t.Errorf("%s: spiceListed = %v, want %v", c.name, got, c.want)
		}
	}
}
