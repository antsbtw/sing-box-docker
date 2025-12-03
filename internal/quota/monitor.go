package quota

import (
	"log"
	"sync"
	"time"

	"otun-node-agent/internal/config"
)

// UserQuota 存储用户限额信息
type UserQuota struct {
	UUID           string
	TrafficLimit   int64      // 0 = 无限制
	TrafficUsed    int64      // 服务器已用量
	SessionTraffic int64      // 本次会话流量
	ExpireAt       *time.Time
	Enabled        bool
}

// Monitor 监控用户流量限额和过期
type Monitor struct {
	users    map[string]*UserQuota
	mu       sync.RWMutex
	onRemove func(uuid, reason string) // 用户被移除时的回调
}

// NewMonitor 创建限额监控器
func NewMonitor(onRemove func(uuid, reason string)) *Monitor {
	return &Monitor{
		users:    make(map[string]*UserQuota),
		onRemove: onRemove,
	}
}

// UpdateUsers 更新用户列表（从服务器同步后调用）
func (m *Monitor) UpdateUsers(users []config.User) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 创建新的用户映射
	newUsers := make(map[string]*UserQuota)

	for _, u := range users {
		if !u.Enabled {
			continue
		}

		// 保留已有的会话流量
		sessionTraffic := int64(0)
		if existing, ok := m.users[u.UUID]; ok {
			sessionTraffic = existing.SessionTraffic
		}

		newUsers[u.UUID] = &UserQuota{
			UUID:           u.UUID,
			TrafficLimit:   u.TrafficLimit,
			TrafficUsed:    u.TrafficUsed,
			SessionTraffic: sessionTraffic,
			ExpireAt:       u.ExpireAt,
			Enabled:        u.Enabled,
		}
	}

	m.users = newUsers
	log.Printf("Quota monitor updated: %d active users", len(newUsers))
}

// CheckUser 检查用户是否可以继续使用（每次流量变化时调用）
func (m *Monitor) CheckUser(uuid string, additionalTraffic int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	user, ok := m.users[uuid]
	if !ok {
		return false // 用户不存在
	}

	// 更新会话流量
	user.SessionTraffic += additionalTraffic

	// 检查过期
	if user.ExpireAt != nil && time.Now().After(*user.ExpireAt) {
		log.Printf("User %s expired", uuid)
		delete(m.users, uuid)
		if m.onRemove != nil {
			go m.onRemove(uuid, "expired")
		}
		return false
	}

	// 检查流量限额（0 = 无限制）
	if user.TrafficLimit > 0 {
		totalUsed := user.TrafficUsed + user.SessionTraffic
		if totalUsed >= user.TrafficLimit {
			log.Printf("User %s quota exceeded: %d/%d bytes",
				uuid, totalUsed, user.TrafficLimit)
			delete(m.users, uuid)
			if m.onRemove != nil {
				go m.onRemove(uuid, "quota_exceeded")
			}
			return false
		}
	}

	return true
}

// GetSessionTraffic 获取用户会话流量
func (m *Monitor) GetSessionTraffic(uuid string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if user, ok := m.users[uuid]; ok {
		return user.SessionTraffic
	}
	return 0
}

// GetAllSessionTraffic 获取所有用户的会话流量（用于上报）
func (m *Monitor) GetAllSessionTraffic() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]int64)
	for uuid, user := range m.users {
		if user.SessionTraffic > 0 {
			result[uuid] = user.SessionTraffic
		}
	}
	return result
}

// ResetSessionTraffic 重置会话流量（上报后调用）
func (m *Monitor) ResetSessionTraffic() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, user := range m.users {
		user.SessionTraffic = 0
	}
}

// CheckAllUsers 检查所有用户的过期状态（定时调用）
func (m *Monitor) CheckAllUsers() {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	for uuid, user := range m.users {
		if user.ExpireAt != nil && now.After(*user.ExpireAt) {
			log.Printf("User %s expired (periodic check)", uuid)
			delete(m.users, uuid)
			if m.onRemove != nil {
				go m.onRemove(uuid, "expired")
			}
		}
	}
}

// GetUserCount 获取当前活跃用户数
func (m *Monitor) GetUserCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.users)
}
