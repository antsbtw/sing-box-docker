package config

import (
	"encoding/json"
	"testing"
)

// 回归：零用户时 sing-box 必须能启动。
// 此前 SS inbound 缺 inbound 级 password，直接
//
//	FATAL create service: parse inbound[1]: missing password
//
// Token 路径"先装好、等后端下发用户"，零用户空窗期必然撞上。
func TestShadowsocksInboundHasMasterPassword(t *testing.T) {
	g := NewGenerator(443, 8388, "test-private-key", []string{"0123456789abcdef"})
	cfg := g.Generate(nil, "www.microsoft.com")

	inbounds := cfg["inbounds"].([]map[string]any)
	var ss map[string]any
	for _, in := range inbounds {
		if in["type"] == "shadowsocks" {
			ss = in
		}
	}
	if ss == nil {
		t.Fatal("未生成 shadowsocks inbound")
	}

	pw, ok := ss["password"].(string)
	if !ok || pw == "" {
		b, _ := json.MarshalIndent(ss, "", "  ")
		t.Fatalf("SS inbound 缺 password，sing-box 无法启动:\n%s", b)
	}
	t.Logf("master password = %s", pw)

	// 必须稳定：变了会让已分发的 SS 链接全部失效
	g2 := NewGenerator(443, 8388, "test-private-key", []string{"0123456789abcdef"})
	cfg2 := g2.Generate(nil, "www.microsoft.com")
	for _, in := range cfg2["inbounds"].([]map[string]any) {
		if in["type"] == "shadowsocks" && in["password"] != pw {
			t.Error("同一私钥应派生出相同的 master password")
		}
	}

	// 不同私钥必须不同
	g3 := NewGenerator(443, 8388, "another-key", []string{"0123456789abcdef"})
	for _, in := range g3.Generate(nil, "www.microsoft.com")["inbounds"].([]map[string]any) {
		if in["type"] == "shadowsocks" && in["password"] == pw {
			t.Error("不同私钥不应派生出相同的 master password")
		}
	}
}
