package tunnel

// 内置 SSH 服务端。需求 §4.2（A-1~A-7）、接线契约 §2.3。
//
// 为什么不用系统 sshd：用它就要有系统账号的凭据，等于把"用户必须知道
// SSH 密码"这个门槛又加回来 —— 整个隧道方案就失去意义了。
//
// 认证只有一种：设备持钥（owner key）。授权列表只在本机，后端既签不出
// 也拿不到用户的私钥 —— 后端即使被完全攻破也进不去这台机器。
//
// v1.14.0 删除了「后端签发一次性密码凭据」那条认证分支（owner key 设计 §7）：
// 它正是「后端被攻破即可进入任何机器」的来源。后端 09-23 起已停止签发。

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
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
	nodeID  string
	hostKey ssh.Signer

	// authKeys 是设备持钥（owner key）的授权列表。
	// 非 nil 时启用 publickey 认证 —— 裁决权在本机，后端签不出钥匙。
	authKeys *AuthKeyStore
}

// EnableOwnerKeys 启用设备持钥认证。
func (s *Server) EnableOwnerKeys(store *AuthKeyStore) { s.authKeys = store }

// hostKeySeedFileName 是主机密钥种子的文件名（放 data/ 下）。
const hostKeySeedFileName = "host_key_seed"

// NewServer 用**本机种子**派生主机密钥（契约 §2.3 / A-2）。
//
// ⚠️ 这里曾用 node_secret 派生，那是个会让 TOFU pin 失效的选择：
//
// 带 token 重装时 install.sh 会删掉 node.json（H-3，契约 §2.4 第 0 步），
// agent 重新注册，后端按 machine_id 去重并**轮换 secret** ——
// 于是**每次重装都换指纹**，App 每次都弹"主机密钥已更改"。
//
// 一个每次都要点确认的安全提示等于没有提示：用户会习惯性点"信任"，
// 真有人冒充时也照点不误。真机上反复重装时就是这样。
//
// 改为本机种子之后：
//   - 重装、重启、后端轮换 secret → 指纹不变，不打扰用户
//   - 换机器、data/ 被清空 → 指纹变，**那时的告警才是真信号**
//
// 种子与 authorized_keys.json 同等对待：只存本机、0600、
// install.sh 的清理不碰它。
//
// dataDir 为空时退回 node_secret 派生（仅用于测试，生产必传）。
func NewServer(nodeID, nodeSecret, dataDir string) (*Server, error) {
	seed, err := loadOrCreateHostKeySeed(dataDir, nodeSecret)
	if err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, fmt.Errorf("derive host key: %w", err)
	}

	return &Server{
		nodeID:  nodeID,
		hostKey: signer,
	}, nil
}

// loadOrCreateHostKeySeed 读取本机种子，没有就生成一把并落盘。
//
// dataDir 为空时退回旧行为（从 node_secret 派生），只为让不关心
// 持久化的测试仍可直接构造 Server。
func loadOrCreateHostKeySeed(dataDir, nodeSecret string) ([]byte, error) {
	if dataDir == "" {
		sum := sha256.Sum256([]byte(hostKeySeedContext + nodeSecret))
		return sum[:], nil
	}

	path := filepath.Join(dataDir, hostKeySeedFileName)

	// 已有就用它 —— 这是"重装后指纹不变"的关键
	if raw, err := os.ReadFile(path); err == nil {
		if decoded, derr := hex.DecodeString(strings.TrimSpace(string(raw))); derr == nil && len(decoded) == ed25519.SeedSize {
			return decoded, nil
		}
		// 文件坏了：重新生成而不是报错退出。
		// 报错会让 agent 起不来，而指纹变化只是需要用户确认一次 ——
		// 两害相权取其轻。
		log.Printf("[tunnel] %s 内容无效，重新生成（指纹会变一次）", hostKeySeedFileName)
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate host key seed: %w", err)
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// 先写临时文件再 rename：中途断电不会留下半个种子，
	// 而半个种子意味着下次启动指纹又变一次。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		return nil, fmt.Errorf("write host key seed: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("persist host key seed: %w", err)
	}

	log.Printf("[tunnel] 已生成本机主机密钥种子（%s）", path)
	return seed, nil
}

