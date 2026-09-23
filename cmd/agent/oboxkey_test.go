package main

import (
	"os"
	"strings"
	"testing"
)

// obox-key 有两种调用方式，参数切片的起点不同。
// 搞错了会把子命令算进参数里 —— 提示会变成
// "sudo obox-key obox-key list"（第一版就是这么错的，真机上被看到）。
func TestOboxKeyArgsForBothInvocations(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"软链调用", []string{"/usr/local/bin/obox-key", "list"}, "list"},
		{"软链带参数", []string{"/usr/local/bin/obox-key", "rm", "SHA256:x"}, "rm SHA256:x"},
		{"子命令调用", []string{"/opt/otun-agent/agent", "obox-key", "list"}, "list"},
		{"子命令带参数", []string{"/opt/otun-agent/agent", "obox-key", "add", "key"}, "add key"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.Args = c.argv
			got := strings.Join(oboxKeyArgs(), " ")
			if got != c.want {
				t.Errorf("参数取错：得到 %q，应为 %q", got, c.want)
			}
		})
	}
}

// 两种调用方式都要被认出来，否则会当成 agent 主程序跑起来。
func TestIsOboxKeyInvocation(t *testing.T) {
	orig := os.Args
	defer func() { os.Args = orig }()

	yes := [][]string{
		{"/usr/local/bin/obox-key", "list"},
		{"obox-key"},
		{"/opt/otun-agent/agent", "obox-key", "list"},
	}
	for _, argv := range yes {
		os.Args = argv
		if !isOboxKeyInvocation() {
			t.Errorf("应认作 obox-key 调用：%v", argv)
		}
	}

	no := [][]string{
		{"/opt/otun-agent/agent"},
		{"/opt/otun-agent/agent", "--version"},
	}
	for _, argv := range no {
		os.Args = argv
		if isOboxKeyInvocation() {
			t.Errorf("不该认作 obox-key 调用：%v", argv)
		}
	}
}
