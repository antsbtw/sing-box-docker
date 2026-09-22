package local

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// LocalUser 本地管理的用户
type LocalUser struct {
	UUID         string     `json:"uuid"`
	Name         string     `json:"name"`
	Protocols    []string   `json:"protocols"`
	SSPassword   string     `json:"ss_password"`
	Enabled      bool       `json:"enabled"`
	TrafficLimit int64      `json:"traffic_limit"` // 字节，0=无限
	TrafficUsed  int64      `json:"traffic_used"`
	ExpireAt     *time.Time `json:"expire_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// LocalUsersData 本地用户数据文件结构
type LocalUsersData struct {
	Version string      `json:"version"`
	Users   []LocalUser `json:"users"`
}

// ErrUserConflict：uuid 已存在但字段与请求不一致。
// 契约 §6.4 要求据此回 failed user_exists，而不是覆盖用户数据。
var ErrUserConflict = errors.New("user exists with different attributes")

// Store 本地用户存储管理
type Store struct {
	mu       sync.RWMutex
	dataDir  string
	users    map[string]*LocalUser // uuid -> user
	onChange func()                // 用户变更回调
}

// NewStore 创建本地用户存储
func NewStore(dataDir string, onChange func()) *Store {
	s := &Store{
		dataDir:  dataDir,
		users:    make(map[string]*LocalUser),
		onChange: onChange,
	}
	s.load()
	return s
}

// load 从文件加载用户
func (s *Store) load() error {
	path := filepath.Join(s.dataDir, "local_users.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 文件不存在，正常情况
		}
		return err
	}

	var usersData LocalUsersData
	if err := json.Unmarshal(data, &usersData); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range usersData.Users {
		user := usersData.Users[i]
		s.users[user.UUID] = &user
	}

	return nil
}

// save 保存用户到文件（调用者必须已持有锁）
func (s *Store) save() error {
	// 注意：此方法假设调用者已经持有锁（Lock 或 RLock）
	// 不要在这里再获取锁，否则会死锁
	users := make([]LocalUser, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, *u)
	}

	data := LocalUsersData{
		Version: fmt.Sprintf("%d", time.Now().UnixNano()),
		Users:   users,
	}

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(s.dataDir, "local_users.json")
	return os.WriteFile(path, jsonData, 0644)
}

// CreateUser 创建新用户
func (s *Store) CreateUser(req *CreateUserRequest) (*LocalUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 生成 UUID
	userUUID := uuid.New().String()

	// 生成 SS 密码
	ssPassword, err := generatePassword(16)
	if err != nil {
		return nil, fmt.Errorf("generate password: %w", err)
	}

	// 默认协议
	protocols := req.Protocols
	if len(protocols) == 0 {
		protocols = []string{"vless", "shadowsocks"}
	}

	// 计算过期时间
	var expireAt *time.Time
	if req.ExpireDays > 0 {
		t := time.Now().AddDate(0, 0, req.ExpireDays)
		expireAt = &t
	}

	now := time.Now()
	user := &LocalUser{
		UUID:         userUUID,
		Name:         req.Name,
		Protocols:    protocols,
		SSPassword:   ssPassword,
		Enabled:      true,
		TrafficLimit: req.TrafficLimit,
		TrafficUsed:  0,
		ExpireAt:     expireAt,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	s.users[userUUID] = user

	if err := s.save(); err != nil {
		delete(s.users, userUUID)
		return nil, fmt.Errorf("save users: %w", err)
	}

	// 触发回调
	if s.onChange != nil {
		go s.onChange()
	}

	return user, nil
}

// UpsertUser 用**调用方给定**的 uuid 与 ss_password 写入用户。
//
// 与 CreateUser 的区别：后者自己生成 uuid 和密码，而 Token 模式下这两项
// 由后端决定（接线契约 §6.2：「uuid 与 ss_password 用后端给的，不自生成」）——
// 后端要用同样的值拼分享链接，两边必须一致。
//
// 幂等（契约 §6.4）：uuid 已存在且关键字段一致 → 返回 (user, false, nil)，
// 视为成功；不一致 → 返回 ErrUserConflict，由调用方回 failed user_exists。
// 后端在回执丢失时会重投，不能因此报错。
func (s *Store) UpsertUser(u *LocalUser) (*LocalUser, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if u.UUID == "" {
		return nil, false, fmt.Errorf("uuid is required")
	}

	if existing, ok := s.users[u.UUID]; ok {
		// ss_password 是身份的一部分：不同密码意味着后端认为这是另一个用户，
		// 不能静默覆盖（契约 §6.4）。
		if existing.SSPassword != u.SSPassword {
			return nil, false, ErrUserConflict
		}

		// 同 uuid 同密码 = 重复投递或后端修正后重发。
		// ⚠️ 此前只比 name 就回 done，protocols / traffic_limit / expire_at /
		// enabled 的变更会被静默丢弃 —— 后端以为生效了，实际沿用旧值。
		// 改为真正 upsert：应用其余字段再回成功。
		existing.Name = u.Name
		if len(u.Protocols) > 0 {
			existing.Protocols = u.Protocols
		}
		existing.Enabled = u.Enabled
		existing.TrafficLimit = u.TrafficLimit
		existing.ExpireAt = u.ExpireAt
		existing.UpdatedAt = time.Now()

		if err := s.save(); err != nil {
			return nil, false, fmt.Errorf("save users: %w", err)
		}
		if s.onChange != nil {
			go s.onChange()
		}
		return existing, false, nil
	}

	protocols := u.Protocols
	if len(protocols) == 0 {
		protocols = []string{"vless", "shadowsocks"}
	}

	now := time.Now()
	user := &LocalUser{
		UUID:         u.UUID,
		Name:         u.Name,
		Protocols:    protocols,
		SSPassword:   u.SSPassword,
		Enabled:      u.Enabled,
		TrafficLimit: u.TrafficLimit,
		TrafficUsed:  0,
		ExpireAt:     u.ExpireAt,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	s.users[user.UUID] = user

	if err := s.save(); err != nil {
		delete(s.users, user.UUID)
		return nil, false, fmt.Errorf("save users: %w", err)
	}

	if s.onChange != nil {
		go s.onChange()
	}
	return user, true, nil
}

// ResetTraffic 把本地累计用量归零。
//
// ⚠️ 只动本地计数（用于配额判定），**不影响** stats 上报的值 ——
// 那是 sing-box 自己的计数器，agent 不碰（契约 §6.2 reset_user_traffic）。
func (s *Store) ResetTraffic(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[uuid]
	if !ok {
		return fmt.Errorf("user not found: %s", uuid)
	}
	user.TrafficUsed = 0
	user.UpdatedAt = time.Now()
	return s.save()
}

// GetUser 获取单个用户
func (s *Store) GetUser(uuid string) (*LocalUser, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	user, ok := s.users[uuid]
	if !ok {
		return nil, false
	}
	// 返回副本
	copy := *user
	return &copy, true
}

// ListUsers 获取所有用户
func (s *Store) ListUsers() []LocalUser {
	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]LocalUser, 0, len(s.users))
	for _, u := range s.users {
		users = append(users, *u)
	}
	return users
}

// UpdateUser 更新用户
func (s *Store) UpdateUser(uuid string, req *UpdateUserRequest) (*LocalUser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	user, ok := s.users[uuid]
	if !ok {
		return nil, fmt.Errorf("user not found: %s", uuid)
	}

	// 更新字段
	if req.Name != nil {
		user.Name = *req.Name
	}
	if req.Enabled != nil {
		user.Enabled = *req.Enabled
	}
	if req.TrafficLimit != nil {
		user.TrafficLimit = *req.TrafficLimit
	}
	switch {
	case req.ClearExpire:
		// 显式 null：永不过期
		user.ExpireAt = nil
	case req.ExpireAt != nil:
		// 绝对时间直存，不经天数换算 —— 过去的时刻就是已过期，
		// 不会被当成"永不过期"
		t := *req.ExpireAt
		user.ExpireAt = &t
	case req.ExpireDays != nil:
		if *req.ExpireDays > 0 {
			t := time.Now().AddDate(0, 0, *req.ExpireDays)
			user.ExpireAt = &t
		} else {
			user.ExpireAt = nil
		}
	}
	if req.Protocols != nil && len(req.Protocols) > 0 {
		user.Protocols = req.Protocols
	}

	user.UpdatedAt = time.Now()

	if err := s.save(); err != nil {
		return nil, fmt.Errorf("save users: %w", err)
	}

	// 触发回调
	if s.onChange != nil {
		go s.onChange()
	}

	copy := *user
	return &copy, nil
}

// DeleteUser 删除用户
func (s *Store) DeleteUser(uuid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.users[uuid]; !ok {
		return fmt.Errorf("user not found: %s", uuid)
	}

	delete(s.users, uuid)

	if err := s.save(); err != nil {
		return fmt.Errorf("save users: %w", err)
	}

	// 触发回调
	if s.onChange != nil {
		go s.onChange()
	}

	return nil
}

// UpdateTraffic 更新用户流量
func (s *Store) UpdateTraffic(uuid string, upload, download int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if user, ok := s.users[uuid]; ok {
		user.TrafficUsed += upload + download
		s.save() // 异步保存，忽略错误
	}
}

// Clear 清空全部本地用户并落盘。
//
// 用于节点被后端解绑后的终态清理（接线契约 §5.3）。
// ⚠️ 不清的话：unit 仍是 enabled，机器重启后 agent 起来、没有 node.json、
// 也没有 token，就以普通 local 模式把老用户全部重新拉起 ——
// 一个已在 App 里删除的节点继续给人当出口。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.users = make(map[string]*LocalUser)
	if err := s.save(); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	if s.onChange != nil {
		go s.onChange()
	}
	return nil
}

// GetUserCount 获取用户数量
func (s *Store) GetUserCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.users)
}

// CreateUserRequest 创建用户请求
type CreateUserRequest struct {
	Name         string   `json:"name"`
	Protocols    []string `json:"protocols"`     // 可选，默认 ["vless", "shadowsocks"]
	TrafficLimit int64    `json:"traffic_limit"` // 字节，0=无限
	ExpireDays   int      `json:"expire_days"`   // 天数，0=永不过期
}

// UpdateUserRequest 更新用户请求
type UpdateUserRequest struct {
	Name         *string  `json:"name,omitempty"`
	Enabled      *bool    `json:"enabled,omitempty"`
	TrafficLimit *int64   `json:"traffic_limit,omitempty"`
	ExpireDays   *int     `json:"expire_days,omitempty"`
	Protocols    []string `json:"protocols,omitempty"`

	// ExpireAt 直接给绝对到期时间，用于接线契约 §6.2 的 expire_at。
	//
	// ⚠️ 不能用 ExpireDays 承载绝对时间：它把"已过去的时刻"算成 0 天，
	// 而 0 在下面的逻辑里是"永不过期" —— 于是"立即到期"变成"永久有效"。
	// 另外天数截断最多丢 23h59m。
	// ExpireAt 与 ExpireDays 同时给出时以 ExpireAt 为准。
	ExpireAt *time.Time `json:"expire_at,omitempty"`

	// ClearExpire 显式清除到期时间（契约里的 expire_at: null = 永不过期）。
	// 单靠 ExpireAt == nil 分不清"没给这个字段"和"给了 null"。
	ClearExpire bool `json:"-"`
}

// generatePassword 生成随机密码
func generatePassword(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes)[:length], nil
}
