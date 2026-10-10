package config

import (
	"os"
	"path/filepath"
	"testing"
)

// 六协议用户。
func sixProtoUser() User {
	return User{
		UUID:       "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
		Protocols:  []string{"vless", "shadowsocks", "vmess", "trojan", "hysteria2", "tuic"},
		SSPassword: "ss-pass",
		Enabled:    true,
	}
}

// inboundTypes 提取生成配置里全部 inbound 的 type。
func inboundTypes(cfg map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, raw := range cfg["inbounds"].([]map[string]any) {
		out[raw["type"].(string)] = raw
	}
	return out
}

func TestGenerateAllSixProtocols(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"}).
		WithExtraProtocols(2082, 2083, 2084, 2085, "/data/self.crt", "/data/self.key")

	cfg := g.Generate([]User{sixProtoUser()}, "www.apple.com")
	ins := inboundTypes(cfg)

	for _, typ := range []string{"vless", "shadowsocks", "vmess", "trojan", "hysteria2", "tuic"} {
		if _, ok := ins[typ]; !ok {
			t.Fatalf("missing inbound type %q", typ)
		}
	}

	// 端口正确。
	if ins["vmess"]["listen_port"].(int) != 2082 {
		t.Errorf("vmess port = %v, want 2082", ins["vmess"]["listen_port"])
	}
	if ins["trojan"]["listen_port"].(int) != 2083 {
		t.Errorf("trojan port = %v, want 2083", ins["trojan"]["listen_port"])
	}

	// TLS 协议引用了自签证书路径。
	for _, typ := range []string{"trojan", "hysteria2", "tuic"} {
		tls := ins[typ]["tls"].(map[string]any)
		if tls["certificate_path"] != "/data/self.crt" {
			t.Errorf("%s cert path = %v", typ, tls["certificate_path"])
		}
		if tls["server_name"] != SelfCertCN {
			t.Errorf("%s server_name = %v, want %s", typ, tls["server_name"], SelfCertCN)
		}
	}

	// 全部用户进统计。
	v2 := cfg["experimental"].(map[string]any)["v2ray_api"].(map[string]any)
	stats := v2["stats"].(map[string]any)["users"].([]string)
	if len(stats) != 1 || stats[0] != sixProtoUser().UUID {
		t.Errorf("stats users = %v", stats)
	}
}

// 无证书时，三个 TLS 协议应被跳过，VLESS/SS/VMess 仍在。
func TestGenerateSkipsTLSWithoutCert(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"}).
		WithExtraProtocols(2082, 2083, 2084, 2085, "", "")
	cfg := g.Generate([]User{sixProtoUser()}, "www.apple.com")
	ins := inboundTypes(cfg)

	for _, typ := range []string{"vless", "shadowsocks", "vmess"} {
		if _, ok := ins[typ]; !ok {
			t.Errorf("expected %q present without cert", typ)
		}
	}
	for _, typ := range []string{"trojan", "hysteria2", "tuic"} {
		if _, ok := ins[typ]; ok {
			t.Errorf("expected %q skipped without cert", typ)
		}
	}
}

// 不配新端口时行为与旧版一致：只有 vless + shadowsocks。
func TestGenerateBackwardCompatible(t *testing.T) {
	g := NewGenerator(443, 8388, "priv-key", []string{"abcd"})
	cfg := g.Generate([]User{sixProtoUser()}, "www.apple.com")
	ins := inboundTypes(cfg)
	if len(ins) != 2 {
		t.Fatalf("expected 2 inbounds (vless+ss), got %d: %v", len(ins), keys(ins))
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 凭据派生是确定性的。
func TestCredentialsDeterministic(t *testing.T) {
	uuid := "a1b2c3d4-e5f6-7890-abcd-ef1234567890"
	if TrojanPassword(uuid) != TrojanPassword(uuid) {
		t.Error("trojan password not deterministic")
	}
	if TUICUUID(uuid) != TUICUUID(uuid) {
		t.Error("tuic uuid not deterministic")
	}
	// TUIC UUID 形如标准 UUID（36 字符，4 个连字符）。
	id := TUICUUID(uuid)
	if len(id) != 36 {
		t.Errorf("tuic uuid length = %d, want 36: %q", len(id), id)
	}
	// 不同协议的密码互不相同。
	if TrojanPassword(uuid) == Hysteria2Password(uuid) {
		t.Error("trojan and hy2 passwords collide")
	}
}

// 自签证书：生成成功、同机落盘复用（重启后指纹不变）。
func TestSelfSignedCert(t *testing.T) {
	dir := t.TempDir()
	p, err := EnsureSelfSignedCert(dir, "my-priv-key")
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(dir, "self.crt")) || !fileExists(filepath.Join(dir, "self.key")) {
		t.Fatal("cert/key file missing")
	}
	crt1, _ := os.ReadFile(p.CertPath)

	// 复用：第二次调用必须沿用同一张证书（字节与 mtime 都不变），
	// 这是「同机重启指纹不变、已分发链接不失效」的保证。
	info1, _ := os.Stat(p.CertPath)
	_, err = EnsureSelfSignedCert(dir, "my-priv-key")
	if err != nil {
		t.Fatal(err)
	}
	info2, _ := os.Stat(p.CertPath)
	crt2, _ := os.ReadFile(p.CertPath)
	if info1.ModTime() != info2.ModTime() || string(crt1) != string(crt2) {
		t.Error("existing cert should be reused, not regenerated")
	}
}