// Serve 在一条隧道连接上跑 SSH 服务端，直到连接结束。
//
// sessionID 用于把凭据绑定到本会话：A 会话的凭据不能拿到 B 会话用。
func (s *Server) Serve(conn net.Conn, sessionID string) error {
	// 没有设备钥匙列表 = 谁都进不去。不给任何退路 —— 退路就是后门。
	if s.authKeys == nil {
		// 连接要关掉 —— 只返回错误的话,对端会一直等握手
		_ = conn.Close()
		return errors.New("未启用设备钥匙，无法认证")
	}

	cfg := &ssh.ServerConfig{}

	// ── publickey：设备持钥（owner key 方案）────────────────────────
	//
	// 钥匙只存在用户设备上，后端既签不出也拿不到 ——
	// 后端即使被完全攻破也进不去这台机器。
	// 裁决依据是本机的 authorized_keys.json，不是任何远端下发的东西。
	cfg.PublicKeyCallback = func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if c.User() != sshUsername {
			return nil, errAuthFailed
		}
		k, ok := s.authKeys.Authorized(key)
		if !ok {
			// 只记指纹，不回具体原因 —— 免得帮攻击者逐项试探
			log.Printf("[tunnel] 未授权的设备钥匙：%s", ssh.FingerprintSHA256(key))
			return nil, errAuthFailed
		}
		log.Printf("[tunnel] 设备 %q 通过 publickey 认证（%s）", k.Name, k.FP)
		// 把本会话的指纹传下去，obox-key 用它记 added_by（审计用，不是门槛）
		return &ssh.Permissions{
			Extensions: map[string]string{"owner-key-fp": k.FP},
		}, nil
	}

	cfg.AddHostKey(s.hostKey)

	sshConn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return fmt.Errorf("ssh handshake: %w", err)
	}
	defer sshConn.Close()

	go ssh.DiscardRequests(reqs)

	for newChan := range chans {
		// direct-tcpip：App 访问本机管理面（127.0.0.1:8080）用。
		//
		// token 模式下本地 API 只绑回环（T-3，不把 node_api_key 保护的
		// 管理面暴露在公网），App 因此够不着它 —— 走隧道转发是正路。
		if newChan.ChannelType() == "direct-tcpip" {
			go s.handleDirectTCPIP(newChan)
			continue
		}
		if newChan.ChannelType() != "session" {
			_ = newChan.Reject(ssh.UnknownChannelType, "only session/direct-tcpip are supported")
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

// directTCPIPPayload 是 RFC 4254 §7.2 的 direct-tcpip 负载。
type directTCPIPPayload struct {
	DestHost   string
	DestPort   uint32
	OriginHost string
	OriginPort uint32
}

// handleDirectTCPIP 把一条 TCP 转发接到本机端口。
//
// ⚠️ **只允许回环目标。**
//
// 不限制的话，这条隧道就变成一个进入用户内网的通用代理：
// 凭据一旦泄漏，攻击者能借它访问 VPS 所在私有网络里的任何主机
// （云厂商的元数据服务 169.254.169.254 尤其危险，能取到实例凭据）。
// App 需要的只是本机管理面，回环足够。
func (s *Server) handleDirectTCPIP(newChan ssh.NewChannel) {
	var p directTCPIPPayload
	if err := ssh.Unmarshal(newChan.ExtraData(), &p); err != nil {
		_ = newChan.Reject(ssh.ConnectionFailed, "bad direct-tcpip payload")
		return
	}

	if !isLoopbackHost(p.DestHost) {
		log.Printf("[tunnel] 拒绝非回环转发：%s:%d", p.DestHost, p.DestPort)
		_ = newChan.Reject(ssh.Prohibited, "only loopback destinations are allowed")
		return
	}

	target := net.JoinHostPort(p.DestHost, fmt.Sprintf("%d", p.DestPort))
	conn, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		log.Printf("[tunnel] direct-tcpip 连 %s 失败：%v", target, err)
		_ = newChan.Reject(ssh.ConnectionFailed, err.Error())
		return
	}

	ch, reqs, err := newChan.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)

	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	go func() { _, _ = io.Copy(conn, ch); stop() }()
	go func() { _, _ = io.Copy(ch, conn); stop() }()

	<-done
	_ = conn.Close()
	_ = ch.Close()
}

