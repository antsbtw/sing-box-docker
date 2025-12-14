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

// Manager 管理 sing-box 进程
type Manager struct {
	binPath    string
	configPath string
	cmd        *exec.Cmd
	mu         sync.Mutex
	running    bool
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

	m.cmd = exec.Command(m.binPath, "run", "-c", m.configPath)
	m.cmd.Stdout = os.Stdout
	m.cmd.Stderr = os.Stderr

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start sing-box: %w", err)
	}

	m.running = true
	log.Printf("sing-box started with PID %d", m.cmd.Process.Pid)

	// 监控进程退出
	go m.monitor()

	return nil
}

// Stop 停止 sing-box 进程
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.running || m.cmd == nil || m.cmd.Process == nil {
		return nil
	}

	log.Println("Stopping sing-box...")

	// 发送 SIGTERM
	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// 如果 SIGTERM 失败，尝试 SIGKILL
		m.cmd.Process.Kill()
	}

	// 等待进程退出
	done := make(chan error, 1)
	go func() {
		done <- m.cmd.Wait()
	}()

	select {
	case <-done:
		log.Println("sing-box stopped")
	case <-time.After(5 * time.Second):
		m.cmd.Process.Kill()
		log.Println("sing-box force killed")
	}

	m.running = false
	m.cmd = nil
	return nil
}

// Reload 重载配置（重启 sing-box 进程）
func (m *Manager) Reload() error {
	// 检查是否运行中（不持有锁，因为 Stop/Start 会自己加锁）
	if !m.IsRunning() {
		return fmt.Errorf("sing-box is not running")
	}

	log.Println("Reloading sing-box config...")

	// sing-box 不支持 SIGHUP，需要重启
	// Stop 和 Start 各自会处理锁，这里不能持有锁
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

// monitor 监控进程状态，崩溃时自动重启
func (m *Manager) monitor() {
	if m.cmd == nil {
		return
	}

	err := m.cmd.Wait()
	
	m.mu.Lock()
	wasRunning := m.running
	m.running = false
	m.mu.Unlock()

	if wasRunning {
		log.Printf("sing-box exited unexpectedly: %v", err)
		// 等待一秒后尝试重启
		time.Sleep(time.Second)
		if err := m.Start(); err != nil {
			log.Printf("Failed to restart sing-box: %v", err)
		}
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
