package enroll

import (
	"encoding/json"
	"strings"
	"testing"
)

// 后端按「owner_keys 为空数组」判「没有任何设备被授权」，
// App 据此把节点显示成「可见但进不去」。
//
// ⚠️ 这条测试盯的是 JSON 里必须出现 "owner_keys":[] ——
// 一旦有人给字段加上 omitempty，或让 provider 返回 nil 编成 null，
// 后端就会保留上一次的旧列表：用户刚删掉的钥匙还显示在 App 上，
// 看起来像「删不掉」。
func TestOwnerKeysAlwaysSerializedAsArray(t *testing.T) {
	cases := []struct {
		name     string
		provider func() []map[string]string
	}{
		{"未注入 provider", nil},
		{"provider 返回 nil", func() []map[string]string { return nil }},
		{"provider 返回空切片", func() []map[string]string { return []map[string]string{} }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &Runner{OwnerKeys: c.provider}
			req := PollRequest{Status: PollStatus{OwnerKeys: r.ownerKeys()}}

			data, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"owner_keys":[]`) {
				t.Errorf("必须上报空数组而非 null 或省略，实际：%s", data)
			}
		})
	}
}

// 有钥匙时如实上报指纹与名字。
func TestOwnerKeysReportsFingerprints(t *testing.T) {
	r := &Runner{OwnerKeys: func() []map[string]string {
		return []map[string]string{{"fp": "SHA256:abc", "name": "iPhone 15"}}
	}}

	data, err := json.Marshal(PollStatus{OwnerKeys: r.ownerKeys()})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "SHA256:abc") || !strings.Contains(s, "iPhone 15") {
		t.Errorf("指纹与名字应被上报，实际：%s", s)
	}
}

// ⚠️ 公钥本体不出机器 —— 上报里绝不能出现 pub 字段。
// 指纹泄露无所谓，公钥本体是「这把钥匙长什么样」的完整信息。
func TestOwnerKeysNeverCarryPublicKey(t *testing.T) {
	r := &Runner{OwnerKeys: func() []map[string]string {
		return []map[string]string{{"fp": "SHA256:abc", "name": "phone"}}
	}}

	data, _ := json.Marshal(PollStatus{OwnerKeys: r.ownerKeys()})
	if strings.Contains(string(data), "ssh-ed25519") || strings.Contains(string(data), `"pub"`) {
		t.Errorf("上报不得包含公钥本体：%s", data)
	}
}

// 每次 poll 都重新问一次 —— 用户刚 obox-key rm，下一次就该看到变化，
// 不能缓存在启动时读到的那一份。
func TestOwnerKeysReadFreshEachPoll(t *testing.T) {
	calls := 0
	r := &Runner{OwnerKeys: func() []map[string]string {
		calls++
		return []map[string]string{{"fp": "SHA256:x", "name": "k"}}
	}}

	r.ownerKeys()
	r.ownerKeys()
	if calls != 2 {
		t.Errorf("每次都应重新读取，实际调用 %d 次", calls)
	}
}
