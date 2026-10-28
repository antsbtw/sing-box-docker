package api

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"otun-node-agent/internal/local"
)

func otunTestServer(v6 string) *LocalAPIServer {
	return &LocalAPIServer{nodeConfig: &NodeConfig{
		ServerIP: "52.195.209.194", ServerIPv6: v6, PublicKey: "pk", ShortID: "sid",
		VLESSPort: 443, SSPort: 28607, SSMethod: "chacha20-ietf-poly1305", RealitySNI: "www.apple.com",
		VMessPort: 2082, TrojanPort: 2083, Hysteria2Port: 2084, TUICPort: 2085,
	}}
}

func otunTestUser() *local.LocalUser {
	return &local.LocalUser{
		UUID: "57d90faa-5b03-4371-9d48-5107c96f5e59", Name: "r3-test", SSPassword: "pw",
		Protocols: []string{"vless", "shadowsocks", "vmess", "trojan", "hysteria2", "tuic"},
	}
}

// 有 IPv6：只有 TCP 三协议带 url_v6，且除地址外与 url 一致（契约 C2、C3）
func TestOTunSubscriptionAddsV6ForTCPOnly(t *testing.T) {
	sub := otunTestServer("2406:da14:1eb6:b300::1").buildOTunSubscription(otunTestUser())
	if sub.Version != 1 || len(sub.Protocols) != 6 {
		t.Fatalf("got %+v", sub)
	}
	for _, p := range sub.Protocols {
		if p.Node != "primary" {
			t.Errorf("%s node = %q", p.Protocol, p.Node)
		}
		if p.Protocol == "vmess" {
			if vmessAdd(t, p.URL) != "52.195.209.194" || vmessAdd(t, p.URLV6) != "2406:da14:1eb6:b300::1" {
				t.Errorf("vmess add: url=%s url_v6=%s", vmessAdd(t, p.URL), vmessAdd(t, p.URLV6))
			}
			continue
		}
		if !strings.Contains(p.URL, "52.195.209.194") {
			t.Errorf("%s url has no v4 address: %s", p.Protocol, p.URL)
		}
		wantV6 := p.Protocol == "vless" || p.Protocol == "vmess" || p.Protocol == "trojan"
		if wantV6 != (p.URLV6 != "") {
			t.Errorf("%s url_v6 = %q, want present=%v", p.Protocol, p.URLV6, wantV6)
		}
		if p.Protocol == "vless" || p.Protocol == "trojan" {
			if !strings.Contains(p.URLV6, "@[2406:da14:1eb6:b300::1]:") {
				t.Errorf("%s url_v6 address not bracketed: %s", p.Protocol, p.URLV6)
			}
			if strings.Replace(p.URLV6, "[2406:da14:1eb6:b300::1]", "52.195.209.194", 1) != p.URL {
				t.Errorf("%s url_v6 differs from url beyond the address", p.Protocol)
			}
		}
	}
}

// 没有 IPv6：条目照常，一律不带 url_v6（契约 C4）
func TestOTunSubscriptionWithoutV6(t *testing.T) {
	sub := otunTestServer("").buildOTunSubscription(otunTestUser())
	if len(sub.Protocols) != 6 {
		t.Fatalf("got %d protocols", len(sub.Protocols))
	}
	for _, p := range sub.Protocols {
		if p.URLV6 != "" {
			t.Errorf("%s has url_v6 without IPv6: %s", p.Protocol, p.URLV6)
		}
	}
}

// vmess 链接是 base64 JSON，取其中的 add（连接地址）
func vmessAdd(t *testing.T, link string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link, "vmess://"))
	if err != nil {
		t.Fatalf("vmess base64: %v", err)
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("vmess json: %v", err)
	}
	add, _ := v["add"].(string)
	return add
}

// 通用订阅（base64）：有 IPv6 时每个协议多一条 IPv6 节点，名字带协议和「IPv6」，排在 v4 之后
func TestBase64SubscriptionListsV4ThenV6(t *testing.T) {
	resp := otunTestServer("2406:da14:1eb6:b300::1").toUserResponse(otunTestUser())
	raw, err := base64.StdEncoding.DecodeString(resp.SubscriptionUrl)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 12 {
		t.Fatalf("got %d links, want 12", len(lines))
	}
	if !strings.HasSuffix(lines[0], "#"+url.PathEscape("r3-test · VLESS")) {
		t.Errorf("v4 vless name: %s", lines[0])
	}
	if !strings.Contains(lines[6], "@[2406:da14:1eb6:b300::1]:443") ||
		!strings.HasSuffix(lines[6], "#"+url.PathEscape("r3-test · VLESS · IPv6")) {
		t.Errorf("v6 vless: %s", lines[6])
	}
	if resp.VLESSUrlV6 == "" || resp.TUICUrlV6 == "" {
		t.Errorf("v6 fields missing: %+v", resp)
	}
	// 没有 IPv6：只有 6 条
	resp = otunTestServer("").toUserResponse(otunTestUser())
	raw, _ = base64.StdEncoding.DecodeString(resp.SubscriptionUrl)
	if n := len(strings.Split(string(raw), "\n")); n != 6 || resp.VLESSUrlV6 != "" {
		t.Errorf("without v6: %d links, v6 field %q", n, resp.VLESSUrlV6)
	}
}

// sing-box 客户端配置：有 IPv6 时多出「· IPv6」出站，并进自动选择
func TestSingBoxProfileHasV6Outbounds(t *testing.T) {
	data, err := otunTestServer("2406:da14:1eb6:b300::1").buildSingBoxProfile(otunTestUser())
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, tag := range []string{"VLESS-Reality · IPv6", "TUIC · IPv6", "2406:da14:1eb6:b300::1"} {
		if !strings.Contains(s, tag) {
			t.Errorf("profile missing %q", tag)
		}
	}
}

// vmess 链接不能带空 host：OTun-M 等会照抄成空 Host 头，WS 握手出两条 Host，VMess 全部不通。
func TestVMessLinkHasNoEmptyHost(t *testing.T) {
	sub := otunTestServer("2406:da14:1eb6:b300::1").buildOTunSubscription(otunTestUser())
	for _, p := range sub.Protocols {
		if p.Protocol != "vmess" {
			continue
		}
		for _, link := range []string{p.URL, p.URLV6} {
			raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(link, "vmess://"))
			if err != nil {
				t.Fatalf("vmess base64: %v", err)
			}
			var v map[string]any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("vmess json: %v", err)
			}
			if h, ok := v["host"]; ok {
				t.Errorf("vmess link has host=%q, want no host key", h)
			}
		}
	}
}
