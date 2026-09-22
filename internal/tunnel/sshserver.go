package tunnel

// 内置 SSH 服务端。需求 §4.2（A-1~A-7）、接线契约 §2.3。
//
// 为什么不用系统 sshd：用它就要有系统账号的凭据，等于把"用户必须知道
// SSH 密码"这个门槛又加回来 —— 整个隧道方案就失去意义了。
// 内置服务端用后端签发的短期凭据认证，用户全程无感。

import (
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// 契约 §2.3：用户名固定 obox
const sshUsername = "obox"

// HostKeySeedContext 派生主机密钥用的前缀（契约 §2.3）。
const hostKeySeedContext = "obox-host-key-v1|"

// Server 是跑在隧道上的 SSH 服务端。
type Server struct {
	nodeID   string
	verifier *CredVerifier
	hostKey  ssh.Signer

	// 后端公钥与 kid，随 poll 响应更新（契约 §3.3）
	mu     sync.RWMutex
	pubkey ed25519.PublicKey
	keyID  string

	// ServerTime 返回当前时间的权威值，来自最近一次 poll 的 server_time。
	// 不信本机时钟：VPS 时钟漂移会让合法凭据被拒或过期凭据被放行。
	ServerTime func() time.Time
}

// NewServer 用 node_secret 派生主机密钥（契约 §2.3 / A-2）。
//
// 派生而非随机生成：重装后只要 secret 不变，指纹就不变，
// 客户端不会弹"主机密钥已更改"。secret 轮换时指纹会变，
// App 侧需按 node_id 记指纹并在 reenrolled 后刷新。
func NewServer(nodeID, nodeSecret string, verifier *CredVerifier) (*Server, error) {
	seed := sha256.Sum256([]byte(hostKeySeedContext + nodeSecret))
	priv := ed25519.NewKeyFromSeed(seed[:])

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("derive host key: %w", err)
	}

	return &Server{
		nodeID:     nodeID,
		verifier:   verifier,
		hostKey:    signer,
		ServerTime: time.Now,
	}, nil
}

// SetTunnelKey 更新后端的凭据签名公钥（契约 §3.3，随每次 poll 下发）。
func (s *Server) SetTunnelKey(pubkey ed25519.PublicKey, keyID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pubkey, s.keyID = pubkey, keyID
}

func (s *Server) tunnelKey() (ed25519.PublicKey, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pubkey, s.keyID
}

// Serve 在一条隧道连接上跑 SSH 服务端，直到连接结束。
//
// sessionID 用于把凭据绑定到本会话：A 会话的凭据不能拿到 B 会话用。
func (s *Server) Serve(conn net.Conn, sessionID string) error {
	pubkey, keyID := s.tunnelKey()
	if pubkey == nil {
		return errors.New("尚未收到后端的 tunnel_pubkey，无法验证凭据")
	}

	cfg := &ssh.ServerConfig{
		// 契约 §2.3：仅 password 方法。不接受 publickey、不接受系统密码。
		PasswordCallback: func(c ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			if c.User() != sshUsername {
				return nil, errAuthFailed
			}
			err := s.verifier.Verify(
				string(password), pubkey, keyID, s.nodeID, sessionID, s.ServerTime())
			if err != nil {
				// ⚠️ 只记日志，不把原因回给客户端 ——
				// 泄露"哪一条不过"会帮助攻击者逐项试探（契约 §3.2 末）
				log.Printf("[tunnel] 凭据验证失败: %v", err)
				return nil, errAuthFailed
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(s.hostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session is supported")
			continue
		}
		ch, chReqs, err := newChan.Accept()
		if err != nil {
			log.Printf("[tunnel] accept channel: %v", err)
			continue
		}
		go s.handleSession(ch, chReqs)
	}
	return nil
}

var errAuthFailed = errors.New("authentication failed")

// handleSession 处理一个 SSH session channel：PTY shell 或 exec。
func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var (
		ptyReq  *ptyRequest
		ptyFile *os.File
		cmd     *exec.Cmd
		started bool
	)

	defer func() {
		if ptyFile != nil {
			_ = ptyFile.Close()
		}
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			p, err := parsePTYRequest(req.Payload)
			if err != nil {
				_ = req.Reply(false, nil)
				continue
			}
			ptyReq = p
			_ = req.Reply(true, nil)

		case "window-change":
			// A-6：手机键盘弹出、旋屏时同步窗口尺寸
			if p, err := parseWindowChange(req.Payload); err == nil && ptyFile != nil {
				setWinsize(ptyFile, p.cols, p.rows)
			}

		case "shell":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			c, f, err := s.startShell(ptyReq)
			if err != nil {
				log.Printf("[tunnel] start shell: %v", err)
				_ = req.Reply(false, nil)
				return
			}
			cmd, ptyFile = c, f
			_ = req.Reply(true, nil)
			s.pipe(ch, ptyFile, cmd)
			return

		case "exec":
			if started {
				_ = req.Reply(false, nil)
				continue
			}
			started = true
			command, err := parseExecRequest(req.Payload)
			if err != nil {
				_ = req.Reply(false, nil)
				return
			}
			_ = req.Reply(true, nil)
			s.runExec(ch, command)
			return

		case "env":
			_ = req.Reply(true, nil) // 接受但忽略

		default:
			_ = req.Reply(false, nil)
		}
	}
}

