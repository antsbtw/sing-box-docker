package tunnel

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// 端到端：用真实的 SSH 客户端连内置服务端，验证认证与 exec。
//
// 返回的 signer 是一台已授权设备的钥匙 —— 认证只有设备持钥这一种。
func newServerPair(t *testing.T) (*Server, ssh.Signer, string, string) {
	t.Helper()
	const nodeID, sessionID = "byo-test01", "sess-abc"

	srv, err := NewServer(nodeID, "node-secret-xyz", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := NewAuthKeyStore(t.TempDir())
	line, _, signer := makeDeviceKey(t, "test-device")
	if _, err := store.Reset(line, "test"); err != nil {
		t.Fatal(err)
	}
	srv.EnableOwnerKeys(store)
	return srv, signer, nodeID, sessionID
}

// dial 在本地 TCP 环回上跑服务端，返回已连接的 SSH 客户端。
//
// ⚠️ 不能用 net.Pipe：它是**无缓冲**的同步管道，SSH 握手双方
// 同时写入时会互相阻塞，测试直接挂死。
func dial(t *testing.T, srv *Server, sessionID string, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	return dialWithKey(t, srv, sessionID, signer)
}

// v1.14.0:密码认证分支已删除。任何密码 —— 包括曾经合法格式的后端凭据 —— 一律被拒。
//
// 这条是「后端被攻破也进不去用户的机器」的回归测试。
func TestSSHServerRejectsAnyPassword(t *testing.T) {
	srv, _, _, sessionID := newServerPair(t)
	c2 := serveOnLoopback(t, srv, sessionID)
	cfg := &ssh.ClientConfig{
		User:            sshUsername,
		Auth:            []ssh.AuthMethod{ssh.Password("obt1.payload.signature")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	if _, _, _, err := ssh.NewClientConn(c2, "tunnel", cfg); err == nil {
		t.Fatal("密码认证必须一律被拒")
	}
}

// 没有启用设备钥匙列表时,连接直接失败 —— 不存在任何退路。
func TestSSHServerWithoutOwnerKeysRefuses(t *testing.T) {
	srv, err := NewServer("n", "s", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, _, signer := makeDeviceKey(t, "x")
	if _, err := dial(t, srv, "sess", signer); err == nil {
		t.Fatal("未启用设备钥匙时不应能登录")
	}
}

// A-10:exec 的 stdin 能送到命令 —— 配方的密参经 stdin 传入。
func TestSSHServerExecReceivesStdin(t *testing.T) {
	srv, signer, _, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, _ := client.NewSession()
	defer sess.Close()
	sess.Stdin = strings.NewReader(`{"auth_key":"k"}`)
	out, err := sess.Output("cat")
	if err != nil {
		t.Fatalf("exec 失败：%v", err)
	}
	if string(out) != `{"auth_key":"k"}` {
		t.Errorf("stdin 未送达，输出 = %q", out)
	}
}

// 不发 stdin、也不半关闭的客户端不能卡住(2026-09-23 的 `id -u` 卡死)。
func TestSSHServerExecWithoutStdinDoesNotHang(t *testing.T) {
	srv, signer, _, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	ch, reqs, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(reqs)
	payload := ssh.Marshal(struct{ Command string }{"echo done"})
	if ok, err := ch.SendRequest("exec", true, payload); err != nil || !ok {
		t.Fatalf("exec 请求失败：%v", err)
	}
	// 故意不 CloseWrite

	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(ch)
		done <- b
	}()
	select {
	case b := <-done:
		if strings.TrimSpace(string(b)) != "done" {
			t.Errorf("输出 = %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未半关闭 stdin 的客户端卡住了")
	}
}

// A-5：exec 能执行任意命令 —— 这是"装任何软件"的基础。
func TestSSHServerExec(t *testing.T) {
	srv, signer, _, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, signer)
	if err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	out, err := sess.Output("echo tunnel-works")
	if err != nil {
		t.Fatalf("exec 失败：%v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "tunnel-works" {
		t.Errorf("输出 = %q，期望 tunnel-works", got)
	}
}

// exec 的退出码要能正确回传 —— 安装脚本靠它判断成败。
func TestSSHServerExecExitCode(t *testing.T) {
	srv, signer, _, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, signer)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	sess, _ := client.NewSession()
	defer sess.Close()

	err = sess.Run("exit 42")
	var ee *ssh.ExitError
	if !asExitError(err, &ee) {
		t.Fatalf("应返回 ExitError，得到 %v", err)
	}
	if ee.ExitStatus() != 42 {
		t.Errorf("退出码 = %d，期望 42", ee.ExitStatus())
	}
}

func asExitError(err error, target **ssh.ExitError) bool {
	if e, ok := err.(*ssh.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// 契约 §2.3：用户名固定 obox，其他一律拒。
func TestSSHServerRejectsWrongUsername(t *testing.T) {
	srv, signer, _, sessionID := newServerPair(t)

	c2 := serveOnLoopback(t, srv, sessionID)

	cfg := &ssh.ClientConfig{
		User:            "root", // 不是 obox
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	if _, _, _, err := ssh.NewClientConn(c2, "tunnel", cfg); err == nil {
		t.Error("非 obox 用户名应被拒")
	}
}

func fpOf(t *testing.T, s *Server) string {
	t.Helper()
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

// A-2：同一台机器重启，指纹必须不变。
func TestHostKeyStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	s1, err := NewServer("n", "secret-a", dir)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := NewServer("n", "secret-a", dir)

	if fpOf(t, s1) != fpOf(t, s2) {
		t.Error("同一台机器重启后指纹必须一致")
	}
}

// ⚠️ 这是改用本机种子的**全部意义**：
// 带 token 重装会让后端轮换 node_secret（install.sh H-3 删 node.json
// → agent 重新注册 → 后端按 machine_id 去重并换 secret）。
//
// 旧实现从 node_secret 派生，于是每次重装都换指纹，App 每次都弹
// "主机密钥已更改"。一个每次都要点确认的安全提示等于没有提示 ——
// 用户会习惯性点"信任"，真有人冒充时也照点不误。
func TestHostKeySurvivesSecretRotation(t *testing.T) {
	dir := t.TempDir()
	before, err := NewServer("n", "secret-before-reinstall", dir)
	if err != nil {
		t.Fatal(err)
	}
	// 重装：node_secret 换了，data/ 里的种子还在
	after, _ := NewServer("n", "secret-AFTER-reinstall", dir)

	if fpOf(t, before) != fpOf(t, after) {
		t.Error("后端轮换 node_secret 不该改变指纹 —— 否则每次重装都告警，" +
			"而每次都要点的提示等于没有提示")
	}
}

// 换机器（或 data/ 被清空）指纹必须变 —— 那才是真信号。
func TestHostKeyChangesOnNewMachine(t *testing.T) {
	a, err := NewServer("n", "same-secret", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewServer("n", "same-secret", t.TempDir())

	if fpOf(t, a) == fpOf(t, b) {
		t.Error("不同机器应有不同指纹 —— 否则 pin 分辨不出冒充")
	}
}

// 种子文件权限必须是 0600：拿到它就能冒充这台机器。
func TestHostKeySeedPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewServer("n", "s", dir); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, hostKeySeedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("种子权限应为 0600，实际 %o —— 拿到它就能冒充这台机器", perm)
	}
}

// 种子文件损坏时重新生成，而不是让 agent 起不来。
//
// 指纹变化只需用户确认一次；起不来则整台机器失联 —— 两害相权取其轻。
func TestCorruptSeedRegenerates(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewServer("n", "s", dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hostKeySeedFileName)
	if err := os.WriteFile(path, []byte("not-hex-garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer("n", "s", dir)
	if err != nil {
		t.Fatalf("种子损坏不该让 agent 起不来：%v", err)
	}
	if srv.hostKey == nil {
		t.Error("应重新生成可用的主机密钥")
	}
}

// serveOnLoopback 起一个监听，把接入的连接交给 SSH 服务端，返回客户端侧连接。
func serveOnLoopback(t *testing.T, srv *Server, sessionID string) net.Conn {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_ = srv.Serve(conn, sessionID)
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
