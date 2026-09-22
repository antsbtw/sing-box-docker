package tunnel

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// 复刻后端的签发逻辑（契约 §3.1），用于验证我们的验签与之一致。
func issue(t *testing.T, priv ed25519.PrivateKey, p CredPayload) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	mid := base64.RawURLEncoding.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(sigContext+mid))
	return credPrefix + "." + mid + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func setup(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string, CredPayload, time.Time) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := CredPayload{
		V: 1, KID: KeyIDFor(pub),
		NodeID: "byo-61c22405", SessionID: "63367892-6ba8-4359-8839-323359f5c0db",
		IAT: now.Unix(), EXP: now.Add(5 * time.Minute).Unix(),
		Nonce: "3f1a9c2e8b7d6e40",
	}
	return pub, priv, KeyIDFor(pub), p, now
}

func TestVerifyAcceptsValidCredential(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	if err := v.Verify(issue(t, priv, p), pub, kid, p.NodeID, p.SessionID, now); err != nil {
		t.Fatalf("合法凭据应通过，得到 %v", err)
	}
}

// 一次性（契约 §2.3、S-2）：同一凭据第二次必须被拒。
func TestVerifyRejectsReplay(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()
	cred := issue(t, priv, p)

	if err := v.Verify(cred, pub, kid, p.NodeID, p.SessionID, now); err != nil {
		t.Fatalf("首次应通过：%v", err)
	}
	if err := v.Verify(cred, pub, kid, p.NodeID, p.SessionID, now); err != ErrCredReplayed {
		t.Errorf("重放应被拒，得到 %v", err)
	}
}

// 跨会话使用必须拒 —— 防止把 A 会话的凭据拿到 B 会话用。
func TestVerifyRejectsCrossSession(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	err := v.Verify(issue(t, priv, p), pub, kid, p.NodeID, "另一个会话", now)
	if err != ErrCredSession {
		t.Errorf("跨会话应被拒，得到 %v", err)
	}
}

func TestVerifyRejectsCrossNode(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	if err := v.Verify(issue(t, priv, p), pub, kid, "byo-other", p.SessionID, now); err != ErrCredNode {
		t.Errorf("跨节点应被拒，得到 %v", err)
	}
}

// 篡改 payload 后签名不再匹配。
func TestVerifyRejectsTamperedPayload(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	cred := issue(t, priv, p)
	// 换掉中段：改成一个 node_id 不同的 payload，签名保持原样
	evil := p
	evil.NodeID = "byo-attacker"
	raw, _ := json.Marshal(evil)
	parts := []byte(cred)
	_ = parts
	tampered := credPrefix + "." + base64.RawURLEncoding.EncodeToString(raw) + "." +
		cred[len(cred)-86:] // 原签名段

	if err := v.Verify(tampered, pub, kid, p.NodeID, p.SessionID, now); err == nil {
		t.Error("篡改 payload 应被拒")
	}
}

// 换一把密钥签的凭据必须拒。
func TestVerifyRejectsWrongKey(t *testing.T) {
	pub, _, kid, p, now := setup(t)
	_, evilPriv, _ := ed25519.GenerateKey(nil)
	v := NewCredVerifier()

	if err := v.Verify(issue(t, evilPriv, p), pub, kid, p.NodeID, p.SessionID, now); err != ErrCredSignature {
		t.Errorf("他人签名应被拒，得到 %v", err)
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	// 超出 60s 容忍
	late := now.Add(6*time.Minute + 2*time.Minute)
	if err := v.Verify(issue(t, priv, p), pub, kid, p.NodeID, p.SessionID, late); err != ErrCredExpired {
		t.Errorf("过期应被拒，得到 %v", err)
	}
}

// ±60s 容忍：轻微时钟偏差不该拒绝合法凭据。
func TestVerifyToleratesClockSkew(t *testing.T) {
	pub, priv, kid, p, now := setup(t)
	v := NewCredVerifier()

	slightlyLate := now.Add(5*time.Minute + 30*time.Second)
	if err := v.Verify(issue(t, priv, p), pub, kid, p.NodeID, p.SessionID, slightlyLate); err != nil {
		t.Errorf("30 秒偏差应被容忍，得到 %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	pub, _, kid, p, now := setup(t)
	v := NewCredVerifier()

	for _, bad := range []string{
		"", "obt1", "obt1.only-two", "wrong.a.b",
		"obt1.!!!notbase64!!!.c3ln", "obt1.eyJ2IjoxfQ.tooshort",
	} {
		if err := v.Verify(bad, pub, kid, p.NodeID, p.SessionID, now); err == nil {
			t.Errorf("畸形凭据 %q 应被拒", bad)
		}
	}
}

// kid 不匹配说明后端换钥了，应拒并促使 agent 刷新公钥。
func TestVerifyRejectsWrongKeyID(t *testing.T) {
	pub, priv, _, p, now := setup(t)
	v := NewCredVerifier()

	if err := v.Verify(issue(t, priv, p), pub, "0000000000000000", p.NodeID, p.SessionID, now); err != ErrCredKeyID {
		t.Errorf("kid 不匹配应被拒，得到 %v", err)
	}
}

func TestKeyIDIsEightBytesHex(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	if kid := KeyIDFor(pub); len(kid) != 16 {
		t.Errorf("kid 应为 16 位 hex（8 字节），得到 %q", kid)
	}
}

func TestParsePubkey(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	s := base64.RawURLEncoding.EncodeToString(pub)

	got, err := ParsePubkey(s)
	if err != nil {
		t.Fatalf("ParsePubkey: %v", err)
	}
	if !got.Equal(pub) {
		t.Error("往返不一致")
	}
	if _, err := ParsePubkey("too-short"); err == nil {
		t.Error("长度不对应报错")
	}
}
