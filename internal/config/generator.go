package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

// Generator 生成 sing-box 配置
type Generator struct {
	vlessPort  int
	ssPort     int
	privateKey string
	shortIDs   []string
}

// NewGenerator 创建配置生成器
func NewGenerator(vlessPort, ssPort int, privateKey string, shortIDs []string) *Generator {
	return &Generator{
		vlessPort:  vlessPort,
		ssPort:     ssPort,
		privateKey: privateKey,
		shortIDs:   shortIDs,
	}
}

// ssMasterPassword 生成 shadowsocks inbound 的 master key。
//
// chacha20-ietf-poly1305 要求 32 字节密钥的 base64。
// 从节点私钥派生而非随机生成：必须在重启、重新生成配置后保持不变，
// 否则已分发出去的 SS 链接会全部失效。
func (g *Generator) ssMasterPassword() string {
	sum := sha256.Sum256([]byte("otun-ss-master|" + g.privateKey))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Generate 根据用户列表生成 sing-box 配置
func (g *Generator) Generate(users []User, realitySNI string) map[string]any {
	var vlessUsers []map[string]any
	var ssUsers []map[string]any
	var statsUsers []string

	for _, u := range users {
		if !u.Enabled {
			continue
		}

		// 关键修复：无论用户使用哪些协议，都加入统计列表
		// 这样可以确保 sing-box V2Ray API 统计该用户的所有流量（VLESS + Shadowsocks）
		// 从而实现跨协议的统一流量限制
		statsUsers = append(statsUsers, u.UUID)

		for _, proto := range u.Protocols {
			switch proto {
			case "vless":
				vlessUsers = append(vlessUsers, map[string]any{
					"uuid": u.UUID,
					"flow": "xtls-rprx-vision",
				})
			case "shadowsocks":
				ssUsers = append(ssUsers, map[string]any{
					"name":     u.UUID,
					"password": u.SSPassword,
				})
			}
		}
	}

	config := map[string]any{
		"log": map[string]any{
			"level":     "info",
			"timestamp": true,
		},
		"outbounds": []map[string]any{
			{"type": "direct", "tag": "direct"},
		},
	}

	var inbounds []map[string]any

	// VLESS + Reality inbound - 始终创建，即使没有用户
	// 这样 sing-box 可以启动并监听端口，等待用户添加
	vlessInbound := map[string]any{
		"type":        "vless",
		"tag":         "vless-in",
		"listen":      "::",
		"listen_port": g.vlessPort,
		"tls": map[string]any{
			"enabled":     true,
			"server_name": realitySNI,
			"reality": map[string]any{
				"enabled": true,
				"handshake": map[string]any{
					"server":      realitySNI,
					"server_port": 443,
				},
				"private_key": g.privateKey,
				"short_id":    g.shortIDs,
			},
		},
	}
	if len(vlessUsers) > 0 {
		vlessInbound["users"] = vlessUsers
	} else {
		// 空用户列表，sing-box 需要这个字段
		vlessInbound["users"] = []map[string]any{}
	}
	inbounds = append(inbounds, vlessInbound)

	// Shadowsocks inbound - 始终创建
	//
	// ⚠️ inbound 级 password 是**必填**的：sing-box 的 shadowsocks
	// 多用户模式用它作 master key 参与密钥派生，缺了会直接
	//     FATAL create service: parse inbound[1]: missing password
	// 而无法启动。
	//
	// 此前没设这个字段。存量 SSH 路径不暴露，是因为 App 装完会立刻建用户；
	// Token 路径是"先装好、等后端下发用户"，零用户的空窗期就崩了。
	//
	// 从节点私钥派生：同一台机器恒定（重启后 SS 链接不失效），
	// 且不泄露私钥本身。
	ssInbound := map[string]any{
		"type":        "shadowsocks",
		"tag":         "ss-in",
		"listen":      "::",
		"listen_port": g.ssPort,
		"method":      "chacha20-ietf-poly1305",
		"password":    g.ssMasterPassword(),
	}
	if len(ssUsers) > 0 {
		ssInbound["users"] = ssUsers
	} else {
		ssInbound["users"] = []map[string]any{}
	}
	inbounds = append(inbounds, ssInbound)

	config["inbounds"] = inbounds

	// V2Ray API for stats - 始终启用
	config["experimental"] = map[string]any{
		"v2ray_api": map[string]any{
			"listen": "127.0.0.1:10085",
			"stats": map[string]any{
				"enabled": true,
				"users":   statsUsers,
			},
		},
	}

	return config
}

// WriteToFile 将配置写入文件
func (g *Generator) WriteToFile(config map[string]any, path string) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}
