package api

import "testing"

// 分享链接里的 host:port：IPv6 必须加方括号，IPv4 原样
func TestHostPortBracketsIPv6(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":         "1.2.3.4:443",
		"2001:db8::1":     "[2001:db8::1]:443",
		"[2001:db8::1]":   "[2001:db8::1]:443",
		"vpn.example.com": "vpn.example.com:443",
	}
	for ip, want := range cases {
		s := &LocalAPIServer{nodeConfig: &NodeConfig{ServerIP: ip}}
		if got := s.hostPort(443); got != want {
			t.Errorf("hostPort(%q) = %q, want %q", ip, got, want)
		}
	}
}
