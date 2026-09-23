package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// 造一把 ed25519 设备钥匙，返回 OpenSSH 公钥行与它的指纹。
func makeDeviceKey(t *testing.T, name string) (line, fp string, signer ssh.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	signer, err = ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + name
	return line, ssh.FingerprintSHA256(sshPub), signer
}

func newStore(t *testing.T) *AuthKeyStore {
	t.Helper()
	return NewAuthKeyStore(t.TempDir())
}

// 没有钥匙是合法状态 —— 无 --owner-key 装出来的节点就是这样。
// 它应当「可见但进不去」，而不是报错。
func TestEmptyListIsValidState(t *testing.T) {
	s := newStore(t)
	keys, err := s.List()
	if err != nil {
		t.Fatalf("空列表不该报错：%v", err)
	}
	if len(keys) != 0 {
		t.Errorf("应为空，实际 %d 把", len(keys))
	}

	line, _, _ := makeDeviceKey(t, "someone")
	pub, _, _, _ := ParseAuthorizedKey(line)
	if _, ok := s.Authorized(pub); ok {
		t.Error("没有钥匙时任何公钥都不该被授权")
	}
}

// Reset 重置为仅此一把 —— 重装即「从这台设备重新掌控」。
func TestResetReplacesAllKeys(t *testing.T) {
	s := newStore(t)
	old, _, _ := makeDeviceKey(t, "old-phone")
	if _, err := s.Add(old, "enroll"); err != nil {
		t.Fatal(err)
	}

	newLine, newFP, _ := makeDeviceKey(t, "new-phone")
	if _, err := s.Reset(newLine, "enroll"); err != nil {
		t.Fatal(err)
	}

	keys, _ := s.List()
	if len(keys) != 1 {
		t.Fatalf("重置后应只剩一把，实际 %d 把", len(keys))
	}
	if keys[0].FP != newFP {
		t.Error("留下的应该是新钥匙")
	}

	// 旧设备立刻进不去
	oldPub, _, _, _ := ParseAuthorizedKey(old)
	if _, ok := s.Authorized(oldPub); ok {
		t.Error("重装后旧设备必须进不去")
	}
}

// 授权判断比对公钥本体。
func TestAuthorizedMatchesRealKey(t *testing.T) {
	s := newStore(t)
	line, _, _ := makeDeviceKey(t, "iPhone")
	if _, err := s.Add(line, "enroll"); err != nil {
		t.Fatal(err)
	}

	pub, _, _, _ := ParseAuthorizedKey(line)
	if _, ok := s.Authorized(pub); !ok {
		t.Error("列表里的钥匙应被授权")
	}

	other, _, _ := makeDeviceKey(t, "attacker")
	otherPub, _, _, _ := ParseAuthorizedKey(other)
	if _, ok := s.Authorized(otherPub); ok {
		t.Error("不在列表里的钥匙必须被拒")
	}
}

// 重复添加同一把是幂等的，不该产生两条。
func TestAddIsIdempotent(t *testing.T) {
	s := newStore(t)
	line, _, _ := makeDeviceKey(t, "iPad")
	_, _ = s.Add(line, "local")
	_, _ = s.Add(line, "local")

	keys, _ := s.List()
	if len(keys) != 1 {
		t.Errorf("重复添加应保持一把，实际 %d 把", len(keys))
	}
}

// ⚠️ 不能删到列表为空 —— 删光了这台机器就再也进不去，
// 只能去云控制台重装。这是最容易把用户锁在外面的操作。
func TestCannotRemoveLastKey(t *testing.T) {
	s := newStore(t)
	line, fp, _ := makeDeviceKey(t, "only-phone")
	if _, err := s.Add(line, "enroll"); err != nil {
		t.Fatal(err)
	}

	err := s.Remove(fp)
	if err == nil {
		t.Fatal("删最后一把必须被拒 —— 否则用户被锁在机器外面")
	}
	if !strings.Contains(err.Error(), "最后一把") {
		t.Errorf("错误信息要说清原因，实际：%v", err)
	}

	keys, _ := s.List()
	if len(keys) != 1 {
		t.Error("被拒之后钥匙必须还在")
	}
}

// 有两把时可以删一把。
func TestRemoveWorksWhenOthersRemain(t *testing.T) {
	s := newStore(t)
	a, fpA, _ := makeDeviceKey(t, "phone")
	b, _, _ := makeDeviceKey(t, "tablet")
	_, _ = s.Add(a, "enroll")
	_, _ = s.Add(b, "local")

	if err := s.Remove(fpA); err != nil {
		t.Fatalf("有两把时应能删一把：%v", err)
	}

	aPub, _, _, _ := ParseAuthorizedKey(a)
	if _, ok := s.Authorized(aPub); ok {
		t.Error("删掉的设备必须立刻进不去")
	}
	bPub, _, _, _ := ParseAuthorizedKey(b)
	if _, ok := s.Authorized(bPub); !ok {
		t.Error("没删的设备仍应可进")
	}
}

// 删一把不存在的要报错，不能静默成功。
func TestRemoveUnknownFingerprintFails(t *testing.T) {
	s := newStore(t)
	line, _, _ := makeDeviceKey(t, "phone")
	_, _ = s.Add(line, "enroll")
	_, _ = s.Add(func() string { l, _, _ := makeDeviceKey(t, "pad"); return l }(), "local")

	if err := s.Remove("SHA256:doesnotexist"); err == nil {
		t.Error("删不存在的指纹应报错")
	}
}

