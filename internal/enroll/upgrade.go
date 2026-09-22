package enroll

// agent 自升级。接线契约 §6.2 upgrade_agent，复核 §7.4。
//
// 升级是所有指令里唯一会杀死自己的那一个，顺序错了就没有第二次机会：
// 回执必须先送达后端，才能退出 —— 否则重启后回执丢失，后端重投，
// 而新版本已经在跑了。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// 升级后若新版本连不上后端，回滚到它。
	// 没有这个文件就没有退路：一个坏版本会让全部节点失联且无人能远程修。
	prevSuffix = ".prev"

	upgradeDownloadTimeout = 5 * time.Minute
)

// UpgradePlan 是 upgrade_agent 的 payload。
type UpgradePlan struct {
	ReleaseTag string            `json:"release_tag"`
	SHA256     map[string]string `json:"sha256"` // 文件名 → 校验和
}

// assetName 返回本机架构对应的 agent 资产名。
func assetName() string {
	return fmt.Sprintf("agent-linux-%s", runtime.GOARCH)
}

// PrepareUpgrade 下载、校验并就位新二进制，但**不退出进程**。
//
// 返回 nil 表示新版本已写到 exePath，旧版本已备份为 exePath.prev。
// 调用方必须在回执确认送达之后才退出（契约 §7.4）。
func PrepareUpgrade(plan UpgradePlan, repo, exePath string) error {
	if plan.ReleaseTag == "" {
		return fmt.Errorf("release_tag 为空")
	}

	name := assetName()
	want, ok := plan.SHA256[name]
	if !ok || want == "" {
		// 没有本机架构的校验和就不能升 —— 宁可停在旧版本，
		// 也不装一个来路不明的二进制。
		return fmt.Errorf("payload 缺少 %s 的 sha256", name)
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, plan.ReleaseTag, name)
	return prepareFrom(url, want, exePath)
}

// prepareUpgradeVia 供测试注入下载地址，逻辑与 PrepareUpgrade 一致。
func prepareUpgradeVia(baseURL string, plan UpgradePlan, repo, exePath string) error {
	name := assetName()
	want, ok := plan.SHA256[name]
	if !ok || want == "" {
		return fmt.Errorf("payload 缺少 %s 的 sha256", name)
	}
	return prepareFrom(baseURL+"/"+name, want, exePath)
}

func prepareFrom(url, want, exePath string) error {

	tmp := exePath + ".new"
	defer os.Remove(tmp) // rename 成功后是 no-op

	if err := downloadTo(url, tmp); err != nil {
		return fmt.Errorf("下载 %s: %w", url, err)
	}

	got, err := fileSHA256(tmp)
	if err != nil {
		return fmt.Errorf("计算校验和: %w", err)
	}
	if got != want {
		return fmt.Errorf("校验和不匹配：期望 %s，实际 %s", want, got)
	}

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// 先备份旧版本再覆盖 —— 回滚的唯一依据
	prev := exePath + prevSuffix
	_ = os.Remove(prev)
	if err := os.Rename(exePath, prev); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("备份旧版本: %w", err)
	}

	if err := os.Rename(tmp, exePath); err != nil {
		// 覆盖失败就把旧版本放回去，不能留下一个没有可执行文件的节点
		if rbErr := os.Rename(prev, exePath); rbErr != nil {
			return fmt.Errorf("覆盖失败(%v)且回滚失败(%v)：节点已无可执行文件", err, rbErr)
		}
		return fmt.Errorf("覆盖新版本: %w", err)
	}
	return nil
}

// Rollback 把备份的旧版本换回来。
//
// 新版本连续多次连不上后端时调用 —— 没有这条路径，一个坏版本
// 会让全部节点失联，而且没人能远程修（复核 §7.4-2）。
func Rollback(exePath string) error {
	prev := exePath + prevSuffix
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("无可回滚的版本: %w", err)
	}
	if err := os.Rename(prev, exePath); err != nil {
		return fmt.Errorf("回滚失败: %w", err)
	}
	return nil
}

// HasPrevVersion 是否存在可回滚的版本。
func HasPrevVersion(exePath string) bool {
	_, err := os.Stat(exePath + prevSuffix)
	return err == nil
}

// ClearPrevVersion 升级确认成功后删除备份。
func ClearPrevVersion(exePath string) {
	_ = os.Remove(exePath + prevSuffix)
}

func downloadTo(url, dest string) error {
	client := &http.Client{Timeout: upgradeDownloadTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Sync()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
