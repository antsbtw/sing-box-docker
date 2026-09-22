package main

import (
	"context"
	"os"
	"testing"
	"time"
)

// 契约 §5.3：收到解绑后，agent 必须立刻收束主循环并以退出码 3 结束，
// 而不是只置标志、等到收到信号才被发现。
//
// 这里验证的是「置标志 + 收束 context」这对动作：之前 shutdown 不存在，
// Run 会一直挂着，main 里的 revoked 检查永远执行不到。
func TestRevokeCancelsRunContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	a := &Agent{}
	a.mu.Lock()
	a.shutdown = cancel
	a.mu.Unlock()

	if a.Revoked() {
		t.Fatal("初始不应为已解绑")
	}

	cleaned := false
	a.handleRevoked(func() { cleaned = true })

	if !cleaned {
		t.Error("解绑必须清除本地凭据（runner.Cleanup）")
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("解绑后主循环 context 未被收束，进程会一直挂着")
	}

	if !a.Revoked() {
		t.Fatal("Revoked() 应报告已解绑，main 据此以退出码 3 结束")
	}
}

// shutdown 未登记时不能 panic（例如 Run 之前就收到解绑）。
func TestRevokeWithoutShutdownHandle(t *testing.T) {
	a := &Agent{}
	a.handleRevoked(nil)
	if !a.Revoked() {
		t.Fatal("Revoked() 应为 true")
	}
}

// T-3：token 模式下管理面只绑回环，不暴露在公网。
func TestTokenModeBindsLoopback(t *testing.T) {
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer os.Chdir(cwd)
	os.Chdir(dir)
	os.MkdirAll("./data", 0o755)

	// 无 token、无 node.json → 存量 API-Key 模式
	t.Setenv("OTUN_ENROLL_TOKEN", "")
	os.Unsetenv("OTUN_ENROLL_TOKEN")
	if isTokenMode() {
		t.Error("既无 token 也未接入时，不应判为 token 模式（存量节点要保持外部可达）")
	}

	// 有 token → token 模式
	t.Setenv("OTUN_ENROLL_TOKEN", "obx1_test")
	if !isTokenMode() {
		t.Error("配了 OTUN_ENROLL_TOKEN 应判为 token 模式")
	}
}
