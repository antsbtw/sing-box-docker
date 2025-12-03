package config

import (
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

// Generate 根据用户列表生成 sing-box 配置
func (g *Generator) Generate(users []User, realitySNI string) map[string]any {
	// 筛选启用的用户
	var vlessUsers []map[string]any
	var ssUsers []map[string]any
	var statsUsers []string

	for _, u := range users {
		if !u.Enabled {
			continue
		}

		for _, proto := range u.Protocols {
			switch proto {
			case "vless":
				vlessUsers = append(vlessUsers, map[string]any{
					"uuid": u.UUID,
					"flow": "xtls-rprx-vision",
				})
				statsUsers = append(statsUsers, u.UUID)
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

	// VLESS + Reality inbound
	if len(vlessUsers) > 0 {
		inbounds = append(inbounds, map[string]any{
			"type":        "vless",
			"tag":         "vless-in",
			"listen":      "::",
			"listen_port": g.vlessPort,
			"users":       vlessUsers,
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
		})
	}

	// Shadowsocks inbound
	if len(ssUsers) > 0 {
		inbounds = append(inbounds, map[string]any{
			"type":        "shadowsocks",
			"tag":         "ss-in",
			"listen":      "::",
			"listen_port": g.ssPort,
			"method":      "chacha20-ietf-poly1305",
			"users":       ssUsers,
		})
	}

	config["inbounds"] = inbounds

	// V2Ray API for stats
	if len(statsUsers) > 0 {
		config["experimental"] = map[string]any{
			"v2ray_api": map[string]any{
				"listen": "127.0.0.1:10085",
				"stats": map[string]any{
					"enabled": true,
					"users":   statsUsers,
				},
			},
		}
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
