package local

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	if req.ExpireDays != nil {
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
}

// generatePassword 生成随机密码
func generatePassword(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes)[:length], nil
}
