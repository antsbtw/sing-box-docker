package config

import "time"

// ManagementMode 管理模式
type ManagementMode string

const (
	// ModeLocal 本地管理模式：只使用本地 API 管理用户
	ModeLocal ManagementMode = "local"
	// ModeRemote 远程管理模式：从上游服务器同步用户
	ModeRemote ManagementMode = "remote"
	// ModeHybrid 混合模式：本地 + 远程用户合并
	ModeHybrid ManagementMode = "hybrid"
)

// AgentConfig 是 Agent 的运行配置
type AgentConfig struct {
	APIURL         string
	NodeAPIKey     string
	NodeID         string
	SyncInterval   time.Duration
	StatsInterval  time.Duration
	VLESSPort      int
	SSPort         int
	SingboxBin     string
	SingboxConfig  string
	LogLevel       string
	ManagementMode ManagementMode // 管理模式
	ServerIP       string         // 服务器公网 IP（用于生成连接 URL）
}

// User 是从管理服务器获取的用户信息
type User struct {
	UUID          string     `json:"uuid"`
	Protocols     []string   `json:"protocols"`
	SSPassword    string     `json:"ss_password"`
	Enabled       bool       `json:"enabled"`
	TrafficLimit  int64      `json:"traffic_limit"`
	TrafficUsed   int64      `json:"traffic_used"`
	ExpireAt      *time.Time `json:"expire_at"`
	DeviceID      string     `json:"device_id"`       // 绑定的设备指纹
}

// UsersResponse 是管理服务器返回的用户列表
type UsersResponse struct {
	Version string `json:"version"`
	Users   []User `json:"users"`
	Config  struct {
		RealitySNI string `json:"reality_sni"`
	} `json:"config"`
}

// StatsEntry 是单个用户的流量统计
type StatsEntry struct {
	UUID     string `json:"uuid"`
	Upload   int64  `json:"upload"`
	Download int64  `json:"download"`
}

// StatsReport 是上报的流量统计
type StatsReport struct {
	Timestamp time.Time    `json:"timestamp"`
	Stats     []StatsEntry `json:"stats"`
}

// Connection 活跃连接信息
type Connection struct {
	UserUUID    string    `json:"user_uuid"`
	ClientIP    string    `json:"client_ip"`
	ConnectedAt time.Time `json:"connected_at"`
	Upload      int64     `json:"upload"`
	Download    int64     `json:"download"`
}

// ConnectionsReport 连接上报
type ConnectionsReport struct {
	NodeID      string       `json:"node_id"`
	Timestamp   time.Time    `json:"timestamp"`
	Connections []Connection `json:"connections"`
}

// HeartbeatRequest 心跳请求
type HeartbeatRequest struct {
	NodeID    string    `json:"node_id"`
	Timestamp time.Time `json:"timestamp"`
	Load      NodeLoad  `json:"load"`
}

// NodeLoad 节点负载信息
type NodeLoad struct {
	CPUPercent        float64 `json:"cpu_percent"`
	MemoryPercent     float64 `json:"memory_percent"`
	BandwidthMbps     int     `json:"bandwidth_mbps"`
	ActiveConnections int     `json:"active_connections"`
	UserCount         int     `json:"user_count"`
}

// HeartbeatResponse 心跳响应
type HeartbeatResponse struct {
	OK          bool     `json:"ok"`
	KickUsers   []string `json:"kick_users"`    // 需要踢掉的用户
	ReloadUsers bool     `json:"reload_users"`  // 是否需要重新拉取用户列表
}
