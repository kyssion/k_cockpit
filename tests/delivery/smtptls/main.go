// smtptls 是一个最小的 SMTP 测试服务器，用于验证**真实 TLS 投递**（U-37）。
//
// 为什么要在进程外起一个真的 SMTP：mailer 的单测用进程内假服务器覆盖了
// 「要求 STARTTLS 但服务端不支持时必须失败」，但那条用例走的是同一份代码
// 里的桩，验不了三件只有真连接才能回答的事：
//
//  1. TLS 握手能不能成（协议版本、密码套件、SNI）；
//  2. 证书校验是不是真的在做——自签证书**必须**被拒，除非把它装进信任库；
//  3. 升级之后 AUTH 与投递能不能接着走完（很多实现只在明文阶段对，
//     一升级就卡住）。
//
// 三种模式，分别对应三种真实部署：
//
//	-starttls  宣告 250-STARTTLS，收到 STARTTLS 后升级（587 端口的形态）
//	-implicit  连上即 TLS（465 端口的形态）
//	-plain     不宣告 STARTTLS，只有明文（用来验「不得静默降级」）
//
// 用法：
//
//	smtptls -starttls :2587 -implicit :2465 -plain :2526 \
//	        -cert cert.pem -key key.pem -user kc -pass kc-secret
//
// 只实现发信所需的那一小段协议（EHLO/AUTH/MAIL/RCPT/DATA/QUIT）；它不是
// 邮件服务器，收到内容只做摘要打印，不落盘。
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"io"
	"log"
	"net"
	"strings"
	"time"
)

func main() {
	starttlsAddr := flag.String("starttls", "", "STARTTLS 模式监听地址，如 :2587")
	implicitAddr := flag.String("implicit", "", "隐式 TLS 模式监听地址，如 :2465")
	plainAddr := flag.String("plain", "", "纯明文模式监听地址，如 :2526")
	certFile := flag.String("cert", "", "证书文件（PEM）")
	keyFile := flag.String("key", "", "私钥文件（PEM）")
	user := flag.String("user", "", "AUTH PLAIN 期望的用户名（留空表示不校验）")
	pass := flag.String("pass", "", "AUTH PLAIN 期望的口令（留空表示不校验）")
	flag.Parse()

	var tlsConf *tls.Config
	if *certFile != "" && *keyFile != "" {
		cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			log.Fatalf("加载证书失败: %v", err)
		}
		// MinVersion 与 mailer 客户端一致，避免「客户端要求 1.2、服务端只给
		// 1.0」这类配置差异伪装成代码缺陷。
		tlsConf = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}

	specs := []struct {
		addr string
		mode string
	}{
		{*starttlsAddr, "starttls"},
		{*implicitAddr, "implicit"},
		{*plainAddr, "plain"},
	}

	started := 0
	for _, sp := range specs {
		if sp.addr == "" {
			continue
		}
		ln, err := net.Listen("tcp", sp.addr)
		if err != nil {
			log.Fatalf("监听 %s 失败: %v", sp.addr, err)
		}
		s := &server{mode: sp.mode, tlsConf: tlsConf, user: *user, pass: *pass}
		log.Printf("[smtptls] mode=%s listen=%s", sp.mode, sp.addr)
		go func() {
			for {
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				go s.handle(conn)
			}
		}()
		started++
	}
	if started == 0 {
		log.Fatal("至少需要一个监听地址（-starttls / -implicit / -plain）")
	}
	select {} // 一直跑，由外部 SIGTERM 结束
}

type server struct {
	mode    string
	tlsConf *tls.Config
	user    string
	pass    string
}

