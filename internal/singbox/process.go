package singbox

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// 崩溃后的重启退避：1s → 2s → 4s …，封顶 maxRestartBackoff。
	//
	// 原来是固定 1 秒且无上限。sing-box 因端口被占而起不来时，
	// 这会变成每秒一次的无限循环：日志刷爆、CPU 空转，
	// 而且把真正的错误淹没在几千行重复里（真机上一天刷了上万行）。
	initialRestartBackoff = time.Second
	maxRestartBackoff     = 30 * time.Second

	// 连续失败这么多次就停下。
	//
	// 一直重试是在假装工作：端口被占、配置错误、二进制损坏
	// 都不会因为多试几次而好转。停下来并明确报错，
	// 让 /health 能如实反映状态，比无声地空转有用。
	maxConsecutiveFailures = 10

	// 进程活过这么久就算这次启动成功。
	// bind 失败通常在几十毫秒内发生，5 秒足够区分。
	startSettleWindow = 5 * time.Second
)

// Manager 管理 sing-box 进程
type Manager struct {
	binPath    string
	configPath string
	cmd        *exec.Cmd
	mu         sync.Mutex
	running    bool

	// generation 用于让过期的 monitor 认出"自己看的那个进程已经被取代了"。
	//
	// ⚠️ 没有它会有一个竞态：Stop() 把 running 置 false 之后，
	// 仍阻塞在 cmd.Wait() 里的 monitor 醒来，可能读到下一次 Start()
	// 设置的 running=true，于是把一个**正常运行**的进程当成崩溃，
	// 再拉起一个 —— 两个进程抢同一批端口，后者必然失败并进入循环。
	generation uint64

	// consecutiveFailures 连续启动失败次数，成功启动后清零。
	consecutiveFailures int

	// stopped 为 true 表示已放弃重启（连续失败过多）。
	// /health 会读它，避免谎报健康。
	stopped bool
}

// NewManager 创建进程管理器
func NewManager(binPath, configPath string) *Manager {
	return &Manager{
		binPath:    binPath,
		configPath: configPath,
	}
}

// Start 启动 sing-box 进程
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startLocked()
}

// startLocked 启动进程。调用方必须持有锁。
func (m *Manager) startLocked() error {
	if m.running {
		return fmt.Errorf("sing-box is already running")
	}

	// 检查配置文件是否存在
	if _, err := os.Stat(m.configPath); os.IsNotExist(err) {
		return fmt.Errorf("config file not found: %s", m.configPath)
	}

	// 检查 sing-box 二进制是否存在
	if _, err := os.Stat(m.binPath); os.IsNotExist(err) {
		return fmt.Errorf("sing-box binary not found: %s", m.binPath)
	}

	// ⚠️ 启动前清理残留进程 —— 这是本文件最要紧的一行。
	//
	// 原来只认 m.cmd.Process.Pid，也就是"自己这次启动的那个"。
	// 但残留进程有好几种来源：上一次安装留下的、agent 自己崩溃
	// 重启前留下的、用户手动跑过的。它们占着 443/54716/10085，
	// 新进程 bind 失败 → 退出 → 被拉起 → 再失败，无限循环。
	//
	// 更糟的是循环期间**老进程还活着**，跑的是旧配置：
	// 用户改了 SNI、建了新用户，生效的却还是老的 ——
	// 于是"配置明明对的却连不上"，而且时好时坏（取决于
	// 那一刻哪个进程活着）。真机上整整一天的反复就是它造成的。
	if n := m.reapStrays(); n > 0 {
		log.Printf("[sing-box] 清理了 %d 个残留进程后再启动", n)
	}

	m.cmd = exec.Command(m.binPath, "run", "-c", m.configPath)
	// sing-box 自己的日志（含启动失败的 FATAL 行）直接进 journal，
	// 否则调用方只能看到一句 "exit status 1"，查不出为什么。
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	m.running = true
	m.generation++
	log.Printf("sing-box started with PID %d", m.cmd.Process.Pid)

	// 监控进程退出。把 generation 与 cmd 按值传进去 ——
	// 醒来时拿它和当前状态比对，就知道自己是不是已经过期。
	go m.monitor(m.generation, m.cmd)

	// 活过观察窗口就算这次启动成功，清零失败计数。
	//
	// ⚠️ 不能在这里直接清零：bind 失败是在 Start 返回之后几十毫秒
	// 才发生的，立刻清零会让退避与上限形同虚设 —— 每次崩溃都被
	// 当成"新的第一次"。所以要等它确实活过一段时间。
	gen := m.generation
	go func() {
		time.Sleep(startSettleWindow)
		m.mu.Lock()
		defer m.mu.Unlock()
		if gen == m.generation && m.running {
			m.consecutiveFailures = 0
			m.stopped = false
		}
	}()

	return nil
}

