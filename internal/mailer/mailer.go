// Package mailer 提供 SMTP 发信能力（F-1-08）。
//
// 三条刻意的取舍：
//
//  1. **配置来自设置服务，不读环境变量**。环境变量锁定的设置项在界面上是
//     只读的（settings R-002）；如果这里再去读一遍环境变量，就会出现
//     「界面说锁定了、发信却用了另一套配置」——运维无从判断哪个生效。
//
//  2. **只依赖标准库**。net/smtp 加上 crypto/tls 足够覆盖 STARTTLS 与
//     隐式 TLS 两种常见部署；第三方邮件库带来的主要是更漂亮的多部分邮件
//     构造，而我们只需要发纯文本的一次性码。
//
//  3. **未配置时返回「未配置」而不是报错**。邮件是可选能力：没配 SMTP 的
//     部署照样能跑，只是找回密码与邮箱绑定不可用。把「没配」混同于
//     「配错了」会让界面在全新安装时显示一片红色。
package mailer

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// 加密方式。
const (
	SecurityNone     = "none"     // 明文，仅内网可信链路
	SecurityStartTLS = "starttls" // 先明文连接再升级（RFC 3207）
	SecurityTLS      = "tls"      // 隐式 TLS（通常 465 端口）
)

// ErrNotConfigured 表示尚未配置 SMTP。
//
// 调用方据此降级（例如隐藏「找回密码」入口），而不是当作故障弹红色横幅。
var ErrNotConfigured = errors.New("尚未配置邮件服务")

// Config 是一次发信所需的 SMTP 配置。
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	Security string
	From     string
	FromName string
	Timeout  time.Duration
}

// Ready 报告配置是否可用。
//
// 只要求主机、端口与发件地址：**用户名与密码可以为空**——内网中继服务器
// 常常不做认证，要求它们必填会让一部分正常部署无法保存配置。
func (c Config) Ready() bool {
	return c.Host != "" && c.Port > 0 && c.From != ""
}

// addr 返回拨号地址。
func (c Config) addr() string {
	port := c.Port
	if port <= 0 {
		port = 25
	}
	return net.JoinHostPort(c.Host, fmt.Sprint(port))
}

// Service 提供发信能力。
type Service struct {
	// load 每次发信都读一次配置：管理员在设置页保存 SMTP 后应当**立即**
	// 生效，重启才能发信的实现会让「测试邮件」按钮看起来是坏的。
	load func(context.Context) (Config, error)
}

// New 构造发信服务。load 返回当前生效的 SMTP 配置。
func New(load func(context.Context) (Config, error)) *Service {
	return &Service{load: load}
}

// Configured 报告邮件服务是否可用。
func (s *Service) Configured(ctx context.Context) (bool, error) {
	cfg, err := s.load(ctx)
	if err != nil {
		return false, err
	}
	return cfg.Ready(), nil
}

