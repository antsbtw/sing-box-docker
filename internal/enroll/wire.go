package enroll

// 线上数据结构。接线契约 v1 §4、§5、§6、§10。
// 字段名与类型以契约为准，两边照它写测试。

import "time"

// ── 注册 §4 ────────────────────────────────────────────────

type RegisterRequest struct {
	Contract       int            `json:"contract"`
	Token          string         `json:"token"`
	MachineID      string         `json:"machine_id"`
	AgentVersion   string         `json:"agent_version"`
	SingboxVersion string         `json:"singbox_version"`
	OS             string         `json:"os"`
	Arch           string         `json:"arch"`
	ReportedIP     string         `json:"reported_ip,omitempty"`
	ListenPorts    map[string]int `json:"listen_ports"`
	Secrets        NodeSecrets    `json:"secrets"`
	BootID         string         `json:"boot_id"`
}

// NodeSecrets 是后端拼分享链接所需的公开参数。
//
// ⚠️ 契约 §4.1 红线：不得包含 Reality 私钥、NODE_API_KEY，
// 或除 SS 用户密码之外的任何秘密。
type NodeSecrets struct {
	RealityPublicKey string   `json:"reality_public_key"`
	ShortIDs         []string `json:"short_ids"`
	RealitySNI       string   `json:"reality_sni"`
	SSMethod         string   `json:"ss_method"`
}

type RegisterResponse struct {
	NodeID             string    `json:"node_id"`
	NodeSecret         string    `json:"node_secret"`
	PollURL            string    `json:"poll_url"`
	HeartbeatIntervalS int       `json:"heartbeat_interval_s"`
	PollMaxWaitS       int       `json:"poll_max_wait_s"`
	ServerTime         time.Time `json:"server_time"`
	Reenrolled         bool      `json:"reenrolled"`
}

// ── 长轮询 §5 ──────────────────────────────────────────────

type PollRequest struct {
	BootID       string       `json:"boot_id"`
	AgentVersion string       `json:"agent_version"`
	Status       PollStatus   `json:"status"`
	Stats        []UserStat   `json:"stats"`
	Acks         []CommandAck `json:"acks"`
}

type PollStatus struct {
	SingboxRunning bool           `json:"singbox_running"`
	ListenPorts    map[string]int `json:"listen_ports"`
	UserCount      int            `json:"user_count"`
	UptimeS        int64          `json:"uptime_s"`
	Load1          float64        `json:"load1"`
}

// UserStat 单用户流量。
//
// ⚠️ Up/Down 是**自本次 boot 以来的累计值**，不是增量（契约 §5.1）。
// agent 不清零、不做差：后端按 (node, uuid, boot_id) 取最大值，
// boot_id 变化时把旧 boot 的最大值并入历史基数。
// 这样丢包不会算丢、重启不会算重。
type UserStat struct {
	UUID string `json:"uuid"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

type PollResponse struct {
	ServerTime time.Time `json:"server_time"`
	Commands   []Command `json:"commands"`
	Acked      []string  `json:"acked"`
	NextWaitS  int       `json:"next_wait_s"`
}

// ── 指令 §6 ────────────────────────────────────────────────

type Command struct {
	CommandID string         `json:"command_id"`
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"created_at"`
	ExpiresAt time.Time      `json:"expires_at"`
	Payload   map[string]any `json:"payload"`
}

type CommandAck struct {
	CommandID string         `json:"command_id"`
	Result    string         `json:"result"` // done | failed
	Error     string         `json:"error,omitempty"`
	Detail    string         `json:"detail,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// 指令类型（契约 §6.2）
const (
	CmdCreateUser       = "create_user"
	CmdUpdateUser       = "update_user"
	CmdDeleteUser       = "delete_user"
	CmdResetUserTraffic = "reset_user_traffic"
	CmdGetConfig        = "get_config"
	CmdReload           = "reload"
	CmdPing             = "ping"
	CmdUpgradeAgent     = "upgrade_agent"
)

// 回执结果与错误码（契约 §6.3）
const (
	ResultDone   = "done"
	ResultFailed = "failed"

	ErrExpired            = "expired"
	ErrUserExists         = "user_exists"
	ErrUserNotFound       = "user_not_found"
	ErrInvalidPayload     = "invalid_payload"
	ErrSingboxApplyFailed = "singbox_apply_failed"
	ErrUnsupportedType    = "unsupported_type"
	ErrInternal           = "internal"
)

// ── 错误响应 §10 ───────────────────────────────────────────

type APIError struct {
	Code        string `json:"error"`
	Message     string `json:"message"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
	// agent_too_old 时后端另带这两个字段
	MinVersion    string `json:"min_version,omitempty"`
	LatestRelease string `json:"latest_release,omitempty"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

// 服务端错误码（契约 §10）
const (
	ErrBadRequest     = "bad_request"
	ErrTokenInvalid   = "token_invalid"
	ErrTokenExpired   = "token_expired"
	ErrTokenUsed      = "token_used"
	ErrInvalidSecret  = "invalid_secret"
	ErrNodeRevoked    = "node_revoked"
	ErrNodeNotFound   = "node_not_found"
	ErrAgentTooOld    = "agent_too_old"
	ErrBootIDConflict = "boot_id_conflict"
	ErrRateLimited    = "rate_limited"
	ErrServerInternal = "internal"
)

// IsTerminal 判断该错误是否为终态：停 sing-box、清凭据、退出码 3（契约 §5.3）。
func (e *APIError) IsTerminal() bool {
	return e.Code == ErrNodeRevoked || e.Code == ErrNodeNotFound
}

// IsFatalRegister 注册阶段遇到即应退出进程的错误（契约 §4.3、§2.4-5）。
//
// 这些错误重试也是同样结果（token 已废），退出让 install.sh 的 60 秒等待
// 失败，把错误呈现给用户 —— 比反复重启刷日志有用。
func (e *APIError) IsFatalRegister() bool {
	switch e.Code {
	case ErrBadRequest, ErrTokenInvalid, ErrTokenExpired, ErrTokenUsed, ErrAgentTooOld:
		return true
	}
	return false
}
