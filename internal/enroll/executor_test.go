package enroll

import (
	"context"
	"testing"
	"time"
)

type fakeInfo struct{}

func (fakeInfo) ListenPorts() map[string]int { return map[string]int{"vless": 443} }
func (fakeInfo) Secrets() NodeSecrets        { return NodeSecrets{RealitySNI: "www.apple.com"} }
func (fakeInfo) SingboxVersion() string      { return "1.10.7" }

func newExec(t *testing.T) *Executor {
	t.Helper()
	return NewExecutor(fakeInfo{}, "v1.14.0", func() error { return nil })
}

func cmd(id, typ string, payload map[string]any) Command {
	return Command{
		CommandID: id,
		Type:      typ,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(10 * time.Minute),
		Payload:   payload,
	}
}

// 契约 §6.4：重复投递重放原回执，不重新执行。
func TestDuplicateCommandReplaysAck(t *testing.T) {
	e := newExec(t)
	c := cmd("dup-1", CmdPing, nil)

	first := e.Execute(c, time.Now())
	second := e.Execute(c, time.Now())

	if first.Result != ResultDone || second.Result != ResultDone {
		t.Fatalf("两次都应 done：%s / %s", first.Result, second.Result)
	}
	if second.CommandID != first.CommandID {
		t.Error("重放的回执 command_id 应一致")
	}
}

func TestExpiredCommandIsNotExecuted(t *testing.T) {
	e := newExec(t)
	c := cmd("e1", CmdReload, nil)
	c.ExpiresAt = time.Now().Add(-time.Minute)

	ack := e.Execute(c, time.Now())
	if ack.Result != ResultFailed || ack.Error != ErrExpired {
		t.Errorf("期望 failed/expired，得到 %s/%s", ack.Result, ack.Error)
	}
}

// 契约 §11：未知指令回 unsupported_type，后端据此降级。
func TestUnknownCommandType(t *testing.T) {
	e := newExec(t)
	ack := e.Execute(cmd("x1", "teleport_node", nil), time.Now())
	if ack.Result != ResultFailed || ack.Error != ErrUnsupportedType {
		t.Errorf("期望 failed/unsupported_type，得到 %s/%s", ack.Result, ack.Error)
	}
}

// ⚠️ v1.14.0:四条用户指令已删除(09 §5.3、R-18)。
//
// 这是「后端不能在用户机器上加 VPN 用户」的回归测试 —— 谁把它们加回来,这里就会红。
func TestUserCommandsAreRejected(t *testing.T) {
	e := newExec(t)
	for _, typ := range []string{"create_user", "update_user", "delete_user", "reset_user_traffic"} {
		ack := e.Execute(cmd("u-"+typ, typ, map[string]any{
			"uuid": "0d3f1b2c-1111-4aaa-8bbb-000000000001", "name": "x", "ss_password": "p",
		}), time.Now())
		if ack.Result != ResultFailed || ack.Error != ErrUnsupportedType {
			t.Errorf("%s 应被拒(unsupported_type)，得到 %s/%s", typ, ack.Result, ack.Error)
		}
	}
}

func TestPingAndGetConfig(t *testing.T) {
	e := newExec(t)

	if ack := e.Execute(cmd("p1", CmdPing, nil), time.Now()); ack.Result != ResultDone {
		t.Errorf("ping 应 done，得到 %s", ack.Result)
	}

	ack := e.Execute(cmd("g1", CmdGetConfig, nil), time.Now())
	if ack.Result != ResultDone {
		t.Fatalf("get_config 应 done，得到 %s", ack.Result)
	}
	if ack.Data["agent_version"] != "v1.14.0" {
		t.Errorf("agent_version 应回报注入的版本，得到 %v", ack.Data["agent_version"])
	}
}

// ── open_tunnel 只许连回 api-url 的主机 ──────────────────────

type fakeTunnel struct{ opened []string }

func (f *fakeTunnel) Open(_ context.Context, _ string, url string) error {
	f.opened = append(f.opened, url)
	return nil
}

