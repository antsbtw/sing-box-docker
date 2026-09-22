package enroll

import (
	"testing"
	"time"

	"otun-node-agent/internal/local"
)

type fakeInfo struct{}

func (fakeInfo) ListenPorts() map[string]int { return map[string]int{"vless": 443} }
func (fakeInfo) Secrets() NodeSecrets        { return NodeSecrets{RealitySNI: "www.microsoft.com"} }
func (fakeInfo) SingboxVersion() string      { return "1.10.7" }

func newExec(t *testing.T) *Executor {
	t.Helper()
	store := local.NewStore(t.TempDir(), nil)
	return NewExecutor(store, fakeInfo{}, "v1.11.0", func() error { return nil })
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

// 契约 §6.2：uuid 与 ss_password 必须用后端给的，不能自生成 ——
// 后端要用同样的值拼分享链接。
func TestCreateUserUsesBackendSuppliedIdentity(t *testing.T) {
	e := newExec(t)
	const uid = "0d3f1b2c-1111-4aaa-8bbb-000000000001"

	ack := e.Execute(cmd("c1", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "alice", "ss_password": "pw-from-backend-01",
		"protocols": []any{"vless", "shadowsocks"}, "traffic_limit": float64(0),
	}), time.Now())

	if ack.Result != ResultDone {
		t.Fatalf("期望 done，得到 %s/%s %s", ack.Result, ack.Error, ack.Detail)
	}
	u, ok := e.store.GetUser(uid)
	if !ok {
		t.Fatal("用户未写入 store")
	}
	if u.SSPassword != "pw-from-backend-01" {
		t.Errorf("ss_password 应为后端给的值，得到 %q", u.SSPassword)
	}
}

// 契约 §6.4：回执丢失时后端会重投，必须重放原回执而不是重新执行或报错。
func TestDuplicateCommandReplaysAck(t *testing.T) {
	e := newExec(t)
	c := cmd("dup-1", CmdCreateUser, map[string]any{
		"uuid": "0d3f1b2c-2222-4aaa-8bbb-000000000002",
		"name": "bob", "ss_password": "pw-bob-000000001",
	})

	first := e.Execute(c, time.Now())
	second := e.Execute(c, time.Now())

	if first.Result != ResultDone || second.Result != ResultDone {
		t.Fatalf("两次都应 done：%s / %s", first.Result, second.Result)
	}
	if second.CommandID != first.CommandID {
		t.Error("重放的回执 command_id 应一致")
	}
}

// 契约 §6.4：delete 不存在的 uuid 回 done，不是 failed。
func TestDeleteMissingUserIsDone(t *testing.T) {
	e := newExec(t)
	ack := e.Execute(cmd("d1", CmdDeleteUser,
		map[string]any{"uuid": "no-such-uuid"}), time.Now())

	if ack.Result != ResultDone {
		t.Errorf("删除不存在的用户应回 done，得到 %s/%s", ack.Result, ack.Error)
	}
}

// 契约 §6.4：update 不存在的 uuid 回 failed user_not_found。
func TestUpdateMissingUserFails(t *testing.T) {
	e := newExec(t)
	ack := e.Execute(cmd("u1", CmdUpdateUser,
		map[string]any{"uuid": "no-such-uuid"}), time.Now())

	if ack.Result != ResultFailed || ack.Error != ErrUserNotFound {
		t.Errorf("期望 failed/user_not_found，得到 %s/%s", ack.Result, ack.Error)
	}
}

// 契约 §6.1：过期指令不执行，且以**服务端时间**判定（不信本机时钟）。
func TestExpiredCommandIsNotExecuted(t *testing.T) {
	e := newExec(t)
	const uid = "0d3f1b2c-3333-4aaa-8bbb-000000000003"

	c := cmd("e1", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "carol", "ss_password": "pw-carol-0000001",
	})
	c.ExpiresAt = time.Now().Add(-time.Minute)

	ack := e.Execute(c, time.Now())
	if ack.Result != ResultFailed || ack.Error != ErrExpired {
		t.Errorf("期望 failed/expired，得到 %s/%s", ack.Result, ack.Error)
	}
	if _, ok := e.store.GetUser(uid); ok {
		t.Error("过期指令不应产生副作用")
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

// 同 uuid 但字段不同 → failed user_exists，不能静默覆盖用户数据。
func TestConflictingCreateFails(t *testing.T) {
	e := newExec(t)
	const uid = "0d3f1b2c-4444-4aaa-8bbb-000000000004"

	e.Execute(cmd("k1", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "dave", "ss_password": "pw-dave-00000001",
	}), time.Now())

	ack := e.Execute(cmd("k2", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "dave", "ss_password": "DIFFERENT-PASSWORD",
	}), time.Now())

	if ack.Result != ResultFailed || ack.Error != ErrUserExists {
		t.Errorf("期望 failed/user_exists，得到 %s/%s", ack.Result, ack.Error)
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
	if ack.Data["agent_version"] != "v1.11.0" {
		t.Errorf("agent_version 应回报注入的版本，得到 %v", ack.Data["agent_version"])
	}
}

