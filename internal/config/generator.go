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
	vmessPort  int
	trojanPort int
	hy2Port    int
	tuicPort   int
	privateKey string
	shortIDs   []string
	certPath   string // 自签证书（Trojan/Hy2/TUIC 用）
	keyPath    string
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

// WithExtraProtocols 配置 VMess/Trojan/Hysteria2/TUIC 的端口与自签证书。
// 返回 g 以便链式调用。任一端口为 0 时该协议不生成 inbound。
func (g *Generator) WithExtraProtocols(vmessPort, trojanPort, hy2Port, tuicPort int, certPath, keyPath string) *Generator {
	g.vmessPort = vmessPort
	g.trojanPort = trojanPort
	g.hy2Port = hy2Port
	g.tuicPort = tuicPort
	g.certPath = certPath
	g.keyPath = keyPath
	return g
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

// orEmpty 把 nil 切片转成空 JSON 数组（sing-box 需要 users 字段存在）。
func orEmpty(u []map[string]any) []map[string]any {
	if u == nil {
		return []map[string]any{}
	}
	return u
}

// Generate 根据用户列表生成 sing-box 配置
func (g *Generator) Generate(users []User, realitySNI string) map[string]any {
	var vlessUsers []map[string]any
	var ssUsers []map[string]any
	var vmessUsers []map[string]any
	var trojanUsers []map[string]any
	var hy2Users []map[string]any
	var tuicUsers []map[string]any
	var statsUsers []string

	for _, u := range users {
		if !u.Enabled {
			continue
		}

		// 关键修复：无论用户使用哪些协议，都加入统计列表
		// 这样可以确保 sing-box V2Ray API 统计该用户的所有流量（全部协议）
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
			case "vmess":
				vmessUsers = append(vmessUsers, map[string]any{
					"name":    u.UUID,
					"uuid":    VMessUUID(u.UUID),
					"alterId": 0,
				})
			case "trojan":
				trojanUsers = append(trojanUsers, map[string]any{
					"name":     u.UUID,
					"password": TrojanPassword(u.UUID),
				})
			case "hysteria2":
				hy2Users = append(hy2Users, map[string]any{
					"name":     u.UUID,
					"password": Hysteria2Password(u.UUID),
				})
			case "tuic":
				tuicUsers = append(tuicUsers, map[string]any{
					"name":     u.UUID,
					"uuid":     TUICUUID(u.UUID),
					"password": TUICPassword(u.UUID),
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

	// selfTLS 生成自签 TLS 块（Trojan/Hy2/TUIC 共用）。
	selfTLS := func() map[string]any {
		return map[string]any{
			"enabled":          true,
			"server_name":      SelfCertCN,
			"certificate_path": g.certPath,
			"key_path":         g.keyPath,
		}
	}

	// VMess inbound（WS，无 TLS）—— 仅在配置了端口时创建。
	if g.vmessPort > 0 {
		vmessInbound := map[string]any{
			"type":        "vmess",
			"tag":         "vmess-in",
			"listen":      "::",
			"listen_port": g.vmessPort,
			"transport": map[string]any{
				"type": "ws",
				"path": "/vmess",
			},
			"users": orEmpty(vmessUsers),
		}
		inbounds = append(inbounds, vmessInbound)
	}

	// 以下三种需要自签证书，证书路径为空时跳过（避免 sing-box 启动失败）。
	hasCert := g.certPath != "" && g.keyPath != ""

	// Trojan inbound（TCP + 自签 TLS）。
	if g.trojanPort > 0 && hasCert {
		inbounds = append(inbounds, map[string]any{
			"type":        "trojan",
			"tag":         "trojan-in",
			"listen":      "::",
			"listen_port": g.trojanPort,
			"tls":         selfTLS(),
			"users":       orEmpty(trojanUsers),
		})
	}

	// Hysteria2 inbound（UDP/QUIC + 自签 TLS）。
	if g.hy2Port > 0 && hasCert {
		inbounds = append(inbounds, map[string]any{
			"type":        "hysteria2",
			"tag":         "hy2-in",
			"listen":      "::",
			"listen_port": g.hy2Port,
			"tls":         selfTLS(),
			"users":       orEmpty(hy2Users),
		})
	}

	// TUIC inbound（UDP/QUIC + 自签 TLS）。
	if g.tuicPort > 0 && hasCert {
		inbounds = append(inbounds, map[string]any{
			"type":                "tuic",
			"tag":                 "tuic-in",
			"listen":              "::",
			"listen_port":         g.tuicPort,
			"congestion_control":  "bbr",
			"tls":                 selfTLS(),
			"users":               orEmpty(tuicUsers),
		})
	}

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
