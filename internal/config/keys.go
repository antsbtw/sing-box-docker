package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	"golang.org/x/crypto/curve25519"
)

// NodeSecrets 存储节点密钥和端口配置
type NodeSecrets struct {
	PrivateKey string   `json:"private_key"`
	PublicKey  string   `json:"public_key"`
	ShortIDs   []string `json:"short_ids"`
	SSPort     int      `json:"ss_port"` // 随机生成的 SS 端口
}

// GenerateKeyPair 生成新的 Reality 密钥对
func GenerateKeyPair() (*NodeSecrets, error) {
	// 生成私钥
	var privateKey [32]byte
	if _, err := rand.Read(privateKey[:]); err != nil {
		return nil, fmt.Errorf("generate private key: %w", err)
	}

	// 计算公钥
	var publicKey [32]byte
	curve25519.ScalarBaseMult(&publicKey, &privateKey)

	// 生成 short_id
	shortID := make([]byte, 8)
	if _, err := rand.Read(shortID); err != nil {
		return nil, fmt.Errorf("generate short_id: %w", err)
	}

	// 生成随机 SS 端口 (10000-60000)
	ssPort, err := randomPort(10000, 60000)
	if err != nil {
		return nil, fmt.Errorf("generate ss port: %w", err)
	}

	return &NodeSecrets{
		PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey[:]),
		PublicKey:  base64.RawURLEncoding.EncodeToString(publicKey[:]),
		ShortIDs:   []string{hex.EncodeToString(shortID)},
		SSPort:     ssPort,
	}, nil
}

// randomPort 生成指定范围内的随机端口
func randomPort(min, max int) (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(max-min)))
	if err != nil {
		return 0, err
	}
	return int(n.Int64()) + min, nil
}

// LoadOrGenerateSecrets 加载或生成节点密钥
func LoadOrGenerateSecrets(cacheDir string) (*NodeSecrets, error) {
	path := filepath.Join(cacheDir, "secrets.json")

	// 尝试加载现有密钥
	if data, err := os.ReadFile(path); err == nil {
		var secrets NodeSecrets
		if err := json.Unmarshal(data, &secrets); err == nil {
			// 兼容旧版本：如果没有 SS 端口，生成一个
			if secrets.SSPort == 0 {
				secrets.SSPort, _ = randomPort(10000, 60000)
				// 保存更新
				data, _ := json.MarshalIndent(secrets, "", "  ")
				os.WriteFile(path, data, 0600)
			}
			return &secrets, nil
		}
	}

	// 生成新密钥
	secrets, err := GenerateKeyPair()
	if err != nil {
		return nil, err
	}

	// 保存到文件
	data, _ := json.MarshalIndent(secrets, "", "  ")
	os.MkdirAll(cacheDir, 0755)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, fmt.Errorf("save secrets: %w", err)
	}

	return secrets, nil
}

// 兼容旧接口
type KeyPair = NodeSecrets

func LoadOrGenerateKeys(cacheDir string) (*KeyPair, error) {
	return LoadOrGenerateSecrets(cacheDir)
}
