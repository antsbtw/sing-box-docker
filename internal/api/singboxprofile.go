package api

import (
	"encoding/json"

	"otun-node-agent/internal/config"
	"otun-node-agent/internal/local"
)

// buildSingBoxProfile 为某用户生成一份 sing-box **客户端** JSON 配置。
//
// 为什么需要它：sing-box 的 import-remote-profile 期望订阅网址返回 sing-box
// 自己的 JSON（outbounds 等），而不是通用的 base64 节点链接。所以 sing-box
// 客户端拉订阅时（User-Agent 含 sing-box），返回这份 JSON；其他客户端
// （Shadowrocket/Clash/v2rayN）仍返回 base64 节点链接。
//
// 配置含：tun 入站 + 每个已启用协议一个出站 + 一个 selector（手动选节点）
// + urltest（自动测速）+ direct/block + 中国分流路由。
func (s *LocalAPIServer) buildSingBoxProfile(u *local.LocalUser) ([]byte, error) {
	nc := s.nodeConfig
	var outbounds []map[string]any
	var nodeTags []string

	add := func(o map[string]any) {
		outbounds = append(outbounds, o)
		nodeTags = append(nodeTags, o["tag"].(string))
	}

	sni := nc.RealitySNI
	if sni == "" {
		sni = "www.apple.com"
	}

	for _, proto := range u.Protocols {
		switch proto {
		case "vless":
			add(map[string]any{
				"type": "vless", "tag": "VLESS-Reality",
				"server": nc.ServerIP, "server_port": nc.VLESSPort,
				"uuid": u.UUID, "flow": "xtls-rprx-vision",
				"tls": map[string]any{
					"enabled": true, "server_name": sni,
					"utls":    map[string]any{"enabled": true, "fingerprint": "chrome"},
					"reality": map[string]any{"enabled": true, "public_key": nc.PublicKey, "short_id": nc.ShortID},
				},
			})
		case "shadowsocks":
			method := nc.SSMethod
			if method == "" {
				method = "chacha20-ietf-poly1305"
			}
			add(map[string]any{
				"type": "shadowsocks", "tag": "Shadowsocks",
				"server": nc.ServerIP, "server_port": nc.SSPort,
				"method": method, "password": u.SSPassword,
			})
		case "vmess":
			if nc.VMessPort == 0 {
				continue
			}
			add(map[string]any{
				"type": "vmess", "tag": "VMess",
				"server": nc.ServerIP, "server_port": nc.VMessPort,
				"uuid": config.VMessUUID(u.UUID), "security": "auto", "alter_id": 0,
				"transport": map[string]any{"type": "ws", "path": "/vmess"},
			})
		case "trojan":
			if nc.TrojanPort == 0 {
				continue
			}
			add(map[string]any{
				"type": "trojan", "tag": "Trojan",
				"server": nc.ServerIP, "server_port": nc.TrojanPort,
				"password": config.TrojanPassword(u.UUID),
				"tls":      map[string]any{"enabled": true, "server_name": config.SelfCertCN, "insecure": true},
			})
		case "hysteria2":
			if nc.Hysteria2Port == 0 {
				continue
			}
			add(map[string]any{
				"type": "hysteria2", "tag": "Hysteria2",
				"server": nc.ServerIP, "server_port": nc.Hysteria2Port,
				"password": config.Hysteria2Password(u.UUID),
				"tls":      map[string]any{"enabled": true, "server_name": config.SelfCertCN, "insecure": true, "alpn": []string{"h3"}},
			})
		case "tuic":
			if nc.TUICPort == 0 {
				continue
			}
			add(map[string]any{
				"type": "tuic", "tag": "TUIC",
				"server": nc.ServerIP, "server_port": nc.TUICPort,
				"uuid": config.TUICUUID(u.UUID), "password": config.TUICPassword(u.UUID),
				"congestion_control": "bbr",
				"tls":                map[string]any{"enabled": true, "server_name": config.SelfCertCN, "insecure": true, "alpn": []string{"h3"}},
			})
		}
	}

	// selector（手动）+ urltest（自动）放在节点之前。
	selectorTags := append([]string{"自动选择"}, nodeTags...)
	proxy := map[string]any{
		"type": "selector", "tag": "节点选择",
		"outbounds": selectorTags, "default": "自动选择",
	}
	urltest := map[string]any{
		"type": "urltest", "tag": "自动选择",
		"outbounds": nodeTags, "url": "https://www.gstatic.com/generate_204", "interval": "3m",
	}

	// 1.14 新格式：不再有 dns-out / block 这类 legacy special outbound，
	// DNS 劫持与拒绝改用 route rule 的 action。
	all := []map[string]any{proxy, urltest}
	all = append(all, outbounds...)
	all = append(all, map[string]any{"type": "direct", "tag": "direct"})

	profile := map[string]any{
		"log": map[string]any{"level": "info"},
		// 1.14 新 DNS server 格式：用 type 字段，不再用 address。
		"dns": map[string]any{
			"servers": []map[string]any{
				{"tag": "google", "type": "tls", "server": "8.8.8.8", "detour": "节点选择"},
				{"tag": "local", "type": "udp", "server": "223.5.5.5", "detour": "direct"},
			},
			"rules": []map[string]any{
				{"rule_set": "geosite-cn", "server": "local"},
			},
			"final": "google",
		},
		"inbounds": []map[string]any{
			{"type": "tun", "address": []string{"172.19.0.1/30"}, "auto_route": true, "strict_route": true, "stack": "gvisor"},
		},
		"outbounds": all,
		"route": map[string]any{
			"default_domain_resolver": "local",
			"rule_set": []map[string]any{
				{"type": "remote", "tag": "geosite-cn", "format": "binary", "url": "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs", "download_detour": "节点选择"},
				{"type": "remote", "tag": "geoip-cn", "format": "binary", "url": "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs", "download_detour": "节点选择"},
			},
			// 1.14 用 action：sniff 探测、hijack-dns 劫持 DNS、中国直连。
			"rules": []map[string]any{
				{"action": "sniff"},
				{"protocol": "dns", "action": "hijack-dns"},
				{"rule_set": []string{"geoip-cn", "geosite-cn"}, "outbound": "direct"},
				{"ip_is_private": true, "outbound": "direct"},
			},
			"final": "节点选择",
		},
	}

	return json.MarshalIndent(profile, "", "  ")
}

// BuildSingBoxProfileForTest 是 buildSingBoxProfile 的导出包装，仅供离线校验/测试用。
func (s *LocalAPIServer) BuildSingBoxProfileForTest(u *local.LocalUser) ([]byte, error) {
	return s.buildSingBoxProfile(u)
}
