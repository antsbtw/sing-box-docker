package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"otun-node-agent/internal/api"
	"otun-node-agent/internal/config"
	"otun-node-agent/internal/quota"
	"otun-node-agent/internal/singbox"
	"otun-node-agent/internal/stats"
)

// Agent 是主控制器
type Agent struct {
	cfg        *config.AgentConfig
	secrets    *config.NodeSecrets
	syncer     *config.Syncer
	cache      *config.Cache
	generator  *config.Generator
	manager    *singbox.Manager
	connMgr    *singbox.ConnectionManager
	monitor    *quota.Monitor
	collector  *stats.Collector
	reporter   *stats.Reporter

	currentVersion string
	mu             sync.RWMutex
}

func main() {
	log.Println("========================================")
	log.Println("  OTun Node Agent v1.0.0")
	log.Println("========================================")

	// 加载配置
	cfg := config.LoadFromEnv()
	if cfg.NodeAPIKey == "" {
		log.Fatal("NODE_API_KEY is required")
	}

	log.Printf("Node ID: %s", cfg.NodeID)
	log.Printf("API URL: %s", cfg.APIURL)

	// 初始化 Agent
	agent, err := NewAgent(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize agent: %v", err)
	}

	// 设置优雅退出
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutdown signal received...")
		cancel()
	}()

	// 启动 Agent
	agent.Run(ctx)
}

// NewAgent 创建新的 Agent 实例
func NewAgent(cfg *config.AgentConfig) (*Agent, error) {
	// 确保数据目录存在
	dataDir := "./data"
	statsCache := "./data/stats"
	os.MkdirAll(dataDir, 0755)
	os.MkdirAll(statsCache, 0755)

	// 加载或生成节点密钥（包含随机 SS 端口）
	secrets, err := config.LoadOrGenerateSecrets(dataDir)
	if err != nil {
		return nil, err
	}

	log.Printf("Reality Public Key: %s", secrets.PublicKey)
	log.Printf("Short ID: %s", secrets.ShortIDs[0])

	// 使用随机端口（如果环境变量未指定）
	ssPort := cfg.SSPort
	if ssPort == 8388 { // 默认值，使用随机端口
		ssPort = secrets.SSPort
	}
	log.Printf("VLESS Port: %d", cfg.VLESSPort)
	log.Printf("Shadowsocks Port: %d", ssPort)

	// 创建各个组件
	singboxAPIAddr := "127.0.0.1:10085"
	syncer := config.NewSyncer(cfg.APIURL, cfg.NodeAPIKey)
	cache := config.NewCache(dataDir)
	generator := config.NewGenerator(cfg.VLESSPort, ssPort, secrets.PrivateKey, secrets.ShortIDs)
	manager := singbox.NewManager(cfg.SingboxBin, cfg.SingboxConfig)
	connMgr := singbox.NewConnectionManager(singboxAPIAddr)
	collector := stats.NewCollector(singboxAPIAddr)
	reporter := stats.NewReporter(cfg.APIURL, cfg.NodeAPIKey, statsCache)

	agent := &Agent{
		cfg:       cfg,
		secrets:   secrets,
		syncer:    syncer,
		cache:     cache,
		generator: generator,
		manager:   manager,
		connMgr:   connMgr,
		collector: collector,
		reporter:  reporter,
	}

	// 更新配置中的实际端口
	cfg.SSPort = ssPort

	// 创建限额监控器（带移除回调）
	agent.monitor = quota.NewMonitor(func(uuid, reason string) {
		log.Printf("User quota exceeded: %s (%s), kicking...", uuid, reason)
		if kicked, err := connMgr.KickUser(uuid); err != nil {
			log.Printf("Failed to kick user %s: %v", uuid, err)
		} else if kicked > 0 {
			log.Printf("Kicked %d connections for user %s", kicked, uuid)
		}
	})

	return agent, nil
}