// startShell 启动带 PTY 的交互式 shell（A-4、A-7）。
func (s *Server) startShell(p *ptyRequest) (*exec.Cmd, *os.File, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
		if _, err := os.Stat(shell); err != nil {
			shell = "/bin/sh"
		}
	}

	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "TERM="+termOrDefault(p))

	f, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		setWinsize(f, p.cols, p.rows)
	}
	return cmd, f, nil
}

// runExec 执行一条非交互命令（A-5，安装脚本用）。
func (s *Server) runExec(ch ssh.Channel, command string) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdin = ch
	cmd.Stdout = ch
	cmd.Stderr = ch.Stderr()

	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = 1
		}
	}
	sendExitStatus(ch, code)
}

// pipe 在 SSH channel 与 PTY 之间双向搬运，直到任一端结束。
func (s *Server) pipe(ch ssh.Channel, ptyFile *os.File, cmd *exec.Cmd) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	go func() {
		_, _ = io.Copy(ptyFile, ch) // 用户输入 → shell
		stop()
	}()
	go func() {
		_, _ = io.Copy(ch, ptyFile) // shell 输出 → 用户
		stop()
	}()

	<-done
	_ = ptyFile.Close()

	code := 0
	if err := cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	sendExitStatus(ch, code)
}

func sendExitStatus(ch ssh.Channel, code int) {
	payload := []byte{0, 0, 0, 0}
	payload[0] = byte(code >> 24)
	payload[1] = byte(code >> 16)
	payload[2] = byte(code >> 8)
	payload[3] = byte(code)
	_, _ = ch.SendRequest("exit-status", false, payload)
}

// ── SSH 请求负载解析（RFC 4254） ──────────────────────────

type ptyRequest struct {
	term       string
	cols, rows uint32
}

func parsePTYRequest(payload []byte) (*ptyRequest, error) {
	term, rest, ok := parseString(payload)
	if !ok {
		return nil, errors.New("pty-req: term")
	}
	cols, rest, ok := parseUint32(rest)
	if !ok {
		return nil, errors.New("pty-req: cols")
	}
	rows, _, ok := parseUint32(rest)
	if !ok {
		return nil, errors.New("pty-req: rows")
	}
	return &ptyRequest{term: term, cols: cols, rows: rows}, nil
}

type windowChange struct{ cols, rows uint32 }

func parseWindowChange(payload []byte) (*windowChange, error) {
	cols, rest, ok := parseUint32(payload)
	if !ok {
		return nil, errors.New("window-change: cols")
	}
	rows, _, ok := parseUint32(rest)
	if !ok {
		return nil, errors.New("window-change: rows")
	}
	return &windowChange{cols: cols, rows: rows}, nil
}

func parseExecRequest(payload []byte) (string, error) {
	cmd, _, ok := parseString(payload)
	if !ok {
		return "", errors.New("exec: command")
	}
	return cmd, nil
}

func parseUint32(b []byte) (uint32, []byte, bool) {
	if len(b) < 4 {
		return 0, nil, false
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), b[4:], true
}

func parseString(b []byte) (string, []byte, bool) {
	n, rest, ok := parseUint32(b)
	if !ok || uint32(len(rest)) < n {
		return "", nil, false
	}
	return string(rest[:n]), rest[n:], true
}

func termOrDefault(p *ptyRequest) string {
	if p != nil && p.term != "" {
		return p.term
	}
	return "xterm-256color"
}

// setWinsize 同步 PTY 窗口尺寸（A-6）。
func setWinsize(f *os.File, cols, rows uint32) {
	ws := struct{ Rows, Cols, X, Y uint16 }{
		Rows: uint16(rows), Cols: uint16(cols),
	}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		uintptr(syscall.TIOCSWINSZ), uintptr(unsafe.Pointer(&ws)))
}
