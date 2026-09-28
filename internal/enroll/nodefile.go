package enroll

// node.json 的读写。接线契约 §2.3。
//
// 这个文件是"是否已接入"的唯一判据：存在即视为已接入，启动时优先用它，
// 不再读 OTUN_ENROLL_TOKEN。install.sh 也靠轮询它来判断注册是否成功，
// 据此决定要不要从 systemd 单元里删掉一次性 token（契约 §2.4）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ContractVersion 本 agent 实现的契约版本。
// 后端只加字段不删字段，agent 忽略未知字段（契约 §11）。
const ContractVersion = 1

// NodeFile 是 /opt/otun-agent/data/node.json 的内容。
type NodeFile struct {
	Contract   int       `json:"contract"`
	NodeID     string    `json:"node_id"`
	NodeSecret string    `json:"node_secret"`
	MachineID  string    `json:"machine_id"`
	APIURL     string    `json:"api_url"`
	PollURL    string    `json:"poll_url"`
	EnrolledAt time.Time `json:"enrolled_at"`
}

func nodeFilePath(dataDir string) string {
	return filepath.Join(dataDir, "node.json")
}

// LoadNodeFile 读取已有的接入信息。不存在返回 (nil, nil) —— 这是正常状态，
// 表示尚未接入，调用方应转而走 token 注册。
func LoadNodeFile(dataDir string) (*NodeFile, error) {
	b, err := os.ReadFile(nodeFilePath(dataDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read node.json: %w", err)
	}

	var nf NodeFile
	if err := json.Unmarshal(b, &nf); err != nil {
		// 文件损坏（断电写坏、人为编辑）视同未接入，让 agent 走重新注册，
		// 而不是抱着一个读不出来的文件反复失败。
		return nil, fmt.Errorf("parse node.json: %w", err)
	}
	if nf.NodeID == "" || nf.NodeSecret == "" {
		return nil, fmt.Errorf("node.json missing node_id or node_secret")
	}
	return &nf, nil
}

// SaveNodeFile 原子写入。
//
// ⚠️ 必须原子：install.sh 在轮询这个文件的存在性，看到就删 systemd 里的 token。
// 若写到一半被读到（或断电留下半个文件），token 已删而 secret 不完整，
// agent 重启后既无 token 又无 secret —— 永远注册不上（契约 §2.4 第 4 条的成因）。
func SaveNodeFile(dataDir string, nf *NodeFile) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	nf.Contract = ContractVersion
	b, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal node.json: %w", err)
	}

	final := nodeFilePath(dataDir)
	tmp, err := os.CreateTemp(dataDir, ".node.json.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功后这里是 no-op

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	// 0600：内含 node_secret
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	// 先落盘再 rename，否则崩溃时可能 rename 了一个空文件
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename node.json: %w", err)
	}
	return nil
}

// RemoveNodeFile 删除接入信息。
// 用于收到 node_revoked / node_not_found 后清理本地凭据（契约 §5.3）。
func RemoveNodeFile(dataDir string) error {
	err := os.Remove(nodeFilePath(dataDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
