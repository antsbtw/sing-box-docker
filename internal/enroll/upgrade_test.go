package enroll

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// 升级成功后：新版本就位、旧版本留作回滚依据。
func TestPrepareUpgradeReplacesAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	writeExe(t, exe, "OLD-BINARY")

	const newContent = "NEW-BINARY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(newContent))
	}))
	defer srv.Close()

	// PrepareUpgrade 按 github.com/<repo>/releases/... 拼 URL，
	// 这里用测试服务器的 host 替代
	repo := srv.Listener.Addr().String() + "/x"
	plan := UpgradePlan{
		ReleaseTag: "v1.12.0",
		SHA256:     map[string]string{assetName(): sha256Of(newContent)},
	}
	if err := prepareUpgradeVia(srv.URL, plan, repo, exe); err != nil {
		t.Fatalf("PrepareUpgrade: %v", err)
	}

	got, _ := os.ReadFile(exe)
	if string(got) != newContent {
		t.Errorf("新版本未就位，内容为 %q", got)
	}
	prev, err := os.ReadFile(exe + prevSuffix)
	if err != nil || string(prev) != "OLD-BINARY" {
		t.Errorf("旧版本应备份为 .prev，得到 %q / %v", prev, err)
	}
	if !HasPrevVersion(exe) {
		t.Error("HasPrevVersion 应为 true")
	}
}

// 校验和不匹配必须拒绝，且**不能动**现有二进制 ——
// 宁可停在旧版本，也不装来路不明的东西。
func TestPrepareUpgradeRejectsBadChecksum(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	writeExe(t, exe, "OLD-BINARY")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("TAMPERED"))
	}))
	defer srv.Close()

	plan := UpgradePlan{
		ReleaseTag: "v1.12.0",
		SHA256:     map[string]string{assetName(): sha256Of("EXPECTED")},
	}
	err := prepareUpgradeVia(srv.URL, plan, "x/y", exe)
	if err == nil {
		t.Fatal("校验和不符应报错")
	}

	got, _ := os.ReadFile(exe)
	if string(got) != "OLD-BINARY" {
		t.Errorf("校验失败时不应改动现有二进制，内容变成了 %q", got)
	}
	if HasPrevVersion(exe) {
		t.Error("校验失败时不应产生 .prev 备份")
	}
}

// 缺本机架构的校验和 → 拒绝升级。
func TestPrepareUpgradeRequiresChecksumForThisArch(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	writeExe(t, exe, "OLD")

	plan := UpgradePlan{
		ReleaseTag: "v1.12.0",
		SHA256:     map[string]string{"agent-linux-someotherarch": "abc"},
	}
	if err := PrepareUpgrade(plan, "x/y", exe); err == nil {
		t.Fatal("缺本机架构校验和应拒绝")
	}
}

// 回滚把旧版本换回来 —— 坏版本的唯一退路。
func TestRollbackRestoresPrevious(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	writeExe(t, exe, "BROKEN-NEW")
	writeExe(t, exe+prevSuffix, "GOOD-OLD")

	if err := Rollback(exe); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != "GOOD-OLD" {
		t.Errorf("回滚后应是旧版本，得到 %q", got)
	}
	if HasPrevVersion(exe) {
		t.Error("回滚后 .prev 应已消耗")
	}
}

func TestRollbackWithoutBackupFails(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agent")
	writeExe(t, exe, "ONLY")
	if err := Rollback(exe); err == nil {
		t.Error("无备份时回滚应报错")
	}
}

func TestClearPrevVersion(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agent")
	writeExe(t, exe, "CUR")
	writeExe(t, exe+prevSuffix, "OLD")

	ClearPrevVersion(exe)
	if HasPrevVersion(exe) {
		t.Error("ClearPrevVersion 后备份应已删除")
	}
}

func TestAssetNameMatchesArch(t *testing.T) {
	want := fmt.Sprintf("agent-linux-%s", runtime.GOARCH)
	if got := assetName(); got != want {
		t.Errorf("assetName()=%q，期望 %q", got, want)
	}
}
