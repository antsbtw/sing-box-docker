package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"sing-box-manager/internal/models"
)

// JSONStorage JSON文件存储
type JSONStorage struct {
	filePath string
	mutex    sync.RWMutex
	users    map[string]*models.User
}

// NewJSONStorage 创建JSON存储实例
func NewJSONStorage(filePath string) *JSONStorage {
	storage := &JSONStorage{
		filePath: filePath,
		users:    make(map[string]*models.User),
	}
	
	// 加载现有数据
	storage.loadFromFile()
	
	// 启动自动清理过期用户
	go storage.startCleanupJob()
	
	return storage
}

// loadFromFile 从文件加载数据
func (s *JSONStorage) loadFromFile() error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	data, err := os.ReadFile(s.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			// 文件不存在，创建空文件
			return s.saveToFile()
		}
		return err
	}
	
	if len(data) == 0 {
		return nil
	}
	
	return json.Unmarshal(data, &s.users)
}

// saveToFile 保存数据到文件
func (s *JSONStorage) saveToFile() error {
	data, err := json.MarshalIndent(s.users, "", "  ")
	if err != nil {
		return err
	}
	
	return os.WriteFile(s.filePath, data, 0644)
}

// CreateUser 创建用户
func (s *JSONStorage) CreateUser(user *models.User) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if _, exists := s.users[user.ID]; exists {
		return fmt.Errorf("user with ID %s already exists", user.ID)
	}
	
	s.users[user.ID] = user
	return s.saveToFile()
}

// GetUser 获取用户
func (s *JSONStorage) GetUser(id string) (*models.User, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	user, exists := s.users[id]
	if !exists {
		return nil, fmt.Errorf("user with ID %s not found", id)
	}
	
	return user, nil
}

// GetUserByUsername 根据用户名获取用户
func (s *JSONStorage) GetUserByUsername(username string) (*models.User, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	for _, user := range s.users {
		if user.Username == username {
			return user, nil
		}
	}
	
	return nil, fmt.Errorf("user with username %s not found", username)
}

// UpdateUser 更新用户
func (s *JSONStorage) UpdateUser(id string, user *models.User) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if _, exists := s.users[id]; !exists {
		return fmt.Errorf("user with ID %s not found", id)
	}
	
	s.users[id] = user
	return s.saveToFile()
}

// DeleteUser 删除用户
func (s *JSONStorage) DeleteUser(id string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	if _, exists := s.users[id]; !exists {
		return fmt.Errorf("user with ID %s not found", id)
	}
	
	delete(s.users, id)
	return s.saveToFile()
}

// ListUsers 列出所有用户
func (s *JSONStorage) ListUsers() ([]*models.User, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	
	users := make([]*models.User, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}
	
	return users, nil
}

// AddConnectedDevice 添加连接设备
func (s *JSONStorage) AddConnectedDevice(userID, deviceID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	user, exists := s.users[userID]
	if !exists {
		return fmt.Errorf("user with ID %s not found", userID)
	}
	
	// 检查设备是否已连接
	for _, device := range user.ConnectedDevices {
		if device == deviceID {
			return nil // 已连接
		}
	}
	
	user.ConnectedDevices = append(user.ConnectedDevices, deviceID)
	return s.saveToFile()
}

// RemoveConnectedDevice 移除连接设备
func (s *JSONStorage) RemoveConnectedDevice(userID, deviceID string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	user, exists := s.users[userID]
	if !exists {
		return fmt.Errorf("user with ID %s not found", userID)
	}
	
	for i, device := range user.ConnectedDevices {
		if device == deviceID {
			user.ConnectedDevices = append(user.ConnectedDevices[:i], user.ConnectedDevices[i+1:]...)
			break
		}
	}
	
	return s.saveToFile()
}

// UpdateTrafficUsage 更新流量使用
func (s *JSONStorage) UpdateTrafficUsage(userID string, bytesUsed int64) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	user, exists := s.users[userID]
	if !exists {
		return fmt.Errorf("user with ID %s not found", userID)
	}
	
	user.TrafficUsed += bytesUsed
	return s.saveToFile()
}

// startCleanupJob 启动自动清理任务
func (s *JSONStorage) startCleanupJob() {
	ticker := time.NewTicker(1 * time.Hour) // 每小时检查一次
	defer ticker.Stop()
	
	for range ticker.C {
		s.cleanupExpiredUsers()
	}
}

// cleanupExpiredUsers 清理过期用户
func (s *JSONStorage) cleanupExpiredUsers() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	
	var expiredUsers []string
	for id, user := range s.users {
		if user.IsExpired() {
			expiredUsers = append(expiredUsers, id)
		}
	}
	
	for _, id := range expiredUsers {
		delete(s.users, id)
	}
	
	if len(expiredUsers) > 0 {
		s.saveToFile()
		fmt.Printf("Cleaned up %d expired users\n", len(expiredUsers))
	}
}