package nodeagent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 这份测试盯的是「身份材料落盘」这一段：目录与文件的权限位、写入顺序、
// 以及失败时不留下半截身份。U-35 实机复核发现 /var/lib/k_cockpit-agent
// 实际是 0755（老安装脚本建的），而代码里写的是 0700——MkdirAll 对已存在
// 的目录不改权限，于是声明的权限在升级路径上从来没成立过。

func stateFixture(t *testing.T) StateDir {
	t.Helper()
	return StateDir{Dir: filepath.Join(t.TempDir(), "state")}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestSaveEnrollmentWritesFullSet 覆盖首次落盘：四件套齐全且权限正确。
func TestSaveEnrollmentWritesFullSet(t *testing.T) {
	sd := stateFixture(t)
	st := &State{AgentID: "11111111-2222-3333-4444-555555555555", Server: "cp.example:8443"}
	if err := sd.SaveEnrollment(st, []byte("CA"), []byte("CERT"), []byte("KEY")); err != nil {
		t.Fatalf("SaveEnrollment: %v", err)
	}

	for _, f := range []string{"agent.json", "ca.pem", "client.pem", "client.key"} {
		if _, err := os.Stat(filepath.Join(sd.Dir, f)); err != nil {
			t.Errorf("%s 未写入: %v", f, err)
		}
	}

	if got := modeOf(t, sd.Dir); got != 0o700 {
		t.Errorf("状态目录权限 = %o，期望 700", got)
	}
	// 私钥必须只有属主可读：这是整个目录存在的理由。
	if got := modeOf(t, sd.keyFile()); got != 0o600 {
		t.Errorf("client.key 权限 = %o，期望 600", got)
	}
	// agent.json 含服务器地址与身份，同样收紧（非机密但无必要放开）。
	if got := modeOf(t, sd.agentFile()); got != 0o600 {
		t.Errorf("agent.json 权限 = %o，期望 600", got)
	}
	// 证书与 CA 是公开材料，保持可读，便于运维排查。
	for _, p := range []string{sd.certFile(), sd.caFile()} {
		if got := modeOf(t, p); got != 0o644 {
			t.Errorf("%s 权限 = %o，期望 644", filepath.Base(p), got)
		}
	}

	got, err := sd.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AgentID != st.AgentID || got.Server != st.Server {
		t.Errorf("Load 回来的是 %+v，期望 %+v", got, st)
	}
	if !sd.Enrolled() {
		t.Error("Enrolled 应为 true")
	}
}

// TestSaveEnrollmentTightensExistingDir 是本轮实机发现的回归点：
// 目录已经以 0755 存在时（老安装脚本 / 手工 install -d），再 enroll 必须
// 把权限收回到 0700，而不是沿用历史。
func TestSaveEnrollmentTightensExistingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不区分 POSIX 权限位")
	}
	sd := stateFixture(t)
	if err := os.MkdirAll(sd.Dir, 0o755); err != nil {
		t.Fatalf("预建目录: %v", err)
	}
	// MkdirAll 对已存在目录不生效，先确认前提成立（否则这条用例什么也没测）。
	if err := os.Chmod(sd.Dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := sd.SaveEnrollment(&State{AgentID: "a", Server: "s:8443"},
		[]byte("CA"), []byte("CERT"), []byte("KEY")); err != nil {
		t.Fatalf("SaveEnrollment: %v", err)
	}
	if got := modeOf(t, sd.Dir); got != 0o700 {
		t.Errorf("已存在的 0755 目录未被收紧，权限仍是 %o", got)
	}
}

// TestSaveEnrollmentKeepsIdentityWhenWriteFails 覆盖「先证书后 agent.json」
// 的顺序承诺：中途失败时 agent.json **不得出现**。
//
// 反过来的顺序会造出一个「看起来已 enroll、启动却因为没证书而崩」的节点，
// 而那台机器上的运维只会看到一句含糊的启动失败。
func TestSaveEnrollmentKeepsIdentityWhenWriteFails(t *testing.T) {
	sd := stateFixture(t)
	if err := os.MkdirAll(sd.Dir, 0o700); err != nil {
		t.Fatalf("预建目录: %v", err)
	}
	// 把 client.pem 先占成目录，让写入证书这一步必然失败。
	if err := os.Mkdir(sd.certFile(), 0o755); err != nil {
		t.Fatalf("占位: %v", err)
	}

	err := sd.SaveEnrollment(&State{AgentID: "a", Server: "s:8443"},
		[]byte("CA"), []byte("CERT"), []byte("KEY"))
	if err == nil {
		t.Fatal("证书写入失败时 SaveEnrollment 应返回错误")
	}
	if _, statErr := os.Stat(sd.agentFile()); !os.IsNotExist(statErr) {
		t.Errorf("写证书失败后 agent.json 不应存在（stat err = %v）", statErr)
	}
	if sd.Enrolled() {
		t.Error("半截状态不应被判为已 enroll")
	}
}

// TestLoadRejectsBrokenState 覆盖损坏的 agent.json：缺字段与非法 JSON
// 都要报出来，而不是返回一个字段为空的 State 让上层去连一个空地址。
func TestLoadRejectsBrokenState(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺 agent_id", `{"server":"s:8443"}`, "缺少 agent_id 或 server"},
		{"缺 server", `{"agent_id":"a"}`, "缺少 agent_id 或 server"},
		{"非法 JSON", `{`, "agent.json 损坏"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sd := stateFixture(t)
			if err := os.MkdirAll(sd.Dir, 0o700); err != nil {
				t.Fatalf("预建目录: %v", err)
			}
			if err := os.WriteFile(sd.agentFile(), []byte(c.body), 0o600); err != nil {
				t.Fatalf("写 agent.json: %v", err)
			}
			_, err := sd.Load()
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误 = %q，期望包含 %q", err, c.want)
			}
		})
	}
}

// TestLoadMissingDirIsNotExist 覆盖未 enroll：错误必须能按 os.ErrNotExist
// 判定，cmd/agent 才能给出「先运行 enroll」而不是「身份损坏」。
func TestLoadMissingDirIsNotExist(t *testing.T) {
	sd := stateFixture(t)
	_, err := sd.Load()
	if !os.IsNotExist(err) {
		t.Fatalf("未 enroll 时错误 = %v，期望 os.ErrNotExist", err)
	}
	if sd.Enrolled() {
		t.Error("未 enroll 时 Enrolled 应为 false")
	}
}

// TestClientTLSRejectsBrokenCA 覆盖 CA 缺失与 CA 无法解析两种情形。
//
// 这两种都必须在**启动时**报错：静默降级成 InsecureSkipVerify 等于把
// mTLS 的服务端身份校验丢掉。
func TestClientTLSRejectsBrokenCA(t *testing.T) {
	t.Run("CA 文件缺失", func(t *testing.T) {
		sd := stateFixture(t)
		if _, err := sd.ClientTLS(); err == nil {
			t.Fatal("缺 CA 时应报错")
		}
	})
	t.Run("CA 无法解析", func(t *testing.T) {
		sd := stateFixture(t)
		if err := os.MkdirAll(sd.Dir, 0o700); err != nil {
			t.Fatalf("预建目录: %v", err)
		}
		if err := os.WriteFile(sd.caFile(), []byte("not a pem"), 0o644); err != nil {
			t.Fatalf("写 CA: %v", err)
		}
		_, err := sd.ClientTLS()
		if err == nil || !strings.Contains(err.Error(), "CA 证书无法解析") {
			t.Fatalf("错误 = %v，期望包含「CA 证书无法解析」", err)
		}
	})
}