// reapStrays 杀掉所有跑着本机配置的 sing-box 进程。
//
// 用 pkill 按完整命令行匹配（配置路径），而不是按进程名 ——
// 机器上可能有用户自己跑的别的 sing-box 实例，不该误杀。
//
// 返回清理掉的进程数。找不到残留时返回 0，这是常态、不是错误。
func (m *Manager) reapStrays() int {
	pattern := fmt.Sprintf("%s run -c %s", m.binPath, m.configPath)

	// pgrep -f 取 pid：先看有没有，避免无谓的 kill
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil || len(out) == 0 {
		return 0 // 没有残留
	}

	// 自己刚启动的那个不该在这里（startLocked 里此时还没 Start），
	// 所以匹配到的都是残留。
	exec.Command("pkill", "-TERM", "-f", pattern).Run()

	// 给它们一点时间释放端口。不等的话新进程仍会 bind 失败，
	// 等于没清理。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if o, e := exec.Command("pgrep", "-f", pattern).Output(); e != nil || len(o) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// 还赖着不走的强杀
	if o, e := exec.Command("pgrep", "-f", pattern).Output(); e == nil && len(o) > 0 {
		exec.Command("pkill", "-KILL", "-f", pattern).Run()
		time.Sleep(300 * time.Millisecond)
	}

	return 1 // 至少清理了一个；精确计数对调用方没有意义
}

// Stop 停止 sing-box 进程
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 先把 generation 推进，让仍在 Wait() 里的 monitor 醒来后
	// 认出自己已过期，不要去"重启"。
	m.generation++
	m.running = false

	if m.cmd == nil || m.cmd.Process == nil {
		return nil
	}

	log.Println("Stopping sing-box...")
	cmd := m.cmd
	m.cmd = nil

	// 发送 SIGTERM
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		cmd.Process.Kill()
	}

	// 等待进程退出。
	//
	// ⚠️ 这里**不能**用 cmd.Wait() —— monitor 已经在 Wait 了，
	// 同一个 Cmd 上并发 Wait 会 panic 或返回错误。改为轮询进程是否还在。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			log.Println("sing-box stopped")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd.Process.Kill()
	log.Println("sing-box force killed")
	return nil
}

// Reload 重载配置（重启 sing-box 进程）
func (m *Manager) Reload() error {
	if !m.IsRunning() {
		return fmt.Errorf("sing-box is not running")
	}

	log.Println("Reloading sing-box config...")

	// sing-box 不支持 SIGHUP，需要重启
	if err := m.Stop(); err != nil {
		return fmt.Errorf("stop sing-box for reload: %w", err)
	}

	if err := m.Start(); err != nil {
		return fmt.Errorf("start sing-box after reload: %w", err)
	}

	return nil
}

// IsRunning 检查是否运行中
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// GaveUp 报告是否已放弃重启（连续失败过多）。
//
// /health 用它：sing-box 反复起不来时必须如实报 unhealthy，
// 谎报健康会让排查往完全错误的方向走。
func (m *Manager) GaveUp() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

// monitor 监控进程状态，崩溃时按退避重启。
//
// gen 是启动这个进程时的 generation。醒来后若与当前不符，
// 说明这个进程已被 Stop 或被新的 Start 取代，此时**不能**重启 ——
// 否则会和新进程抢端口。
func (m *Manager) monitor(gen uint64, cmd *exec.Cmd) {
	if cmd == nil {
		return
	}

	err := cmd.Wait()

	m.mu.Lock()
	// 过期的 monitor：这个进程已经不是"当前进程"了，什么都不做。
	if gen != m.generation {
		m.mu.Unlock()
		return
	}
	m.running = false
	m.consecutiveFailures++
	failures := m.consecutiveFailures

	if failures >= maxConsecutiveFailures {
		m.stopped = true
		m.mu.Unlock()
		log.Printf("[sing-box] 连续 %d 次启动失败，停止重试。"+
			"最后一次退出：%v —— 请检查上面 sing-box 自己打印的错误。",
			failures, err)
		return
	}

	// 指数退避：1s, 2s, 4s… 封顶
	backoff := initialRestartBackoff << (failures - 1)
	if backoff > maxRestartBackoff {
		backoff = maxRestartBackoff
	}
	m.mu.Unlock()

	log.Printf("sing-box exited unexpectedly: %v（第 %d 次，%v 后重试）",
		err, failures, backoff)
	time.Sleep(backoff)

	m.mu.Lock()
	// 退避期间可能已被 Stop 或被新的 Start 取代
	if gen != m.generation {
		m.mu.Unlock()
		return
	}
	startErr := m.startLocked()
	m.mu.Unlock()

	if startErr != nil {
		log.Printf("Failed to restart sing-box: %v", startErr)
	}
}

// CheckConfig 验证配置文件
func (m *Manager) CheckConfig() error {
	cmd := exec.Command(m.binPath, "check", "-c", m.configPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("config check failed: %s", string(output))
	}
	return nil
}