// isLoopbackHost 判断目标是不是本机回环。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleSession 处理一个 SSH session channel：PTY shell 或 exec。
func (s *Server) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()

	var (
		mu      sync.Mutex // 保护 ptyFile / cmd：请求循环与 pipe 协程并发访问
		ptyReq  *ptyRequest
		ptyFile *os.File
		cmd     *exec.Cmd
		started bool
	)

	// pipe 在独立协程里跑，请求循环必须保持可用，
	// 否则 shell 启动后的 window-change（旋屏、键盘弹出）永远排不到处理（A-6）。
	piped := make(chan struct{})

	defer func() {
		mu.Lock()
		f, c := ptyFile, cmd
		mu.Unlock()
		if f != nil {
			_ = f.Close()
		}
		if c != nil && c.Process != nil {
			_ = c.Process.Kill()
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
			if p, err := parseWindowChange(req.Payload); err == nil {
				mu.Lock()
				if ptyFile != nil {
					setWinsize(ptyFile, p.cols, p.rows)
				} else if ptyReq != nil {
					// shell 尚未启动，记下尺寸，startShell 时一并生效
					ptyReq.cols, ptyReq.rows = p.cols, p.rows
				}
				mu.Unlock()
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
			mu.Lock()
			cmd, ptyFile = c, f
			mu.Unlock()
			_ = req.Reply(true, nil)
			go func() {
				defer close(piped)
				// 置空与关闭在同一把锁内完成，
				// window-change 不会再 ioctl 到已关闭的 fd
				s.pipe(ch, f, c, func() {
					mu.Lock()
					ptyFile = nil
					mu.Unlock()
					_ = f.Close()
				})
			}()

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
			s.runExec(ch, command, ptyReq)
			return

		case "env":
			_ = req.Reply(true, nil) // 接受但忽略

		default:
			_ = req.Reply(false, nil)
		}
	}

	// reqs 关闭（客户端收起 channel）后，若 shell 仍在跑，等 pipe 收尾，
	// 避免 defer 抢在 pipe 之前关掉 ptyFile。
	mu.Lock()
	running := ptyFile != nil
	mu.Unlock()
	if running {
		<-piped
	}
}

// startShell 启动带 PTY 的交互式 shell（A-4、A-7）。
func (s *Server) startShell(p *ptyRequest) (*exec.Cmd, *os.File, error) {
	shell := loginShell()

	cmd := exec.Command(shell, "-l")
	cmd.Env = shellEnv(termOrDefault(p))
	cmd.Dir = homeDir()

	f, err := pty.Start(cmd)
	if err != nil {
		return nil, nil, err
	}
	if p != nil {
		setWinsize(f, p.cols, p.rows)
	}
	return cmd, f, nil
}

// loginShell 选一个可用的交互 shell。
// systemd 服务环境不带 SHELL，所以不能只靠环境变量。
func loginShell() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

// homeDir 返回当前用户的 HOME。
// systemd 不传 HOME，缺了它 vi/top 等程序无处写配置（A-4）。
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/root"
}

// shellEnv 组装交互 shell 的环境。
//
// agent 由 systemd 拉起，拿到的是被裁剪过的环境：没有 HOME/USER/LOGNAME，
// PATH 也可能只有 /usr/bin:/bin。直接 append 到 os.Environ() 上，
// 登录脚本会因为认不出终端而把 TERM 清空，vi、top 跟着一起坏（A-4、A-6）。
// 这里补齐登录 shell 该有的那几个变量，并剔除 agent 自己的密钥类变量 ——
// 终端是给用户用的，没必要把 node secret 摆在 env 里。
func shellEnv(term string) []string {
	home := homeDir()
	uname := os.Getenv("USER")
	if uname == "" {
		if u, err := user.Current(); err == nil {
			uname = u.Username
		} else {
			uname = "root"
		}
	}

	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}

	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		if isSensitiveEnv(kv) {
			continue
		}
		switch {
		case strings.HasPrefix(kv, "TERM="),
			strings.HasPrefix(kv, "HOME="),
			strings.HasPrefix(kv, "USER="),
			strings.HasPrefix(kv, "LOGNAME="),
			strings.HasPrefix(kv, "SHELL="),
			strings.HasPrefix(kv, "PATH="):
			continue // 由下面统一补齐
		}
		env = append(env, kv)
	}
	return append(env,
		"TERM="+term,
		"HOME="+home,
		"USER="+uname,
		"LOGNAME="+uname,
		"SHELL="+loginShell(),
		"PATH="+path,
	)
}

// isSensitiveEnv 过滤掉 agent 自身的凭据，避免出现在用户终端的 env 里。
func isSensitiveEnv(kv string) bool {
	for _, k := range []string{
		"NODE_API_KEY=", "OTUN_ENROLL_TOKEN=", "NODE_SECRET=",
	} {
		if strings.HasPrefix(kv, k) {
			return true
		}
	}
	return false
}

