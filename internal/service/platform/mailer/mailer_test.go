package mailer_test

import (
	"bufio"
	"context"
	"errors"
	"mime"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"k_cockpit/internal/service/platform/mailer"
)

// fakeSMTP 是一个够用的 SMTP 服务端：只实现本项目会用到的那些命令。
//
// 为什么不用真发信或者打桩到「不建连接」：`Send` 的价值几乎全在真实会话
// 顺序里——EHLO、AUTH、MAIL、RCPT、DATA 任一步失败都要给出不同的可读原因。
// 打桩掉连接就只能验证到「它没崩」。
type fakeSMTP struct {
	ln net.Listener

	// advertiseAuth 决定 EHLO 响应里是否宣告 AUTH PLAIN。
	advertiseAuth bool
	// authOK=false 时 AUTH 返回 535。
	authOK bool
	// rejectRcpt=true 时 RCPT TO 返回 550。
	rejectRcpt bool

	mu     sync.Mutex
	msgs   []string
	authed bool
}

func startFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &fakeSMTP{ln: ln, advertiseAuth: true, authOK: true}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTP) port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

func (s *fakeSMTP) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgs...)
}

func (s *fakeSMTP) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	send := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}
	send("220 fake ESMTP ready")

	var data strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")

		if inData {
			if line == "." {
				inData = false
				s.mu.Lock()
				s.msgs = append(s.msgs, data.String())
				s.mu.Unlock()
				data.Reset()
				send("250 2.0.0 queued")
				continue
			}
			data.WriteString(line + "\r\n")
			continue
		}

		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			_, _ = w.WriteString("250-fake\r\n")
			if s.advertiseAuth {
				_, _ = w.WriteString("250-AUTH PLAIN\r\n")
			}
			_, _ = w.WriteString("250 OK\r\n")
			_ = w.Flush()
		case strings.HasPrefix(upper, "HELO"):
			send("250 fake")
		case strings.HasPrefix(upper, "AUTH"):
			s.mu.Lock()
			s.authed = true
			s.mu.Unlock()
			if s.authOK {
				send("235 2.7.0 accepted")
			} else {
				send("535 5.7.8 bad credentials")
			}
		case strings.HasPrefix(upper, "MAIL FROM"):
			send("250 2.1.0 ok")
		case strings.HasPrefix(upper, "RCPT TO"):
			if s.rejectRcpt {
				send("550 5.1.1 no such user")
			} else {
				send("250 2.1.5 ok")
			}
		case upper == "DATA":
			send("354 end data with <CRLF>.<CRLF>")
			inData = true
		case upper == "QUIT":
			send("221 bye")
			return
		default:
			send("250 ok")
		}
	}
}

// config 返回指向假服务端的配置。
func (s *fakeSMTP) config() mailer.Config {
	return mailer.Config{
		Host: "127.0.0.1", Port: s.port(),
		From: "noreply@example.com", FromName: "我的面板",
		Security: mailer.SecurityNone, Timeout: 3 * time.Second,
	}
}

func stub(cfg mailer.Config) func(context.Context) (mailer.Config, error) {
	return func(context.Context) (mailer.Config, error) { return cfg, nil }
}

func decodeHeader(t *testing.T, s string) string {
	t.Helper()
	out, err := new(mime.WordDecoder).DecodeHeader(s)
	if err != nil {
		t.Fatalf("头部不是合法的 RFC 2047 编码: %q (%v)", s, err)
	}
	return out
}

// TestConfigReady 覆盖「什么算配好了」。用户名密码可以为空：内网中继常
// 不做认证，要求必填会让一部分正常部署根本存不下配置。
func TestConfigReady(t *testing.T) {
	cases := []struct {
		name string
		cfg  mailer.Config
		want bool
	}{
		{"完整", mailer.Config{Host: "smtp.example.com", Port: 587, From: "a@b.c"}, true},
		{"无认证", mailer.Config{Host: "relay.local", Port: 25, From: "a@b.c"}, true},
		{"缺主机", mailer.Config{Port: 25, From: "a@b.c"}, false},
		{"缺端口", mailer.Config{Host: "smtp.example.com", From: "a@b.c"}, false},
		{"端口为负", mailer.Config{Host: "smtp.example.com", Port: -1, From: "a@b.c"}, false},
		{"缺发件人", mailer.Config{Host: "smtp.example.com", Port: 25}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.Ready(); got != c.want {
				t.Errorf("Ready() = %v, 期望 %v", got, c.want)
			}
		})
	}
}