// Run 启动 Agent 主循环
func (a *Agent) Run(ctx context.Context) {
	// 启动健康检查服务
	healthServer := api.NewHealthServer(func() bool {
		return a.manager.IsRunning() || os.Getenv("SKIP_SINGBOX") == "true"
	})
	go func() {
		log.Println("Health server starting on :8080")
		if err := healthServer.Start(":8080"); err != nil {
			log.Printf("Health server error: %v", err)
		}
	}()

	// 节点注册
	if err := a.register(); err != nil {
		log.Printf("Node registration failed: %v", err)
	}

	// 首次同步配置
	if err := a.syncAndApply(); err != nil {
		log.Printf("Initial sync failed: %v", err)
		if a.cache.HasCache() {
			log.Println("Using cached configuration...")
			if err := a.applyFromCache(); err != nil {
				log.Printf("Failed to apply cache: %v", err)
			}
		}
	}

	// 启动 sing-box
	if os.Getenv("SKIP_SINGBOX") != "true" {
		if err := a.manager.Start(); err != nil {
			log.Printf("Failed to start sing-box: %v", err)
		}
	} else {
		log.Println("SKIP_SINGBOX=true, skipping sing-box start")
	}

	// 尝试上报缓存的统计
	if err := a.reporter.FlushCache(); err != nil {
		log.Printf("Failed to flush stats cache: %v", err)
	}

	// 定时器
	syncTicker := time.NewTicker(a.cfg.SyncInterval)           // 60s - 用户列表同步
	statsTicker := time.NewTicker(a.cfg.StatsInterval)         // 5min - 流量统计上报
	heartbeatTicker := time.NewTicker(30 * time.Second)        // 30s - 心跳
	connectionsTicker := time.NewTicker(10 * time.Second)      // 10s - 连接上报
	quotaTicker := time.NewTicker(10 * time.Second)            // 10s - 过期检查

	defer syncTicker.Stop()
	defer statsTicker.Stop()
	defer heartbeatTicker.Stop()
	defer connectionsTicker.Stop()
	defer quotaTicker.Stop()

	log.Printf("Agent is running (sync: %v, stats: %v)",
		a.cfg.SyncInterval, a.cfg.StatsInterval)

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping agent...")
			a.collectAndReport()
			a.manager.Stop()
			return

		case <-syncTicker.C:
			if err := a.syncAndApply(); err != nil {
				log.Printf("Sync error: %v", err)
			}

		case <-statsTicker.C:
			a.collectAndReport()

		case <-heartbeatTicker.C:
			a.sendHeartbeat()

		case <-connectionsTicker.C:
			a.reportConnections()

		case <-quotaTicker.C:
			a.monitor.CheckAllUsers()
		}
	}
}

// register 向管理服务器注册
func (a *Agent) register() error {
	return a.syncer.Register(
		a.cfg.NodeID,
		a.secrets.PublicKey,
		a.secrets.ShortIDs,
		a.cfg.VLESSPort,
		a.cfg.SSPort,
	)
}

// sendHeartbeat 发送心跳
func (a *Agent) sendHeartbeat() {
	// 获取系统负载
	sysLoad := stats.GetSystemLoad()

	// 获取连接数
	connections, _ := a.connMgr.GetActiveConnections()

	req := &config.HeartbeatRequest{
		NodeID:    a.cfg.NodeID,
		Timestamp: time.Now().UTC(),
		Load: config.NodeLoad{
			CPUPercent:        sysLoad.CPUPercent,
			MemoryPercent:     sysLoad.MemoryPercent,
			ActiveConnections: len(connections),
			UserCount:         a.monitor.GetUserCount(),
		},
	}

	resp, err := a.syncer.Heartbeat(req)
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}

	// 处理踢人指令
	if len(resp.KickUsers) > 0 {
		a.kickUsers(resp.KickUsers)
	}

	// 检查是否需要重新加载用户
	if resp.ReloadUsers {
		log.Println("Manager requested user reload")
		a.syncAndApply()
	}
}