func (s *server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))

	var rw io.ReadWriter = conn
	if s.mode == "implicit" {
		if s.tlsConf == nil {
			log.Printf("[smtptls] implicit 模式需要 -cert/-key")
			return
		}
		tc := tls.Server(conn, s.tlsConf)
		if err := tc.Handshake(); err != nil {
			log.Printf("[smtptls] mode=implicit 握手失败: %v", err)
			return
		}
		rw = tc
	}

	br := bufio.NewReader(rw)
	bw := bufio.NewWriter(rw)
	upgraded := s.mode == "implicit"
	authed := s.user == ""

	reply := func(lines ...string) {
		for _, l := range lines {
			_, _ = bw.WriteString(l + "\r\n")
		}
		_ = bw.Flush()
	}
	// 每行都单独打印，便于从日志里区分「握手失败」与「认证失败」——
	// 两者的排障方向完全不同。
	trace := func(dir, text string) {
		log.Printf("[smtptls] mode=%s %s%s", s.mode, dir, text)
	}

	reply("220 kc-smtptls ESMTP ready")

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		trace("C: ", line)
		upper := strings.ToUpper(line)

		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			out := []string{"250-kc-smtptls greets you"}
			if s.mode == "starttls" && !upgraded {
				out = append(out, "250-STARTTLS")
			}
			// 明文阶段**不宣告 AUTH**：避免测试把凭据发在明文链路上，
			// 那样即使被测代码「降级」了也看不出来。
			if upgraded && s.user != "" {
				out = append(out, "250-AUTH PLAIN")
			}
			out = append(out, "250 8BITMIME")
			reply(out...)

		case upper == "STARTTLS":
			if s.mode != "starttls" || upgraded || s.tlsConf == nil {
				reply("454 4.7.0 TLS not available")
				continue
			}
			reply("220 2.0.0 Ready to start TLS")
			tc := tls.Server(conn, s.tlsConf)
			if err := tc.Handshake(); err != nil {
				log.Printf("[smtptls] mode=starttls 升级失败: %v", err)
				return
			}
			rw = tc
			br = bufio.NewReader(rw)
			bw = bufio.NewWriter(rw)
			upgraded = true
			authed = s.user == ""

		case strings.HasPrefix(upper, "AUTH PLAIN"):
			if !upgraded && s.mode != "implicit" {
				reply("538 5.7.11 Encryption required for requested authentication mechanism")
				continue
			}
			fields := strings.Fields(line)
			if len(fields) != 3 {
				reply("501 5.5.4 需要 AUTH PLAIN <base64>")
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(fields[2])
			if err != nil {
				reply("501 5.5.2 无法解码")
				continue
			}
			// 格式：authzid \0 authcid \0 passwd
			seg := strings.Split(string(raw), "\x00")
			if len(seg) == 3 && seg[1] == s.user && seg[2] == s.pass {
				authed = true
				reply("235 2.7.0 Authentication successful")
				continue
			}
			reply("535 5.7.8 Authentication credentials invalid")

		case strings.HasPrefix(upper, "MAIL FROM"):
			if !authed {
				reply("530 5.7.0 Authentication required")
				continue
			}
			reply("250 2.1.0 OK")

		case strings.HasPrefix(upper, "RCPT TO"):
			if !authed {
				reply("530 5.7.0 Authentication required")
				continue
			}
			reply("250 2.1.5 OK")

		case upper == "DATA":
			if !authed {
				reply("530 5.7.0 Authentication required")
				continue
			}
			reply("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				l, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if strings.TrimRight(l, "\r\n") == "." {
					break
				}
				body.WriteString(l)
			}
			log.Printf("[smtptls] mode=%s 收到邮件 tls=%v bytes=%d subject=%q",
				s.mode, upgraded, body.Len(), headerValue(body.String(), "Subject"))
			reply("250 2.0.0 OK: queued")

		case upper == "RSET", upper == "NOOP":
			reply("250 2.0.0 OK")

		case upper == "QUIT":
			reply("221 2.0.0 Bye")
			return

		default:
			reply("502 5.5.2 Command not implemented")
		}
	}
}

// headerValue 取一个头部的值（大小写不敏感），用于日志摘要。
func headerValue(msg, name string) string {
	for _, line := range strings.Split(msg, "\n") {
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(line[:colon]), name) {
			return strings.TrimSpace(line[colon+1:])
		}
	}
	return ""
}