func TestConfiguredPropagatesLoadError(t *testing.T) {
	boom := errors.New("设置读取失败")
	svc := mailer.New(func(context.Context) (mailer.Config, error) { return mailer.Config{}, boom })

	if _, err := svc.Configured(context.Background()); !errors.Is(err, boom) {
		t.Errorf("配置读取失败应原样上报, 实际 %v", err)
	}

	ok := mailer.New(stub(mailer.Config{Host: "h", Port: 1, From: "a@b.c"}))
	if configured, err := ok.Configured(context.Background()); err != nil || !configured {
		t.Errorf("Configured() = %v, %v; 期望 true, nil", configured, err)
	}
}

// TestSendRejectsEmptyRecipient：给空地址发信在各家服务器上都是一句含糊的
// 报错，提前拦下来才能说清原因。
func TestSendRejectsEmptyRecipient(t *testing.T) {
	svc := mailer.New(stub(mailer.Config{Host: "127.0.0.1", Port: 1, From: "a@b.c"}))

	err := svc.Send(context.Background(), "   ", "主题", "正文")
	if err == nil || !strings.Contains(err.Error(), "收件人为空") {
		t.Fatalf("空收件人应被拒绝, 实际 %v", err)
	}
}

// TestSendReportsNotConfigured：没配 SMTP 是「不可用」而不是「故障」，
// 调用方据此隐藏入口，而不是弹红色横幅。
func TestSendReportsNotConfigured(t *testing.T) {
	svc := mailer.New(stub(mailer.Config{}))

	err := svc.Send(context.Background(), "user@example.com", "主题", "正文")
	if !errors.Is(err, mailer.ErrNotConfigured) {
		t.Fatalf("未配置时应返回 ErrNotConfigured, 实际 %v", err)
	}
}

// TestSendBlocksHeaderInjection 是本包的安全边界：地址里带换行就能往报文里
// 塞任意头部（Bcc、Reply-To），因此必须在拨号**之前**拒绝。
func TestSendBlocksHeaderInjection(t *testing.T) {
	svc := mailer.New(stub(mailer.Config{Host: "127.0.0.1", Port: 1, From: "a@b.c"}))
	ctx := context.Background()

	err := svc.Send(ctx, "victim@example.com\r\nBcc: attacker@evil.com", "主题", "正文")
	if err == nil || !strings.Contains(err.Error(), "收件地址不合法") {
		t.Errorf("收件人注入换行必须被拒绝, 实际 %v", err)
	}

	badFrom := mailer.New(stub(mailer.Config{
		Host: "127.0.0.1", Port: 1, From: "a@b.c\nBcc: attacker@evil.com",
	}))
	if err := badFrom.Send(ctx, "victim@example.com", "主题", "正文"); err == nil ||
		!strings.Contains(err.Error(), "发件地址不合法") {
		t.Errorf("发件人注入换行必须被拒绝, 实际 %v", err)
	}

	// 没有 @ 的地址同样在本地就挡掉。
	if err := svc.Send(ctx, "not-an-address", "主题", "正文"); err == nil ||
		!strings.Contains(err.Error(), "缺少 @") {
		t.Errorf("非法地址应说明缺少 @, 实际 %v", err)
	}
}

// TestSendDeliversMessage 走完整会话并检查落地报文：头部的编码、必需字段
// 与正文都必须在服务端看到的字节里成立。
func TestSendDeliversMessage(t *testing.T) {
	srv := startFakeSMTP(t)
	svc := mailer.New(stub(srv.config()))

	if err := svc.Send(context.Background(), "user@example.com", "你的验证码", "正文内容"); err != nil {
		t.Fatalf("发信失败: %v", err)
	}
	msgs := srv.messages()
	if len(msgs) != 1 {
		t.Fatalf("服务端应收到 1 封, 实际 %d", len(msgs))
	}
	raw := msgs[0]

	for _, want := range []string{
		"To: user@example.com\r\n",
		"MIME-Version: 1.0\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"Message-ID: <",
		"\r\n\r\n正文内容",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("报文中缺少 %q\n---\n%s", want, raw)
		}
	}

	// 中文主题必须走 RFC 2047，否则中途服务器会把它改成问号。
	subject := headerValue(t, raw, "Subject")
	if !strings.Contains(subject, "=?UTF-8?B?") {
		t.Errorf("中文主题未编码: %q", subject)
	}
	if got := decodeHeader(t, subject); got != "你的验证码" {
		t.Errorf("主题解码 = %q", got)
	}
	from := headerValue(t, raw, "From")
	if !strings.Contains(from, "<noreply@example.com>") {
		t.Errorf("From 缺少地址部分: %q", from)
	}
	if got := decodeHeader(t, strings.TrimSuffix(strings.SplitN(from, "<", 2)[0], " ")); got != "我的面板" {
		t.Errorf("发件人名称解码 = %q", got)
	}
}

