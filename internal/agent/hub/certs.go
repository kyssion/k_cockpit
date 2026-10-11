// Package hub 是控制面侧的 agent 连接枢纽：维护在线连接，把 agent.Client
// 的领域操作路由到对应节点的反向长连接上，并把心跳落库。
//
// 它与 internal/nodeagent 是同一条协议的两侧（internal/agentproto 定义线上
// 形状，internal/agent 定义控制面内存形状，本包负责翻译）。
package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// certManager 管理控制面侧的 CA、服务端证书，以及注册时给 agent 签发的
// 客户端证书。
//
// PKI 形态（ADR-0005）：控制面自身是**私有 CA**。agent 首次凭一次性令牌
// 经 Register 换取（CA 证书 + 客户端证书 + 私钥），之后每次连接出示客户端
// 证书、并验证服务端证书链是否由该 CA 签发——令牌用后即焚，长期信任完全
// 建立在证书上。
type certManager struct {
	dir       string
	caCert    *x509.Certificate
	caKey     *ecdsa.PrivateKey
	server    tls.Certificate
	caPEM     []byte
	serverPEM []byte
}

// loadOrCreateCA 加载（或首次生成）CA 与服务端证书。
//
// 服务端证书按当前网卡 IP 签发；IP 变化（换网络环境）时删除 server.pem
// 重启即可重新签发——CA 不变，已发出去的 agent 客户端证书不受影响。
func loadOrCreateCA(dir string) (*certManager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 PKI 目录: %w", err)
	}
	m := &certManager{dir: dir}

	caKeyPath := filepath.Join(dir, "ca.key")
	caCertPath := filepath.Join(dir, "ca.crt")
	caKey, caCert, err := loadOrGenerateCA(caKeyPath, caCertPath)
	if err != nil {
		return nil, err
	}
	m.caKey, m.caCert = caKey, caCert

	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书: %w", err)
	}
	m.caPEM = caPEM

	// 服务端证书：IP 变化时由运维删除重签（注释即操作说明，不写自动重签
	// 的隐式魔法——证书轮换应当是一次显式动作）。
	serverPath := filepath.Join(dir, "server.pem")
	keyPath := filepath.Join(dir, "server.key")
	if err := ensureServerCert(serverPath, keyPath, caCert, caKey); err != nil {
		return nil, err
	}
	server, err := tls.LoadX509KeyPair(serverPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("加载服务端证书: %w", err)
	}
	m.server = server

	serverPEM, err := os.ReadFile(serverPath)
	if err != nil {
		return nil, fmt.Errorf("读取服务端证书: %w", err)
	}
	m.serverPEM = serverPEM
	return m, nil
}

// TLSConfig 返回 gRPC 服务端的 TLS 配置。
//
// VerifyClientCertIfGiven：注册（Register）发生在 agent 还没有客户端证书
// 的时刻，因此不能要求所有连接都出示证书；「无证书只能 Register」由
// handler 侧按方法判断（见 grpc.go 的鉴权段）。
func (m *certManager) TLSConfig() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(m.caCert)
	return &tls.Config{
		Certificates: []tls.Certificate{m.server},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}
}

// IssueClientCert 为一次注册签发客户端证书。CN 即 agent 身份（agent_id），
// 服务端据此把连接映射到节点行。
func (m *certManager) IssueClientCert(agentID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成客户端密钥: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agentID, Organization: []string{"k_cockpit"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("签发客户端证书: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// loadOrGenerateCA 加载或生成 CA 私钥与自签证书。
func loadOrGenerateCA(keyPath, certPath string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	if key, cert, err := loadCA(keyPath, certPath); err == nil {
		return key, cert, nil
	} else if !os.IsNotExist(err) && !isPEMError(err) {
		// 文件存在但损坏时不静默覆盖：PKI 材料的"消失"必须显式处理。
		log.Printf("[agent-hub] ⚠️ 既有 CA 加载失败，将生成新 CA（已连接的 agent 需重新 enroll）: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("生成 CA 密钥: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "k_cockpit agent CA", Organization: []string{"k_cockpit"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("自签 CA 证书: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, nil, err
	}
	if err := writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return key, cert, nil
}

// ensureServerCert 确保（由本 CA 签发的）服务端证书存在。
//
// SAN 覆盖本机全部非环回 IPv4/IPv6 地址与 localhost：agent 从内网任意
// 地址回连。地址变化时删除 server.pem 重启即重签。
func ensureServerCert(certPath, keyPath string, ca *x509.Certificate, caKey *ecdsa.PrivateKey) error {
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("生成服务端密钥: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "k_cockpit control plane", Organization: []string{"k_cockpit"}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  append(localIPs(), net.ParseIP("127.0.0.1"), net.ParseIP("::1")),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("签发服务端证书: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

// localIPs 枚举本机非环回地址（服务端证书的 IP SAN）。
func localIPs() []net.IP {
	out := []net.IP{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP)
		}
	}
	return out
}

// loadCA 从文件加载 CA。
func loadCA(keyPath, certPath string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	certBlock, _ := pem.Decode(certPEM)
	if keyBlock == nil || certBlock == nil {
		return nil, nil, fmt.Errorf("PEM 解码失败")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return key, cert, nil
}

// isPEMError 判断是否为解析类错误（区别于"文件不存在"）。
func isPEMError(err error) bool {
	return !os.IsNotExist(err)
}

// writeAtomic 原子写文件（临时文件 + rename），避免半份密钥。
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
