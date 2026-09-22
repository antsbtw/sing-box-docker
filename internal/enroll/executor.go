package enroll

// 指令执行。接线契约 §6。
//
// 指令集刻意与本地 /api/local/* 的动词一一对应：执行时直接调 local.Store，
// 不新造一套用户管理逻辑。Token 模式与存量 API-Key 模式共用同一份状态。

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"otun-node-agent/internal/local"
)

// NodeInfoProvider 提供 get_config 要回报的节点参数。
type NodeInfoProvider interface {
	ListenPorts() map[string]int
	Secrets() NodeSecrets
	SingboxVersion() string
}

// Executor 执行后端下发的指令并生成回执。
type Executor struct {
	store        *local.Store
	info         NodeInfoProvider
	agentVersion string
	reload       func() error

	// 自升级。为空则 upgrade_agent 回 unsupported_type，后端按契约 §11 降级。
	upgrade *UpgradeConfig

	// pendingExit 由 upgrade_agent 置位：新二进制已就位，
	// 但必须等回执送达后才能退出（契约 §7.4）。
	pendingExit bool

	// 已完成指令的回执缓存。契约 §6.4：后端可能因回执丢失而重投，
	// 重复收到时直接重放原回执，不要重新执行。
	mu     sync.Mutex
	recent map[string]CommandAck
	order  []string // 淘汰顺序，最多留 maxRecent 条
}

const maxRecent = 200

func NewExecutor(store *local.Store, info NodeInfoProvider, agentVersion string, reload func() error) *Executor {
	return &Executor{
		store:        store,
		info:         info,
		agentVersion: agentVersion,
		reload:       reload,
		recent:       make(map[string]CommandAck, maxRecent),
	}
}

// Execute 执行一条指令，永远返回回执（不返回 error）——
// 契约 §6.1：每条指令都必须回执，done 或 failed。
func (e *Executor) Execute(cmd Command, serverTime time.Time) CommandAck {
	// 重复投递：重放原回执
	if ack, ok := e.replay(cmd.CommandID); ok {
		return ack
	}

	ack := e.execute(cmd, serverTime)
	ack.CommandID = cmd.CommandID
	e.remember(ack)
	return ack
}

func (e *Executor) execute(cmd Command, serverTime time.Time) CommandAck {
	// 过期指令不执行（契约 §6.1）。用服务端时间判断，不信本机时钟 ——
	// VPS 时钟漂移会让指令被误判为过期或误执行（契约 §0）。
	if !cmd.ExpiresAt.IsZero() && serverTime.After(cmd.ExpiresAt) {
		return failed(ErrExpired, "指令已过期")
	}

	switch cmd.Type {
	case CmdCreateUser:
		return e.createUser(cmd)
	case CmdUpdateUser:
		return e.updateUser(cmd)
	case CmdDeleteUser:
		return e.deleteUser(cmd)
	case CmdResetUserTraffic:
		return e.resetTraffic(cmd)
	case CmdGetConfig:
		return e.getConfig()
	case CmdReload:
		return e.doReload()
	case CmdPing:
		return done(nil)
	case CmdUpgradeAgent:
		return e.upgradeAgent(cmd)
	default:
		// 未知类型回 unsupported_type，后端据此降级（契约 §11）
		return failed(ErrUnsupportedType, "不支持的指令类型: "+cmd.Type)
	}
}

// ── 各指令 ────────────────────────────────────────────────

type createUserPayload struct {
	UUID         string     `json:"uuid"`
	Name         string     `json:"name"`
	Protocols    []string   `json:"protocols"`
	SSPassword   string     `json:"ss_password"`
	TrafficLimit int64      `json:"traffic_limit"`
	ExpireAt     *time.Time `json:"expire_at"`
	Enabled      *bool      `json:"enabled"`
}

func (e *Executor) createUser(cmd Command) CommandAck {
	var p createUserPayload
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return failed(ErrInvalidPayload, err.Error())
	}
	if p.UUID == "" || p.SSPassword == "" {
		return failed(ErrInvalidPayload, "uuid 与 ss_password 必填")
	}

	enabled := true
	if p.Enabled != nil {
		enabled = *p.Enabled
	}

	// ⚠️ 用后端给的 uuid / ss_password，不自生成 —— 后端要用同样的值
	// 拼分享链接（契约 §6.2）
	_, _, err := e.store.UpsertUser(&local.LocalUser{
		UUID:         p.UUID,
		Name:         p.Name,
		Protocols:    p.Protocols,
		SSPassword:   p.SSPassword,
		Enabled:      enabled,
		TrafficLimit: p.TrafficLimit,
		ExpireAt:     p.ExpireAt,
	})
	switch {
	case err == local.ErrUserConflict:
		return failed(ErrUserExists, "uuid 已存在且字段不一致")
	case err != nil:
		return failed(ErrInternal, err.Error())
	}
	return done(map[string]any{"uuid": p.UUID})
}

type updateUserPayload struct {
	UUID         string     `json:"uuid"`
	Name         *string    `json:"name"`
	Enabled      *bool      `json:"enabled"`
	TrafficLimit *int64     `json:"traffic_limit"`
	ExpireAt     *time.Time `json:"expire_at"`
	Protocols    []string   `json:"protocols"`
}

