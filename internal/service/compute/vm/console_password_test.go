package vm

import (
	"errors"
	"strings"
	"testing"

	"k_cockpit/internal/platform/api"
)

// 本文件覆盖第 3 轮 DEF-R4-02：VNC 口令长度校验分散，两条分支给出不同的
// 状态码与文案（一边 400 中文、一边 422 英文原文）。

// TestValidateConsolePasswordUnified 覆盖四条边界。
func TestValidateConsolePasswordUnified(t *testing.T) {
	sp := func(s string) *string { return &s }

	cases := []struct {
		name     string
		protocol string
		pw       *string
		bad      bool
	}{
		{"未设置口令（nil）", ConsoleProtocolVNC, nil, false},
		{"恰好 8 字节", ConsoleProtocolVNC, sp("12345678"), false},
		{"9 字节", ConsoleProtocolVNC, sp("123456789"), true},
		// libvirt/RFB 按**字节**算：8 个中文 = 24 字节，必须拒。
		{"8 个中文字符（24 字节）", ConsoleProtocolVNC, sp("密码密码密码密码"), true},
		{"SPICE 不受 8 字节限制", ConsoleProtocolSPICE, sp("a-very-long-spice-password"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConsolePassword(tc.protocol, tc.pw)
			if tc.bad && err == nil {
				t.Fatal("超长口令必须被拒绝（否则会下发给 libvirt，得到英文原文的 422）")
			}
			if !tc.bad && err != nil {
				t.Fatalf("不该拒绝: %v", err)
			}
			if !tc.bad {
				return
			}
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("应为业务错误，实际 %v", err)
			}
			// 状态码与文案必须与另一条分支一致：中文、说明协议原因、点出按字节算。
			if apiErr.Status != 400 {
				t.Errorf("状态码 = %d，期望 400（与既有的 invalid-parameter 口径一致）", apiErr.Status)
			}
			for _, want := range []string{"最长 8 位", "VNC", "字节"} {
				if !strings.Contains(apiErr.Message, want) {
					t.Errorf("文案应含 %q，实际: %s", want, apiErr.Message)
				}
			}
		})
	}
}
