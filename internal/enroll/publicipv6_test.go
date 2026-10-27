package enroll

import "testing"

func TestIsPublicIPv6(t *testing.T) {
	cases := map[string]bool{
		"2604:a880:400:d1:0:5:3fa9:1001": true,
		"[2406:da14:1eb6:b300::1]":       true,
		"fd00::1":                        false, // ULA
		"fe80::1":                        false, // 链路本地
		"::1":                            false,
		"1.2.3.4":                        false,
		"":                               false,
	}
	for in, want := range cases {
		if got := IsPublicIPv6(in); got != want {
			t.Errorf("IsPublicIPv6(%q) = %v, want %v", in, got, want)
		}
	}
}
