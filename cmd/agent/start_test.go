package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归 I3：装上 sing-box 之后，即使 unit 里仍是 SKIP_SINGBOX=true，启动时也要拉起它。
func TestSingboxInstalledIgnoresStaleSkipFlag(t *testing.T) {
	t.Setenv("SKIP_SINGBOX", "true")
	dir := t.TempDir()
	bin := filepath.Join(dir, "sing-box")
	cfg := filepath.Join(dir, "config.json")

	if singboxInstalled(bin, cfg) {
		t.Fatal("没有二进制与配置时不应启动")
	}
	os.WriteFile(bin, []byte("x"), 0o755)
	if singboxInstalled(bin, cfg) {
		t.Fatal("只有二进制、没有配置时不应启动")
	}
	os.WriteFile(cfg, []byte("{}"), 0o644)
	if !singboxInstalled(bin, cfg) {
		t.Fatal("二进制与配置都在时应启动，不管 SKIP_SINGBOX")
	}
}
