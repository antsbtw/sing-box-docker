package models

import (
	"time"
)

// User 用户数据模型
type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	CreatedAt     time.Time `json:"created_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	
	// 流量限制 (字节)
	TrafficLimit int64 `json:"traffic_limit"`
	TrafficUsed  int64 `json:"traffic_used"`
	
	// 设备数限制
	DeviceLimit int `json:"device_limit"`
	
	// 当前连接设备
	ConnectedDevices []string `json:"connected_devices"`
	
	// 状态
	IsActive bool `json:"is_active"`
}

// IsExpired 检查用户是否过期
func (u *User) IsExpired() bool {
	return time.Now().After(u.ExpiresAt)
}

// IsTrafficExceeded 检查流量是否超限
func (u *User) IsTrafficExceeded() bool {
	return u.TrafficUsed >= u.TrafficLimit
}

// CanConnect 检查用户是否可以连接
func (u *User) CanConnect(deviceID string) bool {
	if !u.IsActive || u.IsExpired() || u.IsTrafficExceeded() {
		return false
	}
	
	// 如果设备已连接，允许
	for _, device := range u.ConnectedDevices {
		if device == deviceID {
			return true
		}
	}
	
	// 检查设备数限制
	return len(u.ConnectedDevices) < u.DeviceLimit
}

// CreateUserRequest 创建用户请求
type CreateUserRequest struct {
	Username     string `json:"username" binding:"required"`
	Password     string `json:"password" binding:"required"`
	ExpiresAt    string `json:"expires_at" binding:"required"` // RFC3339格式
	TrafficLimit int64  `json:"traffic_limit" binding:"required"`
	DeviceLimit  int    `json:"device_limit" binding:"required"`
}

// UpdateUserRequest 更新用户请求
type UpdateUserRequest struct {
	ExpiresAt    *string `json:"expires_at,omitempty"`
	TrafficLimit *int64  `json:"traffic_limit,omitempty"`
	DeviceLimit  *int    `json:"device_limit,omitempty"`
	IsActive     *bool   `json:"is_active,omitempty"`
}