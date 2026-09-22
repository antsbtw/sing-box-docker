package tunnel

// SSH 凭据验证。接线契约 tunnel v1 §3。
//
// 凭据由后端用 ed25519 签发，agent 用公钥离线验证 —— 不必每次回问后端。
// 这也意味着 node_secret 泄露只能让攻击者冒充这台 agent 连隧道，
// **签不出凭据**（契约 §5）。

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	credPrefix = "obt1"
	// 签名输入前缀。⚠️ 必须与后端逐字节一致，改动即全部验证失败。
	sigContext = "obox-tunnel-cred-v1\n"
	// 时钟偏差容忍（契约 §3.2 第 4 条）
	clockSkew = 60 * time.Second
)

var (
	ErrCredMalformed = errors.New("credential malformed")
	ErrCredSignature = errors.New("credential signature invalid")
	ErrCredVersion   = errors.New("credential version mismatch")
	ErrCredKeyID     = errors.New("credential key id mismatch")
	ErrCredExpired   = errors.New("credential expired or not yet valid")
	ErrCredNode      = errors.New("credential node mismatch")
	ErrCredSession   = errors.New("credential session mismatch")
	ErrCredReplayed  = errors.New("credential already used")
)

// CredPayload 是凭据中段解出来的内容（契约 §3.1）。
type CredPayload struct {
	V         int    `json:"v"`
	KID       string `json:"kid"`
	NodeID    string `json:"node_id"`
	SessionID string `json:"session_id"`
	IAT       int64  `json:"iat"`
	EXP       int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}

// CredVerifier 按契约 §3.2 的七步验证凭据。
//
// nonce 记在内存即可：凭据 5 分钟过期，进程重启后旧凭据也都失效了。
type CredVerifier struct {
	mu        sync.Mutex
	usedNonce map[string]time.Time
}

func NewCredVerifier() *CredVerifier {
	return &CredVerifier{usedNonce: make(map[string]time.Time)}
}

// Verify 验证凭据。
//
// now 应取自最近一次 poll 的 server_time 推算值 —— 不信本机时钟
// （VPS 时钟漂移会让合法凭据被拒、或过期凭据被放行）。
//
// ⚠️ 返回的错误仅供日志，**不要回给 SSH 客户端**：
// 泄露"哪一条不过"会帮助攻击者逐项试探（契约 §3.2 末）。
func (v *CredVerifier) Verify(cred string, pubkey ed25519.PublicKey, keyID, nodeID, sessionID string, now time.Time) error {
	parts := strings.Split(cred, ".")
	if len(parts) != 3 || parts[0] != credPrefix {
		return ErrCredMalformed
	}

	payloadRaw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrCredMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrCredMalformed
	}

	// ⚠️ 签名输入用第二段的**原文**，不能解码后再编码 ——
	// base64 有多种等价编码，重新编码可能得到不同字节串。
	if !ed25519.Verify(pubkey, []byte(sigContext+parts[1]), sig) {
		return ErrCredSignature
	}

	var p CredPayload
	if err := json.Unmarshal(payloadRaw, &p); err != nil {
		return ErrCredMalformed
	}

	if p.V != 1 {
		return ErrCredVersion
	}
	if p.KID != keyID {
		return ErrCredKeyID
	}

	unix := now.Unix()
	skew := int64(clockSkew.Seconds())
	if unix > p.EXP+skew || unix < p.IAT-skew {
		return ErrCredExpired
	}

	// 绑定到本节点与本会话 —— 防止把 A 会话的凭据拿到 B 会话用
	if p.NodeID != nodeID {
		return ErrCredNode
	}
	if p.SessionID != sessionID {
		return ErrCredSession
	}

	if !v.consumeNonce(p.Nonce, now) {
		return ErrCredReplayed
	}
	return nil
}

func (v *CredVerifier) consumeNonce(nonce string, now time.Time) bool {
	if nonce == "" {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, used := v.usedNonce[nonce]; used {
		return false
	}
	// 顺手清理过期条目，避免长期运行内存无限增长
	for n, t := range v.usedNonce {
		if now.Sub(t) > 30*time.Minute {
			delete(v.usedNonce, n)
		}
	}
	v.usedNonce[nonce] = now
	return true
}

// KeyIDFor 按契约 §3.2 第 3 条计算 kid：sha256(pubkey) 前 8 字节的 hex。
func KeyIDFor(pubkey ed25519.PublicKey) string {
	sum := sha256.Sum256(pubkey)
	return hex.EncodeToString(sum[:8])
}

// ParsePubkey 解析后端下发的 base64url 公钥。
func ParsePubkey(s string) (ed25519.PublicKey, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.URLEncoding.DecodeString(s); err != nil {
			return nil, fmt.Errorf("decode tunnel pubkey: %w", err)
		}
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("tunnel pubkey length = %d, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}
