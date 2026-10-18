package singbox

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// residential.json 存在时追加第二个 -c，主配置仍排第一（reapStrays 靠它匹配）。
func TestArgsIncludeResidentialWhenPresent(t *testing.T) {
	bin, cfg := fakeBinary(t, "sleep 60")
	m := NewManager(bin, cfg)

	if got, want := m.args("run"), []string{"run", "-c", cfg}; !reflect.DeepEqual(got, want) {
		t.Errorf("无住宅出口时：got %v, want %v", got, want)
	}

	res := filepath.Join(filepath.Dir(cfg), ResidentialFile)
	if err := os.WriteFile(res, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := m.args("check"), []string{"check", "-c", cfg, "-c", res}; !reflect.DeepEqual(got, want) {
		t.Errorf("有住宅出口时：got %v, want %v", got, want)
	}
}

// ⚠️ 住宅出口填错不能拖垮原有节点：带着 residential.json 起不来时，
// agent 要把它挪成 .failed，不带它把 sing-box 拉起来。
//
// 终端中途断开、App 没机会回滚时，只有这条兜底。
func TestResidentialDisabledAfterRepeatedFailures(t *testing.T) {
	// 带住宅出口就立刻失败，不带就正常跑 —— 模拟住宅配置填错
	bin, cfg := fakeBinary(t, `case "$*" in *residential.json*) exit 1;; esac
sleep 60`)
	res := filepath.Join(filepath.Dir(cfg), ResidentialFile)
	if err := os.WriteFile(res, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(bin, cfg)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	// 第 1 次失败退避 1s 重试，第 2 次失败触发停用，再 1s 后不带它启动
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(res + ".failed"); err == nil && m.IsRunning() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if _, err := os.Stat(res + ".failed"); err != nil {
		t.Fatalf("residential.json 应被改名为 .failed：%v", err)
	}
	if _, err := os.Stat(res); !os.IsNotExist(err) {
		t.Error("residential.json 应已不存在")
	}
	if !m.IsRunning() {
		t.Error("停用住宅出口后 sing-box 应恢复运行")
	}
	if m.GaveUp() {
		t.Error("不该放弃 —— 去掉住宅出口后节点是好的")
	}
}

// 没有 residential.json 时，失败不应凭空产生 .failed 文件。
func TestNoResidentialNoFailedFile(t *testing.T) {
	bin, cfg := fakeBinary(t, "exit 1")
	m := NewManager(bin, cfg)
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3500 * time.Millisecond)
	m.Stop()

	res := filepath.Join(filepath.Dir(cfg), ResidentialFile)
	if _, err := os.Stat(res + ".failed"); !os.IsNotExist(err) {
		t.Error("没有住宅出口时不该出现 .failed")
	}
}
