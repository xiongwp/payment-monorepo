// mtls.go — 服务间 mTLS 支持。
//
// 沿用 kms-manage P0 安全方案：
//   - Server: RequireAndVerifyClientCert (强制客户端必须出示证书)
//   - 自定义 VerifyPeerCertificate 校验 client cert 的 SAN URI/DNS 在白名单内
//   - Client: 加载 client cert，调用方互信
//
// 服务部署时:
//   1. Internal CA 给每个 service 签 cert (CN=service-name, SAN URI=spiffe://...)
//   2. 每个 pod mount cert 到 /etc/certs/
//   3. payment-mw 读 env CERT_FILE / KEY_FILE / CA_FILE 自动启用 mTLS
//   4. 白名单走 env ALLOWED_PEERS="billing-system,refund-engine,..." (CSV)
//
// 证书签发 SOP 见同目录 MTLS.md。

package mw

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// MTLSConfig mTLS 服务端配置。
type MTLSConfig struct {
	CertFile     string   // server cert
	KeyFile      string   // server key
	CAFile       string   // 根 CA (用于验客户端 cert)
	AllowedPeers []string // 允许的 client CN / SAN 列表（空 = 不限）
}

// LoadFromEnv 从环境变量构造。
// 示例:
//   MTLS_CERT_FILE=/etc/certs/server.crt
//   MTLS_KEY_FILE=/etc/certs/server.key
//   MTLS_CA_FILE=/etc/certs/ca.crt
//   MTLS_ALLOWED_PEERS=billing-system,refund-engine,merchant-webhook
//
// 任一 cert/key/ca 文件不存在 → 返 nil（dev 模式 plain HTTP）。
func LoadMTLSFromEnv() *MTLSConfig {
	cert := os.Getenv("MTLS_CERT_FILE")
	key := os.Getenv("MTLS_KEY_FILE")
	ca := os.Getenv("MTLS_CA_FILE")
	if cert == "" || key == "" || ca == "" {
		return nil
	}
	for _, f := range []string{cert, key, ca} {
		if _, err := os.Stat(f); err != nil {
			return nil
		}
	}
	peers := os.Getenv("MTLS_ALLOWED_PEERS")
	var allowed []string
	if peers != "" {
		for _, p := range strings.Split(peers, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				allowed = append(allowed, p)
			}
		}
	}
	return &MTLSConfig{CertFile: cert, KeyFile: key, CAFile: ca, AllowedPeers: allowed}
}

// ServerTLS 构造给 http.Server 用的 *tls.Config。
//
// 行为:
//   - RequireAndVerifyClientCert 强制双向校验
//   - CA 验签
//   - 加 VerifyPeerCertificate 校验 client cert 的 CN/SAN URI 在白名单
//   - MinVersion TLS1.3
func (c *MTLSConfig) ServerTLS() (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}
	caBytes, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caBytes) {
		return nil, fmt.Errorf("invalid CA PEM")
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}
	if len(c.AllowedPeers) > 0 {
		allowed := map[string]struct{}{}
		for _, p := range c.AllowedPeers {
			allowed[p] = struct{}{}
		}
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no client cert presented")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse client cert: %w", err)
			}
			// 1. CN 匹配
			if _, ok := allowed[leaf.Subject.CommonName]; ok {
				return nil
			}
			// 2. SAN DNS 匹配
			for _, dns := range leaf.DNSNames {
				if _, ok := allowed[dns]; ok {
					return nil
				}
			}
			// 3. SAN URI 匹配（SPIFFE 用）
			for _, uri := range leaf.URIs {
				if _, ok := allowed[uri.String()]; ok {
					return nil
				}
			}
			return fmt.Errorf("client cert peer %q (DNS=%v URI=%v) not in allowed list",
				leaf.Subject.CommonName, leaf.DNSNames, leaf.URIs)
		}
	}
	return tlsCfg, nil
}

// ClientTLS 构造给 http.Client 出站用的 *tls.Config。
func (c *MTLSConfig) ClientTLS(serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	caBytes, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caBytes)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
		ServerName:   serverName, // 必须匹配 server cert 的 CN/SAN
	}, nil
}
