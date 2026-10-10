package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// 新增协议（VMess / Trojan / Hysteria2 / TUIC）的凭据一律从 user.UUID
// 确定性派生，不进用户存储、不改后端 Token 契约。
//
// 关键性质：同一个 UUID 永远派生出同一组凭据。这样 agent 生成 inbound 用的值、
// agent 生成分享链接用的值、以及 App 侧自算分享链接用的值三者天然一致，
// 无需任何一方把派生结果回传给另一方。

// deriveSecret 返回 base64(sha256(domain|uuid)) 的前 22 字符，
// 与 generator.ssMasterPassword 同一手法，作为 Trojan/Hy2/TUIC 的密码。
func deriveSecret(domain, uuid string) string {
	sum := sha256.Sum256([]byte(domain + "|" + uuid))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// VMessUUID：VMess 直接复用用户 UUID。
func VMessUUID(uuid string) string { return uuid }

// TrojanPassword：Trojan 用户密码。
func TrojanPassword(uuid string) string { return deriveSecret("otun-trojan", uuid) }

// Hysteria2Password：Hysteria2 用户密码。
func Hysteria2Password(uuid string) string { return deriveSecret("otun-hy2", uuid) }

// TUICPassword：TUIC 用户密码。
func TUICPassword(uuid string) string { return deriveSecret("otun-tuic-pw", uuid) }

// TUICUUID：TUIC 要求 uuid 形的用户标识，从原 UUID 确定性派生一个 v5 风格的 UUID。
func TUICUUID(uuid string) string {
	sum := sha256.Sum256([]byte("otun-tuic-uuid|" + uuid))
	b := sum[:16]
	// 置版本号(5)与变体位，格式化为标准 UUID 字符串。
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
