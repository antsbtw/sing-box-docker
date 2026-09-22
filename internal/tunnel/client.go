package tunnel

// 隧道客户端。接线契约 §2.1、§2.2。
//
// agent 从长轮询收到 open_tunnel 指令后，主动连到后端的 WS 端点，
// 把这条连接交给内置 SSH 服务端。

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// 契约 §2.1：收到指令后应"立即"连接。拨号本身给足余量即可。
	dialTimeout = 15 * time.Second
	// 契约 §1.2：后端每 30s ping，库会自动回 pong；
	// 这里设读超时兜底，防止对端假死时协程泄漏。
	readTimeout = 90 * time.Second
	// 契约 §1.2：单帧 ≤ 1 MiB
	maxFrameSize = 1 << 20
)

// Manager 管理本节点当前活跃的隧道。
type Manager struct {
	nodeID     string
	nodeSecret string
	sshServer  *Server

	mu      sync.Mutex
	active  map[string]context.CancelFunc // session_id → 取消函数
	maxOpen int
}

func NewManager(nodeID, nodeSecret string, sshServer *Server, maxOpen int) *Manager {
	if maxOpen <= 0 {
		maxOpen = 3 // 契约 §2.2：同一节点同时 ≤3 条
	}
	return &Manager{
		nodeID:     nodeID,
		nodeSecret: nodeSecret,
		sshServer:  sshServer,
		active:     make(map[string]context.CancelFunc),
		maxOpen:    maxOpen,
	}
}

// Open 按 open_tunnel 指令建立一条隧道。
//
// 同步完成 WebSocket 连接后即返回 —— 契约 §2.1 要求据此回执：
// 连上回 done，失败回 failed，后端据此决定是否关会话让 App 立刻知道。
// SSH 会话在后台继续跑。
func (m *Manager) Open(ctx context.Context, sessionID, tunnelURL string) error {
	m.mu.Lock()
	if _, exists := m.active[sessionID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("会话 %s 已存在", sessionID)
	}
	if len(m.active) >= m.maxOpen {
		m.mu.Unlock()
		return fmt.Errorf("并发隧道已达上限 %d", m.maxOpen)
	}
	m.mu.Unlock()

	ws, err := m.dial(ctx, tunnelURL)
	if err != nil {
		return err
	}

	sessCtx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.active[sessionID] = cancel
	m.mu.Unlock()

	go m.serve(sessCtx, sessionID, ws)
	return nil
}

func (m *Manager) dial(ctx context.Context, tunnelURL string) (*websocket.Conn, error) {
	u, err := url.Parse(tunnelURL)
	if err != nil {
		return nil, fmt.Errorf("解析 tunnel_url: %w", err)
	}
	// 契约 §2.2：?node_id= 必带 —— 后端先用它鉴权再查会话，
	// 避免拿会话 id 来探测。
	q := u.Query()
	q.Set("node_id", m.nodeID)
	u.RawQuery = q.Encode()

	dialer := &websocket.Dialer{
		HandshakeTimeout: dialTimeout,
		ReadBufferSize:   32 << 10,
		WriteBufferSize:  32 << 10,
	}
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+m.nodeSecret)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	ws, resp, err := dialer.DialContext(dialCtx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("连接隧道失败 (HTTP %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("连接隧道失败: %w", err)
	}

	ws.SetReadLimit(maxFrameSize)
	return ws, nil
}

// serve 把这条 WS 交给内置 SSH 服务端，直到任一端结束。
//
// ⚠️ 不重连（契约 §2.2）：WS 关闭意味着会话已结束，
// App 想要会再开一条。盲目重连只会去打一个已关闭的会话。
func (m *Manager) serve(ctx context.Context, sessionID string, ws *websocket.Conn) {
	defer func() {
		_ = ws.Close()
		m.mu.Lock()
		delete(m.active, sessionID)
		m.mu.Unlock()
		log.Printf("[tunnel] 会话 %s 结束", shortID(sessionID))
	}()

	// 读超时兜底：后端 30s 一次 ping，收到就顺延
	_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
		return ws.WriteControl(websocket.PongMessage, []byte(data),
			time.Now().Add(10*time.Second))
	})

	// ctx 取消时（节点解绑、进程退出）立即断开，
	// 让阻塞中的 Read 返回
	go func() {
		<-ctx.Done()
		_ = ws.Close()
	}()

	log.Printf("[tunnel] 会话 %s 已连接，启动 SSH 服务端", shortID(sessionID))

	if err := m.sshServer.Serve(newWSConn(ws), sessionID); err != nil {
		// 客户端正常断开也会走到这里，不是错误
		log.Printf("[tunnel] 会话 %s: %v", shortID(sessionID), err)
	}
}

// CloseAll 关闭全部隧道。用于节点解绑或进程退出。
func (m *Manager) CloseAll() {
	m.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(m.active))
	for _, c := range m.active {
		cancels = append(cancels, c)
	}
	m.mu.Unlock()

	for _, c := range cancels {
		c()
	}
}

// ActiveCount 当前活跃隧道数。
func (m *Manager) ActiveCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.active)
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