func (e *Executor) updateUser(cmd Command) CommandAck {
	var p updateUserPayload
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return failed(ErrInvalidPayload, err.Error())
	}
	if p.UUID == "" {
		return failed(ErrInvalidPayload, "uuid 必填")
	}
	if _, ok := e.store.GetUser(p.UUID); !ok {
		return failed(ErrUserNotFound, "uuid 不存在")
	}

	req := &local.UpdateUserRequest{
		Name:         p.Name,
		Enabled:      p.Enabled,
		TrafficLimit: p.TrafficLimit,
		Protocols:    p.Protocols,
	}

	// expire_at 三态（契约 §6.2）：
	//   字段缺席     → 不改
	//   显式 null    → 永不过期
	//   具体时间     → 直存绝对时间
	//
	// ⚠️ 不能换算成天数：已过去的时刻会算出负数、被夹成 0，
	// 而 0 在 Store 里是"永不过期" —— "立即到期"就变成了"永久有效"。
	if raw, present := cmd.Payload["expire_at"]; present {
		if raw == nil {
			req.ClearExpire = true
		} else if p.ExpireAt != nil {
			req.ExpireAt = p.ExpireAt
		}
	}

	if _, err := e.store.UpdateUser(p.UUID, req); err != nil {
		return failed(ErrInternal, err.Error())
	}
	return done(map[string]any{"uuid": p.UUID})
}

func (e *Executor) deleteUser(cmd Command) CommandAck {
	uuid, _ := cmd.Payload["uuid"].(string)
	if uuid == "" {
		return failed(ErrInvalidPayload, "uuid 必填")
	}
	// 幂等（契约 §6.4）：不存在即视为已删除，回 done 而非 failed
	if _, ok := e.store.GetUser(uuid); !ok {
		return done(map[string]any{"uuid": uuid})
	}
	if err := e.store.DeleteUser(uuid); err != nil {
		return failed(ErrInternal, err.Error())
	}
	return done(map[string]any{"uuid": uuid})
}

func (e *Executor) resetTraffic(cmd Command) CommandAck {
	uuid, _ := cmd.Payload["uuid"].(string)
	if uuid == "" {
		return failed(ErrInvalidPayload, "uuid 必填")
	}
	if err := e.store.ResetTraffic(uuid); err != nil {
		return failed(ErrUserNotFound, err.Error())
	}
	return done(map[string]any{"uuid": uuid})
}

func (e *Executor) getConfig() CommandAck {
	return done(map[string]any{
		"listen_ports":    e.info.ListenPorts(),
		"secrets":         e.info.Secrets(),
		"singbox_version": e.info.SingboxVersion(),
		"agent_version":   e.agentVersion,
	})
}

func (e *Executor) doReload() CommandAck {
	if e.reload == nil {
		return failed(ErrInternal, "reload 未接线")
	}
	if err := e.reload(); err != nil {
		return failed(ErrSingboxApplyFailed, err.Error())
	}
	return done(nil)
}

// UpgradeConfig 自升级所需的本机信息。
type UpgradeConfig struct {
	Repo    string // 形如 antsbtw/sing-box-docker
	ExePath string // 当前可执行文件路径
}

// EnableUpgrade 开启 upgrade_agent 支持。
func (e *Executor) EnableUpgrade(cfg *UpgradeConfig) { e.upgrade = cfg }

// PendingExit 报告是否有已就位、等待回执送达后重启的升级。
func (e *Executor) PendingExit() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pendingExit
}

// upgradeAgent 下载并就位新版本，但**不在这里退出** ——
// 回执必须先送达后端。退出由 Runner 在确认 acked 之后执行（契约 §7.4）。
func (e *Executor) upgradeAgent(cmd Command) CommandAck {
	if e.upgrade == nil {
		return failed(ErrUnsupportedType, "本版本不支持自升级")
	}

	var plan UpgradePlan
	if err := decodePayload(cmd.Payload, &plan); err != nil {
		return failed(ErrInvalidPayload, err.Error())
	}

	// 已是目标版本：幂等，不重复下载（契约 §6.4）
	if plan.ReleaseTag == e.agentVersion {
		return done(map[string]any{"from": e.agentVersion, "to": plan.ReleaseTag})
	}

	if err := PrepareUpgrade(plan, e.upgrade.Repo, e.upgrade.ExePath); err != nil {
		// 升级失败不影响当前版本继续服务 —— 旧二进制没被动过
		log.Printf("[upgrade] 失败：%v", err)
		return failed(ErrInternal, err.Error())
	}

	e.mu.Lock()
	e.pendingExit = true
	e.mu.Unlock()

	log.Printf("[upgrade] %s → %s 已就位，等待回执送达后重启",
		e.agentVersion, plan.ReleaseTag)
	return done(map[string]any{"from": e.agentVersion, "to": plan.ReleaseTag})
}

// ── 幂等缓存 ──────────────────────────────────────────────

func (e *Executor) replay(id string) (CommandAck, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ack, ok := e.recent[id]
	if ok {
		log.Printf("[executor] 指令 %s 重复投递，重放原回执", id)
	}
	return ack, ok
}

func (e *Executor) remember(ack CommandAck) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.recent[ack.CommandID]; !exists {
		e.order = append(e.order, ack.CommandID)
	}
	e.recent[ack.CommandID] = ack

	for len(e.order) > maxRecent {
		delete(e.recent, e.order[0])
		e.order = e.order[1:]
	}
}

// ── 辅助 ──────────────────────────────────────────────────

func decodePayload(payload map[string]any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("payload 不是合法 JSON: %w", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("payload 字段不匹配: %w", err)
	}
	return nil
}

func done(data map[string]any) CommandAck {
	return CommandAck{Result: ResultDone, Data: data}
}

func failed(code, detail string) CommandAck {
	return CommandAck{Result: ResultFailed, Error: code, Detail: detail}
}
