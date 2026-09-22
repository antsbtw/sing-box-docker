package tunnel

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func readUntil(t *testing.T, stdout io.Reader, d time.Duration) string {
	t.Helper()
	var mu sync.Mutex
	var out strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(d)
	mu.Lock()
	defer mu.Unlock()
	return out.String()
}

// A-4/A-6：pty-req 的 TERM 与初始窗口尺寸必须传给 shell。
func TestPTYEnvAndWinsize(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "pty1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatalf("RequestPty: %v", err)
	}
	stdout, _ := sess.StdoutPipe()
	stdin, _ := sess.StdinPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	stdin.Write([]byte("echo T=$TERM; stty size; exit\n"))
	out := readUntil(t, stdout, 2*time.Second)
	t.Logf("输出:\n%s", out)

	if !strings.Contains(out, "T=xterm-256color") {
		t.Error("TERM 未传给 shell")
	}
	if !strings.Contains(out, "40 120") {
		t.Error("初始窗口尺寸未生效（stty size 应为 40 120）")
	}
}

// A-6：shell 启动之后到达的 window-change 必须生效（旋屏、键盘弹出）。
func TestWindowChangeAfterShellStart(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "pty2"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdout, _ := sess.StdoutPipe()
	stdin, _ := sess.StdinPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	// shell 已在跑，这时候旋屏
	time.Sleep(300 * time.Millisecond)
	if err := sess.WindowChange(50, 200); err != nil {
		t.Fatalf("WindowChange: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	stdin.Write([]byte("stty size; exit\n"))
	out := readUntil(t, stdout, 2*time.Second)
	t.Logf("输出:\n%s", out)

	if !strings.Contains(out, "50 200") {
		t.Error("shell 启动后的 window-change 未生效（stty size 应为 50 200）")
	}
}

// agent 由 systemd 拉起时环境被裁剪：没有 HOME/USER/SHELL。
// 这种环境下交互 shell 仍必须拿到完整的登录变量，
// 否则登录脚本会清掉 TERM，vi/top 跟着坏（A-4、A-6）——后端联调实测到的偏差。
func TestShellEnvUnderSystemdStrippedEnv(t *testing.T) {
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH", "TERM"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	env := shellEnv("xterm-256color")
	got := map[string]string{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			got[kv[:i]] = kv[i+1:]
		}
	}

	if got["TERM"] != "xterm-256color" {
		t.Errorf("TERM = %q，应为 xterm-256color", got["TERM"])
	}
	for _, k := range []string{"HOME", "USER", "LOGNAME", "SHELL", "PATH"} {
		if got[k] == "" {
			t.Errorf("%s 为空 —— 登录 shell 会因此行为异常", k)
		}
	}
	// 每个变量只能出现一次，重复会让登录脚本读到旧值
	seen := map[string]int{}
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			seen[kv[:i]]++
		}
	}
	for _, k := range []string{"TERM", "HOME", "USER", "PATH"} {
		if seen[k] > 1 {
			t.Errorf("%s 出现 %d 次，应只有一次", k, seen[k])
		}
	}
}

// agent 自身的凭据不应出现在用户终端的环境里。
func TestShellEnvDropsSecrets(t *testing.T) {
	t.Setenv("NODE_API_KEY", "super-secret-key")
	t.Setenv("OTUN_ENROLL_TOKEN", "enroll-token-value")

	for _, kv := range shellEnv("xterm") {
		if strings.Contains(kv, "super-secret-key") || strings.Contains(kv, "enroll-token-value") {
			t.Errorf("凭据泄漏到终端环境：%s", kv)
		}
	}
}

// T-1：shell 退出后必须回 exit-status 并关闭 channel，
// 否则 App 的终端页面会一直挂着，用户看不到「会话已结束」。
func TestShellExitClosesSession(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "exit1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, _ := sess.StdinPipe()
	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	stdin.Write([]byte("exit 7\n"))

	// Wait 必须在 shell 结束后很快返回，并带回退出码
	waitErr := make(chan error, 1)
	go func() { waitErr <- sess.Wait() }()

	select {
	case err := <-waitErr:
		var ee *ssh.ExitError
		if err == nil {
			t.Fatal("期望拿到退出码 7，实际正常返回")
		}
		if !errors.As(err, &ee) {
			t.Fatalf("期望 ExitError，实际 %T: %v", err, err)
		}
		if ee.ExitStatus() != 7 {
			t.Errorf("退出码 = %d，应为 7", ee.ExitStatus())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shell 退出后 10s 内没收到 exit-status/channel 关闭 —— App 终端会挂住")
	}
}

// T-2：pty-req 之后的 exec 要跑在 pty 上（ssh -t host cmd 形态）。
// top、apt 进度条这类命令经 exec 启动时需要（A-4/A-5）。
func TestExecOnPTY(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "exec1"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if err := sess.RequestPty("xterm-256color", 40, 120, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}

	out, err := sess.Output("tty >/dev/null && echo IS_TTY; stty size; echo T=$TERM")
	if err != nil {
		t.Fatalf("exec 失败：%v（输出 %q）", err, string(out))
	}
	got := string(out)
	t.Logf("输出:\n%s", got)

	if !strings.Contains(got, "IS_TTY") {
		t.Error("exec 没有跑在 pty 上（tty 报 not a tty）")
	}
	if !strings.Contains(got, "40 120") {
		t.Error("exec 的 pty 窗口尺寸不对，应为 40 120")
	}
	if !strings.Contains(got, "T=xterm-256color") {
		t.Error("exec 的 TERM 未生效")
	}
}

// 没有 pty-req 的 exec 保持原样：不分配 pty（安装脚本走这条）。
func TestExecWithoutPTYStaysNonInteractive(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "exec2"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	out, err := sess.Output("tty >/dev/null && echo IS_TTY || echo NO_TTY")
	if err != nil {
		t.Fatalf("exec 失败：%v", err)
	}
	if !strings.Contains(string(out), "NO_TTY") {
		t.Errorf("无 pty-req 的 exec 不应分配 pty，输出：%q", string(out))
	}
}

// exec 的退出码要回传（安装脚本据此判断成败）。
func TestExecExitStatus(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "exec3"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	err = sess.Run("exit 7")
	var ee *ssh.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("期望 ExitError，实际 %T: %v", err, err)
	}
	if ee.ExitStatus() != 7 {
		t.Errorf("退出码 = %d，应为 7", ee.ExitStatus())
	}
}
