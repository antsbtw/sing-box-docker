package enroll

// 自测公网 IP。接线契约 §4.1 reported_ip。
//
// 后端以请求来源 IP（CF-Connecting-IP）为准，这个值只作对照：
// 两者不一致时标记 nat_suspected，并入可达性提示（契约 §1.2）。
// 所以它是**可选字段，失败留空**，绝不能阻塞注册。

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// 多个来源互为备份：单一服务挂掉或被墙不应让这个字段永远为空。
// 都是纯文本返回 IP 的轻量端点。
var publicIPEndpoints = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
}

const publicIPTimeout = 3 * time.Second

// DetectPublicIP 返回本机公网 IPv4，失败返回空串。
//
// 强制 IPv4（契约示例是 v4，且后端 observed_ip 取自 HTTP 请求来源，
// 若这里报 v6 而实际走 v4 出口，会误标 nat_suspected）。
func DetectPublicIP(ctx context.Context) string {
	// 只拨 IPv4：即便机器有 v6，节点对外服务的地址也是 v4
	dialer := &net.Dialer{Timeout: publicIPTimeout}
	client := &http.Client{
		Timeout: publicIPTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp4", addr)
			},
		},
	}

	for _, url := range publicIPEndpoints {
		if ip := fetchIP(ctx, client, url, false); ip != "" {
			return ip
		}
	}
	return "" // 契约允许留空
}

// publicIPv6Endpoints 只有 AAAA 记录的查询地址，配合 tcp6 拨号，拿到的一定是 v6。
var publicIPv6Endpoints = []string{
	"https://api6.ipify.org",
	"https://ipv6.icanhazip.com",
	"https://api-ipv6.ip.sb/ip",
}

// DetectPublicIPv6 返回本机公网 IPv6（全局单播），没有或探测失败返回空串。
//
// 用来另给一套 IPv6 链接（订阅、分享、OTun-M 的 url_v6）。主链接与 SERVER_IP 仍用 v4
// （见 DetectPublicIP 的说明），这里拿不到只是不出 IPv6 链接，不影响任何既有功能。
func DetectPublicIPv6(ctx context.Context) string {
	dialer := &net.Dialer{Timeout: publicIPTimeout}
	client := &http.Client{
		Timeout: publicIPTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp6", addr)
			},
		},
	}
	for _, url := range publicIPv6Endpoints {
		if ip := fetchIP(ctx, client, url, true); ip != "" {
			return ip
		}
	}
	return ""
}

// IsPublicIPv6 是不是可以给客户端用的公网 IPv6：全局单播，排除 ULA（fc00::/7）
func IsPublicIPv6(s string) bool {
	ip := net.ParseIP(strings.Trim(s, "[]"))
	if ip == nil || ip.To4() != nil || !ip.IsGlobalUnicast() {
		return false
	}
	return ip[0]&0xfe != 0xfc
}

func fetchIP(ctx context.Context, client *http.Client, url string, wantV6 bool) string {
	reqCtx, cancel := context.WithTimeout(ctx, publicIPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ""
	}
	// 限制读取量：这些端点只返回一个 IP，异常响应不该撑爆内存
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}

	ip := strings.TrimSpace(string(body))
	parsed := net.ParseIP(ip)
	if wantV6 {
		if !IsPublicIPv6(ip) {
			return ""
		}
		return parsed.String()
	}
	if parsed == nil || parsed.To4() == nil {
		return "" // 不是合法 IPv4 就当没拿到
	}
	return parsed.String()
}
