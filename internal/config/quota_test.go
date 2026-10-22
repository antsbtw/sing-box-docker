package config

import (
	"testing"
	"time"
)

// 六个协议的用户都带 name = uuid：sing-box 按 name 记用户流量，缺了就不计（此前 VLESS 恒为 0）
func TestEveryProtocolUserCarriesStatsName(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"}).
		WithExtraProtocols(2082, 2083, 2084, 2085, "/data/self.crt", "/data/self.key")
	u := sixProtoUser()
	ins := inboundTypes(g.Generate([]User{u}, "www.apple.com"))

	for _, typ := range []string{"vless", "shadowsocks", "vmess", "trojan", "hysteria2", "tuic"} {
		users, _ := ins[typ]["users"].([]map[string]any)
		if len(users) != 1 {
			t.Fatalf("%s users = %v", typ, ins[typ]["users"])
		}
		if users[0]["name"] != u.UUID {
			t.Errorf("%s user name = %v, want %s", typ, users[0]["name"], u.UUID)
		}
	}
}

// 超额、过期的用户不进配置；调高限额后再生成就回来
func TestOverQuotaAndExpiredUsersAreLeftOut(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"})
	past := time.Now().Add(-time.Hour)

	over := sixProtoUser()
	over.TrafficLimit, over.TrafficUsed = 1000, 1000
	expired := sixProtoUser()
	expired.UUID = "expired-user"
	expired.ExpireAt = &past

	ins := inboundTypes(g.Generate([]User{over, expired}, "www.apple.com"))
	if users, _ := ins["vless"]["users"].([]map[string]any); len(users) != 0 {
		t.Fatalf("vless users = %v, want none", users)
	}

	over.TrafficLimit = 2000
	ins = inboundTypes(g.Generate([]User{over}, "www.apple.com"))
	if users, _ := ins["vless"]["users"].([]map[string]any); len(users) != 1 {
		t.Fatalf("raised limit: vless users = %v, want 1", users)
	}
}

// TUIC 服务端必须声明 alpn h3，否则带 alpn=h3 的客户端握手失败
func TestTUICInboundDeclaresH3(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"}).
		WithExtraProtocols(2082, 2083, 2084, 2085, "/data/self.crt", "/data/self.key")
	ins := inboundTypes(g.Generate([]User{sixProtoUser()}, "www.apple.com"))
	alpn, _ := ins["tuic"]["tls"].(map[string]any)["alpn"].([]string)
	if len(alpn) != 1 || alpn[0] != "h3" {
		t.Fatalf("tuic alpn = %v, want [h3]", alpn)
	}
}
