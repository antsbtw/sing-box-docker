package enroll

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &NodeFile{
		NodeID:     "byo-7f3a9c2e",
		NodeSecret: "nsk1_abcdef",
		MachineID:  "9c1f",
		APIURL:     "https://portal.example.com",
		PollURL:    "https://portal.example.com/poll",
		EnrolledAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := SaveNodeFile(dir, in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := LoadNodeFile(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out.NodeID != in.NodeID || out.NodeSecret != in.NodeSecret {
		t.Errorf("往返不一致: %+v", out)
	}
	if out.Contract != ContractVersion {
		t.Errorf("contract 应为 %d，得到 %d", ContractVersion, out.Contract)
	}
}

// 文件含 node_secret，权限必须是 0600。
func TestNodeFileIsNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	if err := SaveNodeFile(dir, &NodeFile{NodeID: "n", NodeSecret: "s"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "node.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("权限应为 0600，得到 %o", perm)
	}
}

// 不存在 = 尚未接入，是正常状态，不应报错 ——
// 调用方据此决定走 token 注册。
func TestLoadMissingReturnsNilNil(t *testing.T) {
	nf, err := LoadNodeFile(t.TempDir())
	if err != nil {
		t.Errorf("文件不存在不应报错，得到 %v", err)
	}
	if nf != nil {
		t.Errorf("应返回 nil，得到 %+v", nf)
	}
}

// 文件损坏视同未接入而非死循环：让 agent 重新注册。
func TestLoadCorruptedReportsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNodeFile(dir); err == nil {
		t.Error("损坏的文件应报错")
	}
}

// 缺关键字段的文件不能当作有效凭据。
func TestLoadIncompleteRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node.json"),
		[]byte(`{"contract":1,"node_id":"n"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNodeFile(dir); err == nil {
		t.Error("缺 node_secret 应报错")
	}
}

// 覆盖写不能留下临时文件 —— install.sh 在轮询 node.json 的存在性。
func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		if err := SaveNodeFile(dir, &NodeFile{NodeID: "n", NodeSecret: "s"}); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "node.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("目录应只剩 node.json，得到 %v", names)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveNodeFile(dir); err != nil {
		t.Errorf("删除不存在的文件不应报错: %v", err)
	}
	_ = SaveNodeFile(dir, &NodeFile{NodeID: "n", NodeSecret: "s"})
	if err := RemoveNodeFile(dir); err != nil {
		t.Errorf("Remove: %v", err)
	}
	if nf, _ := LoadNodeFile(dir); nf != nil {
		t.Error("删除后应读不到")
	}
}

// machine_id 必须跨调用稳定，否则后端会把重装当成新机器（契约 §3）。
func TestMachineIDIsStable(t *testing.T) {
	dir := t.TempDir()
	a, err := MachineID(dir)
	if err != nil {
		t.Fatalf("MachineID: %v", err)
	}
	b, err := MachineID(dir)
	if err != nil {
		t.Fatalf("MachineID 第二次: %v", err)
	}
	if a != b {
		t.Errorf("两次调用应一致:\n  %s\n  %s", a, b)
	}
	if len(a) != 64 {
		t.Errorf("应为 64 位 hex，得到 %d 位: %s", len(a), a)
	}
	for _, r := range a {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			t.Fatalf("应为小写 hex，含非法字符 %q", r)
		}
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	// 抖动 ±20%，只验趋势与上限
	if d := Backoff(0); d < 800*time.Millisecond || d > 1300*time.Millisecond {
		t.Errorf("首次退避应约 1s，得到 %v", d)
	}
	if d := Backoff(20); d > 61*time.Second {
		t.Errorf("退避应封顶 60s，得到 %v", d)
	}
	if Backoff(5) <= Backoff(0)/2 {
		t.Error("退避应随尝试次数增长")
	}
}
