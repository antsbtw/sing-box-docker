package tunnel

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// 端到端：用真实的 SSH 客户端连内置服务端，验证认证与 exec。
func newServerPair(t *testing.T) (*Server, ed25519.PrivateKey, string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const nodeID, sessionID = "byo-test01", "sess-abc"

	srv, err := NewServer(nodeID, "node-secret-xyz", t.TempDir(), NewCredVerifier())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetTunnelKey(pub, KeyIDFor(pub))
	return srv, priv, nodeID, sessionID
}

func credFor(t *testing.T, priv ed25519.PrivateKey, nodeID, sessionID, nonce string) string {
	t.Helper()
	pub := priv.Public().(ed25519.PublicKey)
	now := time.Now()
	p := CredPayload{
		V: 1, KID: KeyIDFor(pub), NodeID: nodeID, SessionID: sessionID,
		IAT: now.Unix(), EXP: now.Add(5 * time.Minute).Unix(), Nonce: nonce,
	}
	raw, _ := json.Marshal(p)
	mid := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(sigContext+mid))
	return credPrefix + "." + mid + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// dial 在本地 TCP 环回上跑服务端，返回已连接的 SSH 客户端。
//
// ⚠️ 不能用 net.Pipe：它是**无缓冲**的同步管道，SSH 握手双方
// 同时写入时会互相阻塞，测试直接挂死。
func dial(t *testing.T, srv *Server, sessionID, password string) (*ssh.Client, error) {
	t.Helper()
	c2 := serveOnLoopback(t, srv, sessionID)

	cfg := &ssh.ClientConfig{
		User:            sshUsername,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	conn, chans, reqs, err := ssh.NewClientConn(c2, "tunnel", cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func TestSSHServerAcceptsValidCredential(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	cred := credFor(t, priv, nodeID, sessionID, "n1")

	client, err := dial(t, srv, sessionID, cred)
	if err != nil {
		t.Fatalf("合法凭据应能登录：%v", err)
	}
	defer client.Close()
}

// A-5：exec 能执行任意命令 —— 这是"装任何软件"的基础。
func TestSSHServerExec(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "n2"))
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
	srv, priv, nodeID, sessionID := newServerPair(t)
	client, err := dial(t, srv, sessionID, credFor(t, priv, nodeID, sessionID, "n3"))
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

func TestSSHServerRejectsBadCredential(t *testing.T) {
	srv, _, _, sessionID := newServerPair(t)
	if _, err := dial(t, srv, sessionID, "obt1.garbage.sig"); err == nil {
		t.Error("无效凭据应被拒")
	}
}

// 契约 §2.3：用户名固定 obox，其他一律拒。
func TestSSHServerRejectsWrongUsername(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	cred := credFor(t, priv, nodeID, sessionID, "n4")

	c2 := serveOnLoopback(t, srv, sessionID)

	cfg := &ssh.ClientConfig{
		User:            "root", // 不是 obox
		Auth:            []ssh.AuthMethod{ssh.Password(cred)},
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
	s1, err := NewServer("n", "secret-a", dir, NewCredVerifier())
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := NewServer("n", "secret-a", dir, NewCredVerifier())

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
	before, err := NewServer("n", "secret-before-reinstall", dir, NewCredVerifier())
	if err != nil {
		t.Fatal(err)
	}
	// 重装：node_secret 换了，data/ 里的种子还在
	after, _ := NewServer("n", "secret-AFTER-reinstall", dir, NewCredVerifier())

	if fpOf(t, before) != fpOf(t, after) {
		t.Error("后端轮换 node_secret 不该改变指纹 —— 否则每次重装都告警，" +
			"而每次都要点的提示等于没有提示")
	}
}

// 换机器（或 data/ 被清空）指纹必须变 —— 那才是真信号。
func TestHostKeyChangesOnNewMachine(t *testing.T) {
	a, err := NewServer("n", "same-secret", t.TempDir(), NewCredVerifier())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewServer("n", "same-secret", t.TempDir(), NewCredVerifier())

	if fpOf(t, a) == fpOf(t, b) {
		t.Error("不同机器应有不同指纹 —— 否则 pin 分辨不出冒充")
	}
}

// 种子文件权限必须是 0600：拿到它就能冒充这台机器。
func TestHostKeySeedPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewServer("n", "s", dir, NewCredVerifier()); err != nil {
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
	if _, err := NewServer("n", "s", dir, NewCredVerifier()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, hostKeySeedFileName)
	if err := os.WriteFile(path, []byte("not-hex-garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer("n", "s", dir, NewCredVerifier())
	if err != nil {
		t.Fatalf("种子损坏不该让 agent 起不来：%v", err)
	}
	if srv.hostKey == nil {
		t.Error("应重新生成可用的主机密钥")
	}
}

// 凭据一次性：同一条不能用两次（S-2）。
func TestSSHServerRejectsReplayedCredential(t *testing.T) {
	srv, priv, nodeID, sessionID := newServerPair(t)
	cred := credFor(t, priv, nodeID, sessionID, "n5")

	c, err := dial(t, srv, sessionID, cred)
	if err != nil {
		t.Fatalf("首次应成功：%v", err)
	}
	c.Close()

	if _, err := dial(t, srv, sessionID, cred); err == nil {
		t.Error("同一凭据第二次应被拒")
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
