package enroll

// 指令执行。接线契约 §6。
//
// ⚠️ **指令白名单**(v1.14.0,应用平台 09 §5.3):后端经长轮询能让 agent 做的事
// 只有下面这几条,且都不执行任意代码:
//
//	ping、get_config、reload、open_tunnel —— 所有机器
//	upgrade_agent                        —— 仅托管模式(OBOX_HOSTED=true)
//
// 其余一律回 unsupported_type。
//
// 四条用户指令(create_user / update_user / delete_user / reset_user_traffic)
// 已整体删除:用户自己的机器上,后端不应能加 VPN 用户(等于能借用机器和带宽);
// 所有机器的用户管理都经 App 直连本地 API(/api/local/*)。
//
// agent 仓库的发布权限等同于"能在全部 token 节点上执行代码"—— 本文件的任何放宽都要双人评审。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// TunnelOpener 建立一条隧道。由 tunnel.Manager 实现。
//
// 用接口而非直接依赖 tunnel 包：executor 不需要知道隧道是怎么连的，
// 也便于测试注入。
type TunnelOpener interface {
	Open(ctx context.Context, sessionID, tunnelURL string) error
}

// NodeInfoProvider 提供 get_config 要回报的节点参数。
type NodeInfoProvider interface {
	ListenPorts() map[string]int
	Secrets() NodeSecrets
	SingboxVersion() string
}

// Executor 执行后端下发的指令并生成回执。
type Executor struct {
	info         NodeInfoProvider
	agentVersion string
	reload       func() error

	// 自升级。为空则 upgrade_agent 回 unsupported_type，后端按契约 §11 降级。
	upgrade *UpgradeConfig

	// pendingExit 由 upgrade_agent 置位：新二进制已就位，
	// 但必须等回执送达后才能退出（契约 §7.4）。
	pendingExit bool

	// tunnel 为 nil 时 open_tunnel 回 unsupported_type，
	// 后端据此让 App 提示"请升级节点 agent"（tunnel v1 §2.1-3）。
	tunnel TunnelOpener
	// tunnel_url 只允许指向这个主机（--api-url 的主机名）
	tunnelHost string

	// 已完成指令的回执缓存。契约 §6.4：后端可能因回执丢失而重投，
	// 重复收到时直接重放原回执，不要重新执行。
	mu     sync.Mutex
	recent map[string]CommandAck
	order  []string // 淘汰顺序，最多留 maxRecent 条
}

const maxRecent = 200

func NewExecutor(info NodeInfoProvider, agentVersion string, reload func() error) *Executor {
	return &Executor{
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
	case CmdGetConfig:
		return e.getConfig()
	case CmdReload:
		return e.doReload()
	case CmdPing:
		return done(nil)
	case CmdUpgradeAgent:
		return e.upgradeAgent(cmd)
	case CmdOpenTunnel:
		return e.openTunnel(cmd)
	default:
		// 未知类型回 unsupported_type，后端据此降级（契约 §11）
		return failed(ErrUnsupportedType, "不支持的指令类型: "+cmd.Type)
	}
}

// ── 各指令 ────────────────────────────────────────────────

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

// EnableTunnel 开启 open_tunnel 支持。
//
// allowedHost 是 --api-url 的主机名:隧道只允许连回同一台后端。
// 否则后端(或冒充后端下指令的人)能让 agent 把 SSH 服务端接到任意地址上。
func (e *Executor) EnableTunnel(t TunnelOpener, allowedHost string) {
	e.tunnel = t
	e.tunnelHost = allowedHost
}

type openTunnelPayload struct {
	SessionID string    `json:"session_id"`
	TunnelURL string    `json:"tunnel_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

// openTunnel 处理 open_tunnel 指令（tunnel v1 §2.1）。
//
// 必须**立即**连接并据实回执：连上回 done，失败回 failed，
// 后端据此决定是否关会话 —— App 正在等着这个结果，
// 拖延或谎报都会让用户对着转圈的界面等到超时。
func (e *Executor) openTunnel(cmd Command) CommandAck {
	if e.tunnel == nil {
		return failed(ErrUnsupportedType, "本版本不支持隧道")
	}

	var p openTunnelPayload
	if err := decodePayload(cmd.Payload, &p); err != nil {
		return failed(ErrInvalidPayload, err.Error())
	}
	if p.SessionID == "" || p.TunnelURL == "" {
		return failed(ErrInvalidPayload, "session_id 与 tunnel_url 必填")
	}
	if !sameHost(p.TunnelURL, e.tunnelHost) {
		return failed(ErrInvalidPayload, "tunnel_url 必须与 api-url 同主机")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := e.tunnel.Open(ctx, p.SessionID, p.TunnelURL); err != nil {
		log.Printf("[tunnel] 建立会话 %s 失败：%v", p.SessionID, err)
		return failed(ErrTunnelConnectFailed, err.Error())
	}
	return done(map[string]any{"session_id": p.SessionID})
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

	// 只升不降,且只认语义化版本(拒绝 latest 这类浮动名)。
	// 可降级 = 能把机器退回到一个有已知漏洞的版本(09 §5.3)。
	if !isNewerRelease(plan.ReleaseTag, e.agentVersion) {
		return failed(ErrInvalidPayload,
			fmt.Sprintf("拒绝升级到 %q：目标必须是高于当前 %s 的语义化版本", plan.ReleaseTag, e.agentVersion))
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
