package tunnel

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// 设备持钥（owner key）方案的授权列表。
//
// 核心不变量：**这个文件的唯一写入者是本机**。
// 后端没有任何接口能增删钥匙 —— 如果做成「App 调后端 → 后端下指令加钥匙」，
// 后门就回来了。能改它的只有能在这台机器上拿到 root 的人：
// 云控制台、22 端口 SSH、经隧道进来的终端，三条路等价。
//
// 设计文档：OBOX_TUNNEL_OWNER_KEY_DESIGN.md §3.1、§3.3

const (
	authKeysFileName = "authorized_keys.json"
	authKeysVersion  = 1
)

// AuthorizedKey 是一把被授权的设备公钥。
type AuthorizedKey struct {
	// Pub 是 OpenSSH 格式的公钥行，例如 "ssh-ed25519 AAAA... iPhone 15"
	Pub string `json:"pub"`
	// Name 供展示，通常是设备名
	Name string `json:"name"`
	// FP 是 SHA256:<base64> 指纹，与 OpenSSH `ssh-keygen -l` 一致。
	// 仅用于展示与比对，**不能用于认证** —— 认证一律比对公钥本体。
	FP      string `json:"fp"`
	AddedAt string `json:"added_at"`
	// AddedBy 记录来源：enroll / local / session:<fp>。
	// 只用于事后追溯，不做门槛 —— root 本来就能直接改文件。
	AddedBy string `json:"added_by"`
}

type authKeysFile struct {
	V    int             `json:"v"`
	Keys []AuthorizedKey `json:"keys"`
}

// AuthKeyStore 读写授权列表。并发安全。
type AuthKeyStore struct {
	path string
	mu   sync.RWMutex
}

func NewAuthKeyStore(dataDir string) *AuthKeyStore {
	return &AuthKeyStore{path: filepath.Join(dataDir, authKeysFileName)}
}

// Fingerprint 算一把公钥的 SHA256 指纹。
func Fingerprint(pub ssh.PublicKey) string {
	return ssh.FingerprintSHA256(pub)
}

// ParseAuthorizedKey 解析一行 OpenSSH 公钥，返回公钥、注释（设备名）与指纹。
func ParseAuthorizedKey(line string) (ssh.PublicKey, string, string, error) {
	pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, "", "", fmt.Errorf("公钥格式不对：%w", err)
	}
	return pub, comment, Fingerprint(pub), nil
}

// List 返回当前授权的全部钥匙。文件不存在时返回空列表，不报错 ——
// 「没有任何钥匙」是一个合法状态（无 --owner-key 装出来的节点）。
func (s *AuthKeyStore) List() ([]AuthorizedKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.listLocked()
}

func (s *AuthKeyStore) listLocked() ([]AuthorizedKey, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f authKeysFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s 解析失败：%w", authKeysFileName, err)
	}
	return f.Keys, nil
}

// Authorized 判断一把公钥是否在列表里。
//
// ⚠️ 比对的是**公钥本体**，不是指纹字符串 —— 指纹只用于展示。
func (s *AuthKeyStore) Authorized(pub ssh.PublicKey) (AuthorizedKey, bool) {
	keys, err := s.List()
	if err != nil {
		return AuthorizedKey{}, false
	}
	want := pub.Marshal()
	for _, k := range keys {
		parsed, _, _, err := ParseAuthorizedKey(k.Pub)
		if err != nil {
			continue
		}
		if subtle.ConstantTimeCompare(parsed.Marshal(), want) == 1 {
			return k, true
		}
	}
	return AuthorizedKey{}, false
}

// Reset 把列表重置为仅这一把。install.sh --owner-key 用它：
// 重装即「从这台设备重新掌控」。
func (s *AuthKeyStore) Reset(line, addedBy string) (AuthorizedKey, error) {
	_, comment, fp, err := ParseAuthorizedKey(line)
	if err != nil {
		return AuthorizedKey{}, err
	}
	k := AuthorizedKey{
		Pub: strings.TrimSpace(line), Name: comment, FP: fp,
		AddedAt: time.Now().UTC().Format(time.RFC3339), AddedBy: addedBy,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return k, s.writeLocked([]AuthorizedKey{k})
}

// Add 追加一把。已存在（同指纹）则视为成功，不重复添加。
func (s *AuthKeyStore) Add(line, addedBy string) (AuthorizedKey, error) {
	_, comment, fp, err := ParseAuthorizedKey(line)
	if err != nil {
		return AuthorizedKey{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	keys, err := s.listLocked()
	if err != nil {
		return AuthorizedKey{}, err
	}
	for _, k := range keys {
		if k.FP == fp {
			return k, nil // 幂等
		}
	}

	k := AuthorizedKey{
		Pub: strings.TrimSpace(line), Name: comment, FP: fp,
		AddedAt: time.Now().UTC().Format(time.RFC3339), AddedBy: addedBy,
	}
	return k, s.writeLocked(append(keys, k))
}

// Remove 按指纹删除一把。
//
// ⚠️ **不允许删到列表为空** —— 那会让这台机器再也进不去，
// 只能去云控制台重装。要清空只能走重装（Reset）。
func (s *AuthKeyStore) Remove(fp string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	keys, err := s.listLocked()
	if err != nil {
		return err
	}

	var kept []AuthorizedKey
	found := false
	for _, k := range keys {
		if k.FP == fp {
			found = true
			continue
		}
		kept = append(kept, k)
	}
	if !found {
		return fmt.Errorf("没有指纹为 %s 的钥匙", fp)
	}
	if len(kept) == 0 {
		return fmt.Errorf("不能删除最后一把钥匙 —— 删了这台机器就再也进不去了，" +
			"如需更换请从新设备重新执行安装命令")
	}
	return s.writeLocked(kept)
}

// Fingerprints 返回给后端上报用的精简形式（只有指纹与名字）。
//
// 后端只存不判 —— 授权裁决权从不离开这台机器。
func (s *AuthKeyStore) Fingerprints() []map[string]string {
	keys, err := s.List()
	if err != nil {
		return nil
	}
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{"fp": k.FP, "name": k.Name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["fp"] < out[j]["fp"] })
	return out
}

// writeLocked 原子写入。调用方必须持有写锁。
func (s *AuthKeyStore) writeLocked(keys []AuthorizedKey) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(authKeysFile{V: authKeysVersion, Keys: keys}, "", "  ")
	if err != nil {
		return err
	}

	// 先写临时文件再 rename：中途断电不会留下半个文件，
	// 而半个 authorized_keys 意味着这台机器谁都进不去。
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