// runExec 执行一条命令（A-5，安装脚本用）。
//
// 若客户端先发了 pty-req（即 `ssh -t host cmd` 形态），命令要跑在 pty 上：
// top、apt 进度条这类程序靠 isatty 决定是否输出交互界面（A-4/A-5）。
// 没有 pty-req 时保持非交互，安装脚本走的是这条。
func (s *Server) runExec(ch ssh.Channel, command string, p *ptyRequest) {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Env = shellEnv(termOrDefault(p))
	cmd.Dir = homeDir()

	if p == nil {
		// stdin 经管道转给命令 —— 配方的密参从 stdin 读(应用平台 09 §4.2)。
		//
		// ⚠️ 不能写 cmd.Stdin = ch。那样 cmd.Run() 会等 stdin 到 EOF,
		// 而很多客户端发完 exec 就等结果、从不半关闭写端,于是命令跑完了
		// cmd.Run() 仍卡在复制 stdin 上,channel 永不关闭
		// (2026-09-23 真机实测:`id -u` 卡死)。
		//
		// 用 StdinPipe:复制在独立 goroutine 里做,cmd.Wait() 只等进程退出、
		// 退出时关掉管道,不等客户端的 EOF。不发输入的客户端照旧,
		// 发输入并半关闭的客户端(App 执行配方)命令能读到。
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()

		code := 0
		stdin, err := cmd.StdinPipe()
		if err == nil {
			err = cmd.Start()
		}
		if err == nil {
			go func() {
				_, _ = io.Copy(stdin, ch)
				_ = stdin.Close()
			}()
			err = cmd.Wait()
		}
		if err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = 1
			}
		}
		sendExitStatus(ch, code)
		_ = ch.CloseWrite()
		_ = ch.Close()
		return
	}

	f, err := pty.Start(cmd)
	if err != nil {
		log.Printf("[tunnel] exec 分配 pty 失败：%v", err)
		sendExitStatus(ch, 1)
		_ = ch.Close()
		return
	}
	setWinsize(f, p.cols, p.rows)

	// 复用 pipe 的收尾：等进程退出 → 关 pty master → 回退出码 → 关 channel。
	// exec 的 pty 不与请求循环共享，直接关即可。
	s.pipe(ch, f, cmd, func() { _ = f.Close() })
}

// pipe 在 SSH channel 与 PTY 之间双向搬运，直到任一端结束。
// pipe 搬运字节并负责会话收尾。
//
// closePTY 由调用方提供：pty master 的关闭必须和 window-change 的 ioctl
// 互斥，所以这个动作交回持有锁的一方做，pipe 只决定「什么时候关」。
func (s *Server) pipe(ch ssh.Channel, ptyFile *os.File, cmd *exec.Cmd, closePTY func()) {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	// 进程退出是会话结束的权威信号。
	//
	// 不能只等 io.Copy：shell 退出后 pty master 未必立刻报 EOF，
	// 两个 Copy 都可能继续阻塞，于是 exit-status 发不出去、channel 不关，
	// App 的终端页面一直挂着（联调 T-1）。
	waited := make(chan int, 1)
	go func() {
		code := 0
		if err := cmd.Wait(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else {
				code = 1
			}
		}
		waited <- code
		stop()
	}()

	// 用户输入 → shell。
	// stdin 到头不代表会话结束 —— exec 形态下客户端根本不发输入，
	// 交互 shell 也可能只半关闭。所以这里不调 stop()。
	go func() {
		_, _ = io.Copy(ptyFile, ch)
	}()
	// shell 输出 → 用户。这一侧断了说明 pty 已关或对端走了。
	go func() {
		_, _ = io.Copy(ch, ptyFile)
		stop()
	}()

	<-done

	// 关掉 pty master，解开仍卡在 Copy 上的那一侧，
	// 顺便让 shell 收到 SIGHUP（客户端先断开时）。
	closePTY()

	// 进程还没退（客户端主动断开）就等一下，拿到真实退出码。
	var code int
	select {
	case code = <-waited:
	case <-time.After(2 * time.Second):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case code = <-waited:
		case <-time.After(2 * time.Second):
			code = 1
		}
	}

	// 先回退出码，再关 channel —— x/crypto/ssh 的标准收尾顺序。
	sendExitStatus(ch, code)
	_ = ch.CloseWrite()
	_ = ch.Close()
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
