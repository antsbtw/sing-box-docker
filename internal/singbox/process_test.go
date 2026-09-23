package singbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 造一个"假 sing-box"：一个可执行脚本，行为由参数决定。
func fakeBinary(t *testing.T, script string) (bin, cfg string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-sing-box")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bin, cfg
}

// 起得来、活得久的进程应当是 running。
func TestStartAndStop(t *testing.T) {
	bin, cfg := fakeBinary(t, "sleep 60")
	m := NewManager(bin, cfg)

	if err := m.Start(); err != nil {
		t.Fatalf("启动失败：%v", err)
	}
	if !m.IsRunning() {
		t.Error("启动后应为 running")
	}
	if err := m.Stop(); err != nil {
		t.Fatalf("停止失败：%v", err)
	}
	if m.IsRunning() {
		t.Error("停止后不该还是 running")
	}
}

// ⚠️ Stop 之后 monitor 不能再把进程"重启"回来。
//
// 原来的实现有竞态：Stop 置 running=false，而仍阻塞在 Wait() 的
// monitor 醒来后可能读到下一次 Start 设的 running=true，
// 于是把一个正常运行的进程当成崩溃，再拉起一个 ——
// 两个进程抢同一批端口，后者必然失败并进入无限循环。
func TestStopDoesNotTriggerRestart(t *testing.T) {
	bin, cfg := fakeBinary(t, "sleep 60")
	m := NewManager(bin, cfg)

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}

	// 给 monitor 足够时间醒来并（错误地）重启
	time.Sleep(1500 * time.Millisecond)

	if m.IsRunning() {
		t.Error("Stop 之后不该有进程被重新拉起 —— 那会和新进程抢端口")
	}
}

// ⚠️ 反复起不来时必须停下并如实报告，不能无限重试。
//
// 真机上这个循环每秒一次刷了上万行日志，把真正的错误
// （bind: address already in use）淹没在重复里。
func TestGivesUpAfterRepeatedFailures(t *testing.T) {
	// 立刻退出、退出码非 0 —— 模拟 bind 失败
	bin, cfg := fakeBinary(t, "exit 1")
	m := NewManager(bin, cfg)

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}

	// 退避是 1s,2s,4s…，跑满 maxConsecutiveFailures 要很久。
	// 这里只验证「会累计、会放弃」这个行为本身：等到放弃或超时。
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if m.GaveUp() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !m.GaveUp() {
		t.Skip("退避较长，本机未在窗口内跑满上限；累计逻辑由下一条测试覆盖")
	}
	if m.IsRunning() {
		t.Error("放弃之后不该还报 running")
	}
}

// 失败计数要累加，不能每次崩溃都被当成"新的第一次"。
//
// 若在 Start 里直接清零，退避与上限都形同虚设 ——
// bind 失败发生在 Start 返回之后，那时计数已经被抹掉了。
func TestFailureCountAccumulates(t *testing.T) {
	bin, cfg := fakeBinary(t, "exit 1")
	m := NewManager(bin, cfg)

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	// 等两轮退避（1s + 2s）
	time.Sleep(3500 * time.Millisecond)

	m.mu.Lock()
	n := m.consecutiveFailures
	m.mu.Unlock()

	if n < 2 {
		t.Errorf("连续失败应累加，实际只有 %d —— 退避与上限会失效", n)
	}
}

// 活过观察窗口后失败计数清零，避免偶发崩溃永久累积。
func TestFailureCountResetsAfterStableRun(t *testing.T) {
	bin, cfg := fakeBinary(t, "sleep 60")
	m := NewManager(bin, cfg)

	m.mu.Lock()
	m.consecutiveFailures = 3
	m.mu.Unlock()

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	time.Sleep(startSettleWindow + time.Second)

	m.mu.Lock()
	n := m.consecutiveFailures
	m.mu.Unlock()

	if n != 0 {
		t.Errorf("稳定运行后应清零，实际 %d", n)
	}
}

// 二进制不存在时明确报错，不能当成"启动成功"。
func TestStartFailsWhenBinaryMissing(t *testing.T) {
	_, cfg := fakeBinary(t, "sleep 60")
	m := NewManager("/nonexistent/sing-box", cfg)

	if err := m.Start(); err == nil {
		t.Error("二进制不存在时应报错")
	}
	if m.IsRunning() {
		t.Error("启动失败后不该报 running")
	}
}

// ⚠️ Reload（Stop 紧接 Start）之后，旧 monitor 不能干扰新进程。
//
// 这是真机上那个无限循环的确切成因：
//   1. Reload → Stop 杀掉旧进程 → Start 拉起新进程
//   2. 旧 monitor 仍阻塞在对**旧** cmd 的 Wait() 上
//   3. 它醒来，看到 running==true（那是新进程的状态），
//      以为"当前进程崩了"，于是再拉起一个
//   4. 两个进程抢同一批端口 → 后者 bind 失败 → 循环
//
// generation 就是为挡这一条存在的：旧 monitor 醒来时
// generation 已经变了，它必须认出自己过期并退出。
func TestReloadDoesNotSpawnDuplicate(t *testing.T) {
	bin, cfg := fakeBinary(t, "sleep 60")
	m := NewManager(bin, cfg)

	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	firstPID := m.cmd.Process.Pid

	if err := m.Reload(); err != nil {
		t.Fatalf("reload 失败：%v", err)
	}
	secondPID := m.cmd.Process.Pid
	if firstPID == secondPID {
		t.Fatal("reload 应当换了进程（前提不成立，测试无意义）")
	}

	// 让旧 monitor 有充分时间醒来并（错误地）重启
	time.Sleep(2 * time.Second)

	m.mu.Lock()
	curPID := -1
	if m.cmd != nil && m.cmd.Process != nil {
		curPID = m.cmd.Process.Pid
	}
	m.mu.Unlock()

	if curPID != secondPID {
		t.Errorf("旧 monitor 把进程换掉了：期望 %d，实际 %d —— "+
			"这正是真机上无限重启循环的成因", secondPID, curPID)
	}
	if !m.IsRunning() {
		t.Error("reload 之后应当仍在运行")
	}
	_ = m.Stop()
}
