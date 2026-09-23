package config

import (
	"os"
	"strings"
	"testing"
)

// ⚠️ SNI 只能有一个来源。
//
// generator 与 local API 曾各写死一份，改其一就两边不一致、
// 客户端连不上 —— 而且报错只有一个沉默的 EOF，极难排查。
func TestRealitySNIHasSingleSource(t *testing.T) {
	os.Setenv("NODE_API_KEY", "x")
	defer os.Unsetenv("NODE_API_KEY")
	os.Unsetenv("REALITY_SNI")

	cfg := LoadFromEnv()
	if cfg.RealitySNI == "" {
		t.Fatal("默认 SNI 不能为空 —— 空值会让 Reality 配置不合法")
	}
}

// REALITY_SNI 必须可以被环境变量覆盖。
//
// 默认值在某些地区/线路上未必可用（microsoft.com 就是这么废掉的），
// 运维得能不重新发版就换掉它。
func TestRealitySNIOverridable(t *testing.T) {
	os.Setenv("NODE_API_KEY", "x")
	os.Setenv("REALITY_SNI", "www.example.com")
	defer func() {
		os.Unsetenv("NODE_API_KEY")
		os.Unsetenv("REALITY_SNI")
	}()

	if got := LoadFromEnv().RealitySNI; got != "www.example.com" {
		t.Errorf("REALITY_SNI 应可覆盖，实际 %q", got)
	}
}

// ⚠️ 不要再用 www.microsoft.com。
//
// 2026-09-23 真机实测：同一台 VPS、同一份配置、同一把全新密钥，
// SNI 为 microsoft.com 时服务端一律
//     REALITY: processed invalid connection
// 换成 apple.com 立刻连通。唯一的变量就是它。
//
// 这条测试不是洁癖 —— 它挡的是"顺手改回去"：microsoft.com
// 用 openssl 测起来完全正常（TLSv1.3、证书有效），看着没问题，
// 所以很容易被当成安全的默认值重新引入。
func TestDefaultSNIIsNotMicrosoft(t *testing.T) {
	os.Setenv("NODE_API_KEY", "x")
	defer os.Unsetenv("NODE_API_KEY")
	os.Unsetenv("REALITY_SNI")

	if sni := LoadFromEnv().RealitySNI; strings.Contains(sni, "microsoft") {
		t.Errorf("默认 SNI 不能用 microsoft.com（实测 Reality 握手必失败），实际 %q", sni)
	}
}