// Send 发送一封纯文本邮件。
//
// 收件人为空时直接失败：给空地址发信在多数服务器上表现为一次含糊的错误，
// 提前拦下来能给出可以说清楚的原因。
func (s *Service) Send(ctx context.Context, to, subject, body string) error {
	to = strings.TrimSpace(to)
	if to == "" {
		return errors.New("收件人为空")
	}
	cfg, err := s.load(ctx)
	if err != nil {
		return err
	}
	if !cfg.Ready() {
		return ErrNotConfigured
	}

	msg, err := buildMessage(cfg, to, subject, body)
	if err != nil {
		return err
	}

	client, err := dial(cfg)
	if err != nil {
		// 连接失败是**配置问题最常见的表现**（主机写错、端口不通、
		// 加密方式不匹配），原样带出去，界面上就能直接改。
		return fmt.Errorf("连接邮件服务器失败: %w", err)
	}
	defer func() {
		if err := client.Quit(); err != nil {
			// 退出失败不影响投递结果：数据已经写出去了，日志留痕即可。
			log.Printf("[mailer] 关闭 SMTP 连接失败: %v", err)
		}
	}()

	if cfg.Username != "" {
		auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("邮件服务器认证失败: %w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return fmt.Errorf("邮件服务器拒绝发件人: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("邮件服务器拒绝收件人: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("提交邮件内容失败: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("邮件投递失败: %w", err)
	}
	return nil
}

// SendTest 发送一封测试邮件。
//
// 与验证码邮件分开：测试邮件的正文要说"配置正确"，而不是给出一个用不了
// 的验证码——收到一封写着 000000 的信，用户无法判断这是成功还是失败。
func (s *Service) SendTest(ctx context.Context, to string) error {
	subject := "【" + s.siteName(ctx) + "】测试邮件"
	body := "这是一封测试邮件。\n\n" +
		"收到它说明邮件服务配置正确，找回密码与邮箱绑定可以正常使用。\n"
	return s.Send(ctx, to, subject, body)
}

// SendVerificationCode 发送一次性验证码。
//
// 场景名（绑定邮箱 / 找回密码）参与主题与正文：用户很可能同时收到过两种
// 邮件，只写"您的验证码是"会让人在改密码时以为自己被谁绑了邮箱。
func (s *Service) SendVerificationCode(ctx context.Context, to, scene, code string) error {
	subject := "【" + s.siteName(ctx) + "】" + scene + "验证码"
	body := "您正在" + scene + "，验证码是：\n\n    " + code + "\n\n" +
		"验证码 " + fmt.Sprint(int(codeValidMinutes)) + " 分钟内有效，且只能使用一次。\n" +
		"如果这不是你本人操作，请忽略这封邮件。\n"
	return s.Send(ctx, to, subject, body)
}

// codeValidMinutes 是验证码对外承诺的有效期（分钟）。
//
// 与 internal/auth 里较短的那个 TTL 对齐：正文说"15 分钟"而实际 10 分钟，
// 用户会认为系统坏了。
const codeValidMinutes = 10

// siteName 取对外展示的系统名称。
//
// 配置里的发件人名称就是用户在收件箱里看到的那个名字，用它而不是再引入
// 一个"站点名称"设置项——两个名称不一致时，用户无法判断这封邮件是谁发的。
func (s *Service) siteName(ctx context.Context) string {
	cfg, err := s.load(ctx)
	if err != nil || cfg.FromName == "" {
		return "K Cockpit"
	}
	return cfg.FromName
}

// dial 按加密方式建立 SMTP 连接。
func dial(cfg Config) (*smtp.Client, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tlsCfg := &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}

	if cfg.Security == SecurityTLS {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", cfg.addr(), tlsCfg)
		if err != nil {
			return nil, err
		}
		return smtp.NewClient(conn, cfg.Host)
	}

	conn, err := net.DialTimeout("tcp", cfg.addr(), timeout)
	if err != nil {
		return nil, err
	}
	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		return nil, err
	}
	if cfg.Security == SecurityStartTLS {
		// 服务器不支持 STARTTLS 时按明文继续是危险的：凭据会在线路上裸奔，
		// 而用户以为自己选了加密。宁可失败。
		if err := client.StartTLS(tlsCfg); err != nil {
			_ = client.Quit()
			return nil, fmt.Errorf("服务器不支持 STARTTLS: %w", err)
		}
	}
	return client, nil
}

// buildMessage 构造 RFC 5322 报文。
//
// 主题与发件人名称走 RFC 2047 编码：中文站名与中文主题放在裸 8bit 头里
// 会被中途的服务器改写，表现为「测试邮件」变成一串问号。
func buildMessage(cfg Config, to, subject, body string) ([]byte, error) {
	if err := validateAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("发件地址不合法: %w", err)
	}
	if err := validateAddress(to); err != nil {
		return nil, fmt.Errorf("收件地址不合法: %w", err)
	}
	from := cfg.From
	if cfg.FromName != "" {
		from = encodeHeader(cfg.FromName) + " <" + cfg.From + ">"
	}

	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: <" + messageID() + ">\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteString("\r\n")
	}
	return []byte(b.String()), nil
}

// validateAddress 只做最小校验：SMTP 会话本身会给出权威判断，这里真正要
// 挡住的是**注入额外邮件头**——地址里出现换行就能往报文里塞任意头部。
func validateAddress(addr string) error {
	if addr == "" || strings.ContainsAny(addr, "\r\n") {
		return errors.New("地址为空或含换行")
	}
	if !strings.Contains(addr, "@") {
		return errors.New("地址缺少 @")
	}
	return nil
}

// encodeHeader 用 RFC 2047 的 B 编码（Base64 + UTF-8）；纯 ASCII 原样返回。
func encodeHeader(s string) string {
	if isASCII(s) {
		return s
	}
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?="
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// messageID 生成报文标识：时间加随机数，足够避免同一秒内的重复。
func messageID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d.panel", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d.%s.panel", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}