func TestOpenTunnelOnlyToAPIHost(t *testing.T) {
	e := newExec(t)
	ft := &fakeTunnel{}
	e.EnableTunnel(ft, "saasapi.situstechnologies.com")

	ok := e.Execute(cmd("t1", CmdOpenTunnel, map[string]any{
		"session_id": "s1", "tunnel_url": "wss://saasapi.situstechnologies.com/api/v1/obox/tunnel/s1",
	}), time.Now())
	if ok.Result != ResultDone {
		t.Fatalf("同主机应放行，得到 %s/%s", ok.Result, ok.Error)
	}

	for i, bad := range []string{
		"wss://evil.example.com/tunnel",
		"wss://saasapi.situstechnologies.com.evil.example.com/t",
		"not a url",
	} {
		ack := e.Execute(cmd("t-bad-"+string(rune('a'+i)), CmdOpenTunnel, map[string]any{
			"session_id": "s", "tunnel_url": bad,
		}), time.Now())
		if ack.Result != ResultFailed {
			t.Errorf("%q 应被拒", bad)
		}
	}
	if len(ft.opened) != 1 {
		t.Errorf("只应真正连过 1 次，实际 %v", ft.opened)
	}
}

// ── upgrade_agent ─────────────────────────────────────────

// 非托管模式不调 EnableUpgrade:指令一律拒收。
func TestUpgradeRejectedWhenNotEnabled(t *testing.T) {
	e := newExec(t)
	ack := e.Execute(cmd("up-0", CmdUpgradeAgent, map[string]any{
		"release_tag": "v1.15.0", "sha256": map[string]any{"agent-linux-amd64": "x"},
	}), time.Now())
	if ack.Result != ResultFailed || ack.Error != ErrUnsupportedType {
		t.Errorf("未启用升级时应 unsupported_type，得到 %s/%s", ack.Result, ack.Error)
	}
}

// 只升不降,拒绝浮动名 —— 在下载之前就拦下。
func TestUpgradeRejectsDowngradeAndFloatingTags(t *testing.T) {
	e := newExec(t)
	e.EnableUpgrade(&UpgradeConfig{Repo: "antsbtw/sing-box-docker", ExePath: t.TempDir() + "/agent"})

	for i, tag := range []string{"v1.13.1", "v1.9.99", "latest", "v1.15.0-rc1", ""} {
		ack := e.Execute(cmd("up-bad-"+string(rune('a'+i)), CmdUpgradeAgent, map[string]any{
			"release_tag": tag, "sha256": map[string]any{"agent-linux-amd64": "x", "agent-linux-arm64": "x"},
		}), time.Now())
		// 必须是版本检查拦下的(invalid_payload),而不是下载失败(internal)——
		// 否则删掉检查、换一个真能下载的旧版本,这条测试也照样通过。
		if ack.Result != ResultFailed || ack.Error != ErrInvalidPayload {
			t.Errorf("目标 %q 应被版本检查拒绝，得到 %s/%s", tag, ack.Result, ack.Error)
		}
		if e.PendingExit() {
			t.Fatalf("目标 %q 不应进入待重启状态", tag)
		}
	}

	// 同版本:幂等 done,不下载
	same := e.Execute(cmd("up-same", CmdUpgradeAgent, map[string]any{"release_tag": "v1.14.0"}), time.Now())
	if same.Result != ResultDone {
		t.Errorf("同版本应幂等 done，得到 %s/%s", same.Result, same.Error)
	}
}

func TestIsNewerRelease(t *testing.T) {
	cases := []struct {
		target, current string
		want            bool
	}{
		{"v1.15.0", "v1.14.0", true},
		{"1.14.1", "v1.14.0", true},
		{"v2.0.0", "v1.99.99", true},
		{"v1.14.0", "v1.14.0", false},
		{"v1.13.9", "v1.14.0", false},
		{"latest", "v1.14.0", false},
		{"v1.15", "v1.14.0", false},
		{"v1.15.0-rc1", "v1.14.0", false},
		{"v1.15.0", "dev", false},
	}
	for _, c := range cases {
		if got := isNewerRelease(c.target, c.current); got != c.want {
			t.Errorf("isNewerRelease(%q, %q) = %v，期望 %v", c.target, c.current, got, c.want)
		}
	}
}
