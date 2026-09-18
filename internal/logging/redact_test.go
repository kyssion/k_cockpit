package logging

import (
	"strings"
	"testing"
)

// TestRedactKeyValues 覆盖最常见的形态。
//
// 保留键名是本包的一个刻意选择：排查时要能看出「这里原本有一个密码字段」。
// 把整段抹掉的话，日志会变成「= [已脱敏]」，而人无法判断那是什么。
func TestRedactKeyValues(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"password=hunter2", "password=[已脱敏]"},
		{"password: hunter2", "password: [已脱敏]"},
		{`"password":"hunter2"`, `"password":"[已脱敏]"`},
		{"PASSWORD=Hunter2", "PASSWORD=[已脱敏]"},
		{"api_key=abc123", "api_key=[已脱敏]"},
		{"apiKey=abc123", "apiKey=[已脱敏]"},
		{"token=xyz", "token=[已脱敏]"},
		{"secret: s3cr3t", "secret: [已脱敏]"},
		{"client_secret=a1b2c3", "client_secret=[已脱敏]"},
		{"cookie=session%3Dabc", "cookie=[已脱敏]"},
		{"private_key=-----BEGIN", "private_key=[已脱敏]"},
		{"session_id=s-12345", "session_id=[已脱敏]"},
	}
	for _, c := range cases {
		got := Redact(c.in)
		if got != c.want {
			t.Errorf("Redact(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// TestRedactKeepsKeyName 单独覆盖「键名必须留下」。
//
// 这是最容易实现错的一处：把匹配到的整段（含键名）替换掉，脱敏是成功了，
// 但日志从此失去排查价值——人看到一行「[已脱敏]」不知道那是什么字段。
func TestRedactKeepsKeyName(t *testing.T) {
	got := Redact("连接数据库失败 password=abc123 host=db1")
	if !strings.Contains(got, "password") {
		t.Errorf("键名必须保留，实际 %q", got)
	}
	if strings.Contains(got, "abc123") {
		t.Errorf("值必须被脱敏，实际 %q", got)
	}
	// 同一行里的**非敏感**内容不能受影响。
	if !strings.Contains(got, "host=db1") {
		t.Errorf("同行的普通字段被误伤: %q", got)
	}
}

// TestRedactBareCredentials 覆盖没有键名的裸凭据。
//
// 这一组比键值对更重要：`log.Printf("请求头: %v", headers)` 这种写法会把
// 令牌直接拼进消息正文，而那里**没有键名可依**。
func TestRedactBareCredentials(t *testing.T) {
	cases := []string{
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc123",
		"请求头 authorization: bearer abcdefghijklmnop",
		"token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature123",
		"连接 postgres://user:s3cret@localhost:5432/db 失败",
	}
	for _, c := range cases {
		got := Redact(c)
		for _, leak := range []string{"eyJhbGciOiJIUzI1NiJ9", "abcdefghijklmnop", "s3cret"} {
			if strings.Contains(got, leak) {
				t.Errorf("Redact(%q) 泄漏了 %q: %q", c, leak, got)
			}
		}
	}
}

// TestRedactRecoveryCodes 覆盖 TOTP 恢复码形态。
//
// 它出现在日志里通常意味着某处把它回显了，而那本身就是个问题。但先脱敏
// 再说——一次泄漏就够用很久。
func TestRedactRecoveryCodes(t *testing.T) {
	got := Redact("生成的恢复码: ABCD-EFGH-IJKL")
	if strings.Contains(got, "ABCD-EFGH-IJKL") {
		t.Errorf("恢复码未被脱敏: %q", got)
	}
}

// TestRedactDoesNotEatNormalLogs 覆盖**误伤**。
//
// 过度脱敏会让日志失去价值，而它的后果是隐藏的：某天要排查时才发现关键的
// 那几行全被换成了占位符，而那时已经无法重现。这组用例专门守住这条线。
func TestRedactDoesNotEatNormalLogs(t *testing.T) {
	keep := []string{
		"用户 alice 登录成功",
		"任务 #123 已完成",
		"节点 node-1 上线",
		"GET /api/v1/vms 200 12ms",
		"磁盘使用率 85%",
		"虚拟机 vm-101 启动失败: 镜像不存在",
		// 含 key 这个子串但不是敏感字段。
		"keyboard 布局为 us",
		"排序键 key 不能为空",
	}
	for _, c := range keep {
		if got := Redact(c); got != c {
			t.Errorf("普通日志被误伤:\n  原文 %q\n  结果 %q", c, got)
		}
	}
}

// TestRedactMultipleOccurrences 覆盖一行里多处敏感。
//
// 只替换第一处是最常见的实现疏漏，而它留下的第二处往往正是最后一版配置。
func TestRedactMultipleOccurrences(t *testing.T) {
	got := Redact("password=first token=second secret=third")
	for _, leak := range []string{"first", "second", "third"} {
		if strings.Contains(got, leak) {
			t.Errorf("第 %q 处未被脱敏: %q", leak, got)
		}
	}
}

func TestRedactEmpty(t *testing.T) {
	if got := Redact(""); got != "" {
		t.Errorf("空串应原样返回，实际 %q", got)
	}
}

// TestRedactPlaceholderHidesLength 覆盖占位符的选择。
//
// 用等长的星号会让**长度本身**成为信息——一串 32 个星号与 8 个星号能让人
// 推断出密钥长度，而在有些系统上那足以缩小搜索范围。
func TestRedactPlaceholderHidesLength(t *testing.T) {
	long := Redact("password=" + strings.Repeat("x", 64))
	short := Redact("password=ab")
	if long != short {
		t.Errorf("不同长度的值应当产生相同的脱敏结果:\n  %q\n  %q", long, short)
	}
	if strings.Contains(long, "x") || strings.Contains(short, "ab") {
		t.Error("值的内容不应残留")
	}
}
