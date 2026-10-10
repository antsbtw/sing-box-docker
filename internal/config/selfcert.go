package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// SelfCertCN 是自签证书与各 TLS inbound/分享链接共用的 SNI。
//
// Trojan / Hysteria2 / TUIC 用自签证书（方案 B：客户端 insecure），
// 证书的 CN/SAN、inbound 的 server_name、分享链接的 sni 必须是同一个值，
// 否则部分客户端即便开了 insecure 仍会因 SNI 不匹配而失败。
const SelfCertCN = "www.bing.com"

// SelfCertPaths 自签证书的落盘路径。
type SelfCertPaths struct {
	CertPath string
	KeyPath  string
}

// EnsureSelfSignedCert 确保自签证书存在并返回路径。
//
// 设计要点：
//   - 证书「首次随机生成一次、落盘、之后复用」。同一台机器重启、重新生成
//     sing-box 配置都复用同一张证书，指纹不变，已缓存证书指纹的客户端不受影响。
//   - 每台 VPS 各自一张证书（互不相同），这正是期望：节点之间本就该独立。
//   - privateKey 入参保留仅为接口稳定（当前实现不再从它派生，Go 标准库刻意
//     阻止确定性密钥生成；落盘复用已满足「同机恒定」这一真正的需求）。
func EnsureSelfSignedCert(dir, privateKey string) (*SelfCertPaths, error) {
	_ = privateKey // 见上：保留入参，实现不再依赖它

	paths := &SelfCertPaths{
		CertPath: filepath.Join(dir, "self.crt"),
		KeyPath:  filepath.Join(dir, "self.key"),
	}

	// 已存在则复用 —— 这是「同机重启指纹不变」的关键。
	if fileExists(paths.CertPath) && fileExists(paths.KeyPath) {
		return paths, nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir cert dir: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ec key: %w", err)
	}

	serialMax := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialMax)
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: SelfCertCN},
		DNSNames:     []string{SelfCertCN},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create certificate: %w", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ec key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.WriteFile(paths.CertPath, certPEM, 0644); err != nil {
		return nil, fmt.Errorf("write cert: %w", err)
	}
	if err := os.WriteFile(paths.KeyPath, keyPEM, 0600); err != nil {
		return nil, fmt.Errorf("write key: %w", err)
	}

	return paths, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
