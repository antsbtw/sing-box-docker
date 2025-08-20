package service

import (
	"fmt"
	"time"

	"sing-box-manager/internal/models"
	"sing-box-manager/internal/storage"

	"github.com/google/uuid"
)

// UserService 用户服务
type UserService struct {
	storage *storage.JSONStorage
}

// NewUserService 创建用户服务
func NewUserService(storage *storage.JSONStorage) *UserService {
	return &UserService{
		storage: storage,
	}
}

// CreateUser 创建用户
func (s *UserService) CreateUser(req *models.CreateUserRequest) (*models.User, error) {
	// 检查用户名是否已存在
	if _, err := s.storage.GetUserByUsername(req.Username); err == nil {
		return nil, fmt.Errorf("username %s already exists", req.Username)
	}
	
	// 解析过期时间
	expiresAt, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("invalid expires_at format: %v", err)
	}
	
	// 创建用户
	user := &models.User{
		ID:               uuid.New().String(),
		Username:         req.Username,
		Password:         req.Password,
		CreatedAt:        time.Now(),
		ExpiresAt:        expiresAt,
		TrafficLimit:     req.TrafficLimit,
		TrafficUsed:      0,
		DeviceLimit:      req.DeviceLimit,
		ConnectedDevices: make([]string, 0),
		IsActive:         true,
	}
	
	if err := s.storage.CreateUser(user); err != nil {
		return nil, err
	}
	
	return user, nil
}

// GetUser 获取用户
func (s *UserService) GetUser(id string) (*models.User, error) {
	return s.storage.GetUser(id)
}

// GetUserByUsername 根据用户名获取用户
func (s *UserService) GetUserByUsername(username string) (*models.User, error) {
	return s.storage.GetUserByUsername(username)
}

// UpdateUser 更新用户
func (s *UserService) UpdateUser(id string, req *models.UpdateUserRequest) (*models.User, error) {
	user, err := s.storage.GetUser(id)
	if err != nil {
		return nil, err
	}
	
	// 更新字段
	if req.ExpiresAt != nil {
		expiresAt, err := time.Parse(time.RFC3339, *req.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("invalid expires_at format: %v", err)
		}
		user.ExpiresAt = expiresAt
	}
	
	if req.TrafficLimit != nil {
		user.TrafficLimit = *req.TrafficLimit
	}
	
	if req.DeviceLimit != nil {
		user.DeviceLimit = *req.DeviceLimit
	}
	
	if req.IsActive != nil {
		user.IsActive = *req.IsActive
	}
	
	if err := s.storage.UpdateUser(id, user); err != nil {
		return nil, err
	}
	
	return user, nil
}

// DeleteUser 删除用户
func (s *UserService) DeleteUser(id string) error {
	return s.storage.DeleteUser(id)
}

// ListUsers 列出所有用户
func (s *UserService) ListUsers() ([]*models.User, error) {
	return s.storage.ListUsers()
}

// ConnectDevice 连接设备
func (s *UserService) ConnectDevice(userID, deviceID string) error {
	user, err := s.storage.GetUser(userID)
	if err != nil {
		return err
	}
	
	if !user.CanConnect(deviceID) {
		return fmt.Errorf("user cannot connect: expired, traffic exceeded, or device limit reached")
	}
	
	return s.storage.AddConnectedDevice(userID, deviceID)
}

// DisconnectDevice 断开设备
func (s *UserService) DisconnectDevice(userID, deviceID string) error {
	return s.storage.RemoveConnectedDevice(userID, deviceID)
}

// UpdateTrafficUsage 更新流量使用
func (s *UserService) UpdateTrafficUsage(userID string, bytesUsed int64) error {
	return s.storage.UpdateTrafficUsage(userID, bytesUsed)
}

// GetUserStats 获取用户统计信息
func (s *UserService) GetUserStats(userID string) (map[string]interface{}, error) {
	user, err := s.storage.GetUser(userID)
	if err != nil {
		return nil, err
	}
	
	stats := map[string]interface{}{
		"user_id":           user.ID,
		"username":          user.Username,
		"is_active":         user.IsActive,
		"is_expired":        user.IsExpired(),
		"traffic_used":      user.TrafficUsed,
		"traffic_limit":     user.TrafficLimit,
		"traffic_remaining": user.TrafficLimit - user.TrafficUsed,
		"device_count":      len(user.ConnectedDevices),
		"device_limit":      user.DeviceLimit,
		"expires_at":        user.ExpiresAt,
		"connected_devices": user.ConnectedDevices,
	}
	
	return stats, nil
}