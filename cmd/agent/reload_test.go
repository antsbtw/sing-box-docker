package main

import (
	"os"
	"path/filepath"
	"testing"

	"otun-node-agent/internal/singbox"
)

// 装完 sing-box 之后，reload 要能把它拉起来。
//
// 背景：v1.12.3 起 token 接入默认不装 sing-box，unit 里带
// SKIP_SINGBOX=true，agent 启动时跳过。用户之后从 App 经隧道把
// sing-box 装上，这时 agent 仍以为不该管它 —— 装了也不会跑。
// 而重启 agent 会掐断 App 正连着的隧道，所以只能靠 reload。
func TestManagerStartFailsClearlyWhenBinaryMissing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"log":{"level":"info"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	m := singbox.NewManager(filepath.Join(dir, "no-such-sing-box"), cfgPath)

	err := m.Start()
	if err == nil {
		t.Fatal("二进制不存在时 Start 应报错，否则 reload 分支会误判为已启动")
	}
	if m.IsRunning() {
		t.Error("启动失败后 IsRunning 不能为 true")
	}
}

// 配置文件不存在也要明确报错，而不是静默成功。
func TestManagerStartFailsWhenConfigMissing(t *testing.T) {
	dir := t.TempDir()
	m := singbox.NewManager("/bin/sh", filepath.Join(dir, "missing.json"))

	if err := m.Start(); err == nil {
		t.Fatal("配置不存在时 Start 应报错")
	}
}