// 上报给后端的只有指纹与名字 —— 公钥本体不出机器。
func TestFingerprintsDoNotLeakPublicKey(t *testing.T) {
	s := newStore(t)
	line, fp, _ := makeDeviceKey(t, "iPhone 15")
	_, _ = s.Add(line, "enroll")

	out := s.Fingerprints()
	if len(out) != 1 {
		t.Fatalf("应有一项，实际 %d", len(out))
	}
	if out[0]["fp"] != fp || out[0]["name"] != "iPhone 15" {
		t.Errorf("指纹或名字不对：%v", out[0])
	}
	if _, has := out[0]["pub"]; has {
		t.Error("上报内容不该包含公钥本体")
	}
}

// 文件权限必须是 0600 —— 它决定谁能进这台机器。
func TestFilePermissionsAre0600(t *testing.T) {
	dir := t.TempDir()
	s := NewAuthKeyStore(dir)
	line, _, _ := makeDeviceKey(t, "phone")
	if _, err := s.Add(line, "enroll"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, authKeysFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("权限应为 0600，实际 %o", perm)
	}
}

// 坏掉的公钥行要当场拒绝，不能写进列表。
func TestRejectsMalformedKey(t *testing.T) {
	s := newStore(t)
	for _, bad := range []string{"", "not-a-key", "ssh-ed25519 !!!notbase64!!!"} {
		if _, err := s.Add(bad, "local"); err == nil {
			t.Errorf("应拒绝坏公钥：%q", bad)
		}
	}
	keys, _ := s.List()
	if len(keys) != 0 {
		t.Error("坏公钥不该写进列表")
	}
}

// ── publickey 认证的端到端验证 ──────────────────────────────

// dialWithKey 用设备私钥登录（owner key 方案）。
func dialWithKey(t *testing.T, srv *Server, sessionID string, signer ssh.Signer) (*ssh.Client, error) {
	t.Helper()
	c2 := serveOnLoopback(t, srv, sessionID)

	cfg := &ssh.ClientConfig{
		User:            sshUsername,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	conn, chans, reqs, err := ssh.NewClientConn(c2, "tunnel", cfg)
	if err != nil {
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

// 授权设备能用私钥登进来；未授权的不能。
//
// 这是 owner key 方案的核心：裁决依据是本机的列表，
// 不是任何远端下发的东西。
func TestPublicKeyAuthAcceptsAuthorizedDevice(t *testing.T) {
	dir := t.TempDir()
	store := NewAuthKeyStore(dir)
	line, _, signer := makeDeviceKey(t, "my-iphone")
	if _, err := store.Reset(line, "enroll"); err != nil {
		t.Fatal(err)
	}

	srv, _, _, sessionID := newServerPair(t)
	srv.EnableOwnerKeys(store)

	client, err := dialWithKey(t, srv, sessionID, signer)
	if err != nil {
		t.Fatalf("授权设备应当能登入：%v", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	out, err := sess.Output("echo owner-key-ok")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "owner-key-ok") {
		t.Errorf("命令没跑起来，输出：%q", out)
	}
}

// 未授权的设备一律被拒 —— 哪怕它拿到了会话、连上了隧道。
func TestPublicKeyAuthRejectsUnknownDevice(t *testing.T) {
	dir := t.TempDir()
	store := NewAuthKeyStore(dir)
	owner, _, _ := makeDeviceKey(t, "owner")
	if _, err := store.Reset(owner, "enroll"); err != nil {
		t.Fatal(err)
	}

	srv, _, _, sessionID := newServerPair(t)
	srv.EnableOwnerKeys(store)

	_, _, attackerSigner := makeDeviceKey(t, "attacker")
	if c, err := dialWithKey(t, srv, sessionID, attackerSigner); err == nil {
		c.Close()
		t.Fatal("未授权设备必须被拒 —— 这是本方案的全部意义")
	}
}

// 没有任何钥匙的节点：可见，但谁都进不去。
func TestPublicKeyAuthRejectsWhenNoKeys(t *testing.T) {
	store := NewAuthKeyStore(t.TempDir())

	srv, _, _, sessionID := newServerPair(t)
	srv.EnableOwnerKeys(store)

	_, _, signer := makeDeviceKey(t, "anyone")
	if c, err := dialWithKey(t, srv, sessionID, signer); err == nil {
		c.Close()
		t.Fatal("无钥匙节点必须拒绝所有登录")
	}
}

// 删掉之后立刻进不去 —— 不需要重启 agent。
func TestRemovedDeviceLosesAccessImmediately(t *testing.T) {
	dir := t.TempDir()
	store := NewAuthKeyStore(dir)
	keep, _, _ := makeDeviceKey(t, "keep")
	gone, goneFP, goneSigner := makeDeviceKey(t, "gone")
	_, _ = store.Add(keep, "enroll")
	_, _ = store.Add(gone, "local")

	srv, _, _, sessionID := newServerPair(t)
	srv.EnableOwnerKeys(store)

	// 删之前能进
	c1, err := dialWithKey(t, srv, sessionID, goneSigner)
	if err != nil {
		t.Fatalf("删除前应当能进：%v", err)
	}
	c1.Close()

	if err := store.Remove(goneFP); err != nil {
		t.Fatal(err)
	}

	if c2, err := dialWithKey(t, srv, sessionID, goneSigner); err == nil {
		c2.Close()
		t.Fatal("删除后必须立刻进不去")
	}
}