// TestSendTestSaysConfigIsCorrect：测试邮件不能给一个用不了的验证码，
// 否则用户无法判断这封是成功还是失败。
func TestSendTestSaysConfigIsCorrect(t *testing.T) {
	srv := startFakeSMTP(t)
	svc := mailer.New(stub(srv.config()))

	if err := svc.SendTest(context.Background(), "admin@example.com"); err != nil {
		t.Fatalf("发送测试邮件失败: %v", err)
	}
	raw := srv.messages()[0]
	if got := decodeHeader(t, headerValue(t, raw, "Subject")); got != "【我的面板】测试邮件" {
		t.Errorf("主题 = %q", got)
	}
	if !strings.Contains(raw, "配置正确") {
		t.Error("测试邮件正文应说明配置正确")
	}
	if strings.Contains(raw, "验证码是") {
		t.Error("测试邮件不应包含验证码")
	}
}

// TestSendVerificationCodeCarriesSceneAndTTL：场景名进主题与正文，
// 用户才不会把「改密码」的验证码误认为「有人绑我邮箱」。
func TestSendVerificationCodeCarriesSceneAndTTL(t *testing.T) {
	srv := startFakeSMTP(t)
	svc := mailer.New(stub(srv.config()))

	if err := svc.SendVerificationCode(context.Background(), "u@example.com", "找回密码", "483920"); err != nil {
		t.Fatalf("发送验证码失败: %v", err)
	}
	raw := srv.messages()[0]
	if got := decodeHeader(t, headerValue(t, raw, "Subject")); got != "【我的面板】找回密码验证码" {
		t.Errorf("主题 = %q", got)
	}
	for _, want := range []string{"483920", "找回密码", "10 分钟内有效", "只能使用一次"} {
		if !strings.Contains(raw, want) {
			t.Errorf("正文缺少 %q:\n%s", want, raw)
		}
	}
}

// TestSendAuthenticatesWhenCredentialsGiven 覆盖需要认证的中继。
func TestSendAuthenticatesWhenCredentialsGiven(t *testing.T) {
	srv := startFakeSMTP(t)
	cfg := srv.config()
	cfg.Username, cfg.Password = "relay-user", "relay-pass"
	svc := mailer.New(stub(cfg))

	if err := svc.Send(context.Background(), "u@example.com", "s", "b"); err != nil {
		t.Fatalf("带认证发信失败: %v", err)
	}
	srv.mu.Lock()
	authed := srv.authed
	srv.mu.Unlock()
	if !authed {
		t.Error("配置了用户名却未发起 AUTH")
	}
}

func TestSendReportsAuthFailure(t *testing.T) {
	srv := startFakeSMTP(t)
	srv.authOK = false
	cfg := srv.config()
	cfg.Username, cfg.Password = "u", "wrong"
	svc := mailer.New(stub(cfg))

	err := svc.Send(context.Background(), "u@example.com", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("认证被拒应报认证失败, 实际 %v", err)
	}
}

func TestSendReportsRcptRejection(t *testing.T) {
	srv := startFakeSMTP(t)
	srv.rejectRcpt = true
	svc := mailer.New(stub(srv.config()))

	err := svc.Send(context.Background(), "nobody@example.com", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "拒绝收件人") {
		t.Fatalf("收件人被拒应明确报出, 实际 %v", err)
	}
}

// TestSendReportsConnectFailure：主机写错 / 端口不通是最常见的配置问题，
// 报错必须原样带出底层原因，界面才能直接改。
func TestSendReportsConnectFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // 立刻关掉：这个端口一定拒绝连接

	svc := mailer.New(stub(mailer.Config{
		Host: "127.0.0.1", Port: port, From: "a@b.c", Timeout: time.Second,
	}))
	err = svc.Send(context.Background(), "u@example.com", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "连接邮件服务器失败") {
		t.Fatalf("连接失败应报出原因, 实际 %v", err)
	}
}

// TestStartTLSIsRequiredWhenConfigured：服务器不支持 STARTTLS 时**不能**
// 退回明文——凭据会在线路上裸奔，而用户以为自己选了加密。
func TestStartTLSIsRequiredWhenConfigured(t *testing.T) {
	srv := startFakeSMTP(t)
	cfg := srv.config()
	cfg.Security = mailer.SecurityStartTLS
	svc := mailer.New(stub(cfg))

	err := svc.Send(context.Background(), "u@example.com", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("不支持 STARTTLS 时必须失败, 实际 %v", err)
	}
	if len(srv.messages()) != 0 {
		t.Error("STARTTLS 失败后不应投递任何报文")
	}
}

// headerValue 取出报文里某个头部的原始值（不含头部名）。
func headerValue(t *testing.T, raw, name string) string {
	t.Helper()
	for _, line := range strings.Split(raw, "\r\n") {
		if line == "" {
			break // 头部区结束
		}
		if strings.HasPrefix(line, name+": ") {
			return strings.TrimPrefix(line, name+": ")
		}
	}
	t.Fatalf("报文缺少头部 %s:\n%s", name, raw)
	return ""
}