// M-1：expire_at 三态。最危险的是"已过去的时刻"——
// 换算成天数会得到负数、夹成 0，而 0 在 Store 里是"永不过期"，
// 于是"立即到期"变成"永久有效"。
func TestUpdateUserExpireAtSemantics(t *testing.T) {
	const uid = "0d3f1b2c-5555-4aaa-8bbb-000000000005"

	setup := func(t *testing.T) *Executor {
		e := newExec(t)
		e.Execute(cmd("c", CmdCreateUser, map[string]any{
			"uuid": uid, "name": "eve", "ss_password": "pw-eve-000000001",
		}), time.Now())
		return e
	}

	t.Run("过去的时间必须真的过期", func(t *testing.T) {
		e := setup(t)
		past := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
		ack := e.Execute(cmd("u", CmdUpdateUser, map[string]any{
			"uuid": uid, "expire_at": past,
		}), time.Now())
		if ack.Result != ResultDone {
			t.Fatalf("应 done，得到 %s/%s", ack.Result, ack.Detail)
		}
		u, _ := e.store.GetUser(uid)
		if u.ExpireAt == nil {
			t.Fatal("过去的 expire_at 被当成了「永不过期」—— 这正是 M-1 的 bug")
		}
		if !u.ExpireAt.Before(time.Now()) {
			t.Errorf("到期时间应在过去，得到 %v", u.ExpireAt)
		}
	})

	t.Run("显式 null 表示永不过期", func(t *testing.T) {
		e := setup(t)
		future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
		e.Execute(cmd("u1", CmdUpdateUser, map[string]any{
			"uuid": uid, "expire_at": future,
		}), time.Now())

		e.Execute(cmd("u2", CmdUpdateUser, map[string]any{
			"uuid": uid, "expire_at": nil,
		}), time.Now())

		u, _ := e.store.GetUser(uid)
		if u.ExpireAt != nil {
			t.Errorf("expire_at=null 应清除到期时间，得到 %v", u.ExpireAt)
		}
	})

	t.Run("字段缺席不改动", func(t *testing.T) {
		e := setup(t)
		future := time.Now().Add(72 * time.Hour)
		e.Execute(cmd("u1", CmdUpdateUser, map[string]any{
			"uuid": uid, "expire_at": future.UTC().Format(time.RFC3339),
		}), time.Now())

		e.Execute(cmd("u2", CmdUpdateUser, map[string]any{
			"uuid": uid, "name": "eve2",
		}), time.Now())

		u, _ := e.store.GetUser(uid)
		if u.ExpireAt == nil {
			t.Fatal("未给 expire_at 时不应清除原有到期时间")
		}
		if d := u.ExpireAt.Sub(future); d > time.Minute || d < -time.Minute {
			t.Errorf("到期时间被改动了：%v vs %v", u.ExpireAt, future)
		}
	})
}

// M-2：同 uuid 同密码的重复投递应应用其余字段（真 upsert），
// 而不是只比 name 就回 done 把变更丢掉。
func TestCreateUserAppliesFieldsOnRepeat(t *testing.T) {
	e := newExec(t)
	const uid = "0d3f1b2c-6666-4aaa-8bbb-000000000006"

	e.Execute(cmd("c1", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "frank", "ss_password": "pw-frank-0000001",
		"traffic_limit": float64(1000),
	}), time.Now())

	// 后端修正后重发：同 uuid 同密码，但限额变了
	ack := e.Execute(cmd("c2", CmdCreateUser, map[string]any{
		"uuid": uid, "name": "frank", "ss_password": "pw-frank-0000001",
		"traffic_limit": float64(9999),
	}), time.Now())

	if ack.Result != ResultDone {
		t.Fatalf("应 done，得到 %s/%s", ack.Result, ack.Error)
	}
	u, _ := e.store.GetUser(uid)
	if u.TrafficLimit != 9999 {
		t.Errorf("重复投递应应用新的 traffic_limit，得到 %d", u.TrafficLimit)
	}
}
