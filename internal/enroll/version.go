package enroll

import (
	"net/url"
	"strconv"
	"strings"
)

// parseSemver 解析 "v1.14.0" / "1.14.0"。只认三段纯数字 —— 带后缀的
// (rc、dev)与浮动名(latest)都不算,升级目标必须是正式版本。
func parseSemver(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// isNewerRelease 报告 target 是否是严格高于 current 的正式版本。
//
// current 解析不了(比如本地构建的 "dev")时一律拒绝 —— 无法证明是"升",就不升。
func isNewerRelease(target, current string) bool {
	t, ok := parseSemver(target)
	if !ok {
		return false
	}
	c, ok := parseSemver(current)
	if !ok {
		return false
	}
	for i := 0; i < 3; i++ {
		if t[i] != c[i] {
			return t[i] > c[i]
		}
	}
	return false
}

// sameHost 报告 rawURL 的主机名是否等于 host(不区分大小写,忽略端口)。
// host 为空时一律拒绝 —— 没配 api-url 就不该开隧道。
func sameHost(rawURL, host string) bool {
	if host == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return strings.EqualFold(u.Hostname(), host)
}
