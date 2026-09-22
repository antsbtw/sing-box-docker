package tunnel

import (
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