// reportConnections 上报活跃连接
func (a *Agent) reportConnections() {
	connections, err := a.connMgr.GetActiveConnections()
	if err != nil {
		// sing-box 可能未运行
		return
	}

	if len(connections) == 0 {
		return
	}

	report := &config.ConnectionsReport{
		NodeID:      a.cfg.NodeID,
		Timestamp:   time.Now().UTC(),
		Connections: make([]config.Connection, 0, len(connections)),
	}

	for _, conn := range connections {
		// 解析客户端 IP
		clientIP := conn.Metadata.Source
		if host, _, err := net.SplitHostPort(clientIP); err == nil {
			clientIP = host
		}

		// 解析连接时间
		connectedAt, _ := time.Parse(time.RFC3339, conn.Start)

		report.Connections = append(report.Connections, config.Connection{
			UserUUID:    conn.Metadata.User,
			ClientIP:    clientIP,
			ConnectedAt: connectedAt,
			Upload:      conn.Upload,
			Download:    conn.Download,
		})
	}

	resp, err := a.syncer.ReportConnections(report)
	if err != nil {
		log.Printf("Report connections failed: %v", err)
		return
	}

	// 处理踢人指令
	if len(resp.KickUsers) > 0 {
		a.kickUsers(resp.KickUsers)
	}
}

// kickUsers 踢掉指定用户
func (a *Agent) kickUsers(uuids []string) {
	for _, uuid := range uuids {
		uuid = strings.TrimSpace(uuid)
		if uuid == "" {
			continue
		}

		kicked, err := a.connMgr.KickUser(uuid)
		if err != nil {
			log.Printf("Failed to kick user %s: %v", uuid, err)
		} else if kicked > 0 {
			log.Printf("Kicked %d connections for user %s (by Manager)", kicked, uuid)
		}
	}
}

// syncAndApply 同步配置并应用
func (a *Agent) syncAndApply() error {
	log.Println("Syncing configuration...")

	resp, err := a.syncer.FetchUsers()
	if err != nil {
		return err
	}

	a.mu.RLock()
	sameVersion := a.currentVersion == resp.Version
	a.mu.RUnlock()

	if sameVersion {
		log.Printf("Configuration unchanged (version: %s)", resp.Version)
		return nil
	}

	log.Printf("New configuration version: %s (%d users)", resp.Version, len(resp.Users))

	a.monitor.UpdateUsers(resp.Users)

	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}

	singboxCfg := a.generator.Generate(resp.Users, resp.Config.RealitySNI)

	if err := a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig); err != nil {
		return err
	}

	a.mu.Lock()
	a.currentVersion = resp.Version
	a.mu.Unlock()

	if a.manager.IsRunning() {
		log.Println("Reloading sing-box...")
		return a.manager.Reload()
	}

	return nil
}

// applyFromCache 从缓存应用配置
func (a *Agent) applyFromCache() error {
	resp, err := a.cache.LoadUsers()
	if err != nil {
		return err
	}

	a.monitor.UpdateUsers(resp.Users)

	singboxCfg := a.generator.Generate(resp.Users, resp.Config.RealitySNI)
	return a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig)
}

// collectAndReport 收集并上报统计
func (a *Agent) collectAndReport() {
	userStats, err := a.collector.Collect()
	if err != nil {
		log.Printf("Failed to collect stats: %v", err)
		return
	}

	if len(userStats) == 0 {
		return
	}

	log.Printf("Reporting stats for %d users", len(userStats))

	if err := a.reporter.Report(userStats); err != nil {
		log.Printf("Failed to report stats: %v", err)
	} else {
		a.monitor.ResetSessionTraffic()

		if a.reporter.GetCacheCount() > 0 {
			if err := a.reporter.FlushCache(); err != nil {
				log.Printf("Failed to flush stats cache: %v", err)
			}
		}
	}
}
