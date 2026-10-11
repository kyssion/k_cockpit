// Package nodeagent 是节点侧 agent 的实现：连接控制面（反向长连接）、
// 心跳上报、接收并执行领域操作。
//
// 与 internal/agent/hub 是同一协议的两侧。本包只负责「通道与分发」；
// 各 OpKind 的真实执行器随分期落地（P1 只读探测、P2 生命周期……），
// 注册表里没有的操作返回明确的「未实现」结果——那是能力边界的诚实表达，
// 而不是假装成功。
package nodeagent

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// State 是 agent 的持久化身份：enroll 成功后写入，之后每次启动加载。
type State struct {
	// AgentID 是节点身份（uuid，控制面签发后不得更改——f-6-01：同 uuid
	// 重注册保留身份与历史）。
	AgentID string `json:"agent_id"`
	// Server 是控制面网关地址（host:port）。
	Server string `json:"server"`
}

// StateDir 布局：
//
//	{dir}/agent.json   身份与服务器地址
//	{dir}/ca.pem       控制面 CA（enroll 时取得，验证服务端证书链的锚点）
//	{dir}/client.pem   客户端证书
//	{dir}/client.key   客户端私钥（0600）
type StateDir struct{ Dir string }

func (s StateDir) agentFile() string { return filepath.Join(s.Dir, "agent.json") }
func (s StateDir) caFile() string    { return filepath.Join(s.Dir, "ca.pem") }
func (s StateDir) certFile() string  { return filepath.Join(s.Dir, "client.pem") }
func (s StateDir) keyFile() string   { return filepath.Join(s.Dir, "client.key") }

// Load 读回已 enroll 的状态；未 enroll 返回 os.ErrNotExist 语义。
func (s StateDir) Load() (*State, error) {
	b, err := os.ReadFile(s.agentFile())
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("agent.json 损坏: %w", err)
	}
	if st.AgentID == "" || st.Server == "" {
		return nil, errors.New("agent.json 缺少 agent_id 或 server")
	}
	return &st, nil
}

// SaveEnrollment 原子写入 enroll 结果（身份 + 证书三件套）。
//
// 顺序刻意为「先写证书，后写 agent.json」：agent.json 是"我已 enroll"的
// 标志，它出现时证书必须已经就位——反过来的顺序会产生一个启动即崩的
// 半 enroll 状态。
func (s StateDir) SaveEnrollment(st *State, caPEM, certPEM, keyPEM []byte) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.caFile(), caPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(s.certFile(), certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(s.keyFile(), keyPEM, 0o600); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.agentFile() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.agentFile())
}

// Enrolled 报告本目录是否已完成 enroll。
func (s StateDir) Enrolled() bool {
	_, err := s.Load()
	return err == nil
}

// ClientTLS 返回数据通道用的 TLS 配置：出示客户端证书，并**只信任
// enroll 时固定下来的那把 CA**。
//
// InsecureSkipVerify + 自定义 VerifyPeerCertificate 是「私有 CA + 动态
// 服务端地址」的标准写法：服务端证书的 IP SAN 随部署网络变化，而信任锚
// 是 enroll 时拿到的那把 CA——校验「证书链由它签发」比校验主机名更符合
// 这里的信任模型。
func (s StateDir) ClientTLS() (*tls.Config, error) {
	caPEM, err := os.ReadFile(s.caFile())
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("CA 证书无法解析")
	}
	cert, err := tls.LoadX509KeyPair(s.certFile(), s.keyFile())
	if err != nil {
		return nil, fmt.Errorf("加载客户端证书: %w", err)
	}
	return &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
			// 校验逻辑由 VerifyPeerCertificate 接管（见上）。
			InsecureSkipVerify: true,
			VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
				if len(rawCerts) == 0 {
					return errors.New("服务端未出示证书")
				}
				leaf, err := x509.ParseCertificate(rawCerts[0])
				if err != nil {
					return err
				}
				inters := x509.NewCertPool()
				for _, raw := range rawCerts[1:] {
					if c, err := x509.ParseCertificate(raw); err == nil {
						inters.AddCert(c)
					}
				}
				opts := x509.VerifyOptions{Roots: pool, Intermediates: inters}
				if _, err := leaf.Verify(opts); err != nil {
					return fmt.Errorf("服务端证书链校验失败: %w", err)
				}
				return nil
			},
		},
		nil
}

// EnrollTLS 返回 enroll（首次注册）用的 TLS 配置：无客户端证书、不校验
// 服务端（信任锚是**令牌本身**——它是一次性高熵秘密，能完成注册即证明
// 对端是控制面）。
func (s StateDir) EnrollTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}
