package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"otun-node-agent/internal/api"
	"otun-node-agent/internal/config"
	"otun-node-agent/internal/local"
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

	// 本地用户管理
	localStore *local.Store
	localAPI   *api.LocalAPIServer

	currentVersion string
	mu             sync.RWMutex
}

func main() {
	log.Println("========================================")
	log.Println("  OTun Node Agent v1.1.0")
	log.Println("========================================")

	// 加载配置
	cfg := config.LoadFromEnv()
	if cfg.NodeAPIKey == "" {
		log.Fatal("NODE_API_KEY is required")
	}

	log.Printf("Node ID: %s", cfg.NodeID)
	log.Printf("Management Mode: %s", cfg.ManagementMode)

	// 只在远程/混合模式下显示 API URL
	if cfg.ManagementMode == config.ModeRemote || cfg.ManagementMode == config.ModeHybrid {
		log.Printf("API URL: %s", cfg.APIURL)
	}

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

	// 本地/混合模式：初始化本地用户存储
	if cfg.ManagementMode == config.ModeLocal || cfg.ManagementMode == config.ModeHybrid {
		agent.localStore = local.NewStore(dataDir, func() {
			// 用户变更回调：重新生成配置
			log.Println("Local users changed, regenerating config...")
			agent.regenerateConfig()
		})

		// 创建本地 API 服务
		nodeConfig := &api.NodeConfig{
			NodeID:    cfg.NodeID,
			ServerIP:  cfg.ServerIP,
			PublicKey: secrets.PublicKey,
			ShortID:   secrets.ShortIDs[0],
			VLESSPort: cfg.VLESSPort,
			SSPort:    ssPort,
			SSMethod:  "chacha20-ietf-poly1305",
		}
		agent.localAPI = api.NewLocalAPIServer(agent.localStore, cfg.NodeAPIKey, nodeConfig)

		log.Printf("Local management API enabled")
		if cfg.ServerIP != "" {
			log.Printf("Server IP: %s", cfg.ServerIP)
		} else {
			log.Printf("Warning: SERVER_IP not set, connection URLs will be incomplete")
		}
	}

	return agent, nil
}

// Run 启动 Agent 主循环
func (a *Agent) Run(ctx context.Context) {
	// 启动 HTTP 服务（健康检查 + 本地 API）
	a.startHTTPServer()

	// 根据管理模式执行不同的初始化
	switch a.cfg.ManagementMode {
	case config.ModeLocal:
		// 本地模式：只使用本地用户
		log.Println("Running in LOCAL mode")
		a.initLocalMode()

	case config.ModeRemote:
		// 远程模式：与原来行为一致
		log.Println("Running in REMOTE mode")
		a.initRemoteMode()

	case config.ModeHybrid:
		// 混合模式：本地 + 远程
		log.Println("Running in HYBRID mode")
		a.initHybridMode()
	}

	// 启动 sing-box
	if os.Getenv("SKIP_SINGBOX") != "true" {
		if err := a.manager.Start(); err != nil {
			log.Printf("Failed to start sing-box: %v", err)
		}
	} else {
		log.Println("SKIP_SINGBOX=true, skipping sing-box start")
	}

	// 启动主循环
	a.runMainLoop(ctx)
}

// startHTTPServer 启动 HTTP 服务
func (a *Agent) startHTTPServer() {
	mux := http.NewServeMux()

	// 健康检查
	healthServer := api.NewHealthServer(func() bool {
		return a.manager.IsRunning() || os.Getenv("SKIP_SINGBOX") == "true"
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		healthServer.HandleHealth(w, r)
	})
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		healthServer.HandleReady(w, r)
	})

	// 注册本地 API 路由（如果启用）
	if a.localAPI != nil {
		a.localAPI.RegisterRoutes(mux)
		log.Println("Local API routes registered")
	}

	go func() {
		log.Println("HTTP server starting on :8080")
		server := &http.Server{
			Addr:         ":8080",
			Handler:      mux,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
		}
		if err := server.ListenAndServe(); err != nil {
			log.Printf("HTTP server error: %v", err)
		}
	}()
}

// initLocalMode 初始化本地模式
func (a *Agent) initLocalMode() {
	// 从本地用户生成配置
	a.regenerateConfig()
}

// initRemoteMode 初始化远程模式
func (a *Agent) initRemoteMode() {
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

	// 尝试上报缓存的统计
	if err := a.reporter.FlushCache(); err != nil {
		log.Printf("Failed to flush stats cache: %v", err)
	}
}

// initHybridMode 初始化混合模式
func (a *Agent) initHybridMode() {
	// 节点注册
	if err := a.register(); err != nil {
		log.Printf("Node registration failed: %v", err)
	}

	// 同步远程用户并合并本地用户
	if err := a.syncAndApplyHybrid(); err != nil {
		log.Printf("Initial sync failed: %v", err)
		// 回退到本地用户
		a.regenerateConfig()
	}

	// 尝试上报缓存的统计
	if err := a.reporter.FlushCache(); err != nil {
		log.Printf("Failed to flush stats cache: %v", err)
	}
}

// runMainLoop 主循环
func (a *Agent) runMainLoop(ctx context.Context) {
	// 根据模式决定是否启用远程同步定时器
	var syncTicker *time.Ticker
	var statsTicker *time.Ticker
	var heartbeatTicker *time.Ticker
	var connectionsTicker *time.Ticker

	if a.cfg.ManagementMode == config.ModeRemote || a.cfg.ManagementMode == config.ModeHybrid {
		syncTicker = time.NewTicker(a.cfg.SyncInterval)
		statsTicker = time.NewTicker(a.cfg.StatsInterval)
		heartbeatTicker = time.NewTicker(30 * time.Second)
		connectionsTicker = time.NewTicker(10 * time.Second)
		defer syncTicker.Stop()
		defer statsTicker.Stop()
		defer heartbeatTicker.Stop()
		defer connectionsTicker.Stop()
	}

	quotaTicker := time.NewTicker(10 * time.Second)
	defer quotaTicker.Stop()

	log.Printf("Agent is running (mode: %s)", a.cfg.ManagementMode)

	for {
		select {
		case <-ctx.Done():
			log.Println("Stopping agent...")
			if a.cfg.ManagementMode != config.ModeLocal {
				a.collectAndReport()
			}
			a.manager.Stop()
			return

		case <-quotaTicker.C:
			a.monitor.CheckAllUsers()

		default:
			// 远程/混合模式的定时任务
			if syncTicker != nil {
				select {
				case <-syncTicker.C:
					if a.cfg.ManagementMode == config.ModeHybrid {
						if err := a.syncAndApplyHybrid(); err != nil {
							log.Printf("Sync error: %v", err)
						}
					} else {
						if err := a.syncAndApply(); err != nil {
							log.Printf("Sync error: %v", err)
						}
					}
				case <-statsTicker.C:
					a.collectAndReport()
				case <-heartbeatTicker.C:
					a.sendHeartbeat()
				case <-connectionsTicker.C:
					a.reportConnections()
				default:
				}
			}

			// 小睡一下避免 CPU 空转
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// regenerateConfig 重新生成 sing-box 配置（从本地用户）
func (a *Agent) regenerateConfig() {
	if a.localStore == nil {
		return
	}

	localUsers := a.localStore.ListUsers()
	users := make([]config.User, 0, len(localUsers))

	for _, lu := range localUsers {
		users = append(users, config.User{
			UUID:         lu.UUID,
			Protocols:    lu.Protocols,
			SSPassword:   lu.SSPassword,
			Enabled:      lu.Enabled,
			TrafficLimit: lu.TrafficLimit,
			TrafficUsed:  lu.TrafficUsed,
			ExpireAt:     lu.ExpireAt,
		})
	}

	log.Printf("Regenerating config with %d local users", len(users))

	// 更新限额监控
	a.monitor.UpdateUsers(users)

	// 生成配置
	singboxCfg := a.generator.Generate(users, "www.microsoft.com")

	if err := a.generator.WriteToFile(singboxCfg, a.cfg.SingboxConfig); err != nil {
		log.Printf("Failed to write config: %v", err)
		return
	}

	// 重载 sing-box
	if a.manager.IsRunning() {
		log.Println("Reloading sing-box...")
		if err := a.manager.Reload(); err != nil {
			log.Printf("Failed to reload sing-box: %v", err)
		}
	}
}

// syncAndApplyHybrid 混合模式：同步远程用户并合并本地用户
func (a *Agent) syncAndApplyHybrid() error {
	log.Println("Syncing configuration (hybrid mode)...")

	// 获取远程用户
	resp, err := a.syncer.FetchUsers()
	if err != nil {
		return err
	}

	// 获取本地用户
	localUsers := a.localStore.ListUsers()

	// 合并用户列表（本地优先级更高）
	userMap := make(map[string]config.User)

	// 先添加远程用户
	for _, u := range resp.Users {
		userMap[u.UUID] = u
	}

	// 再添加本地用户（覆盖同 UUID 的远程用户）
	for _, lu := range localUsers {
		userMap[lu.UUID] = config.User{
			UUID:         lu.UUID,
			Protocols:    lu.Protocols,
			SSPassword:   lu.SSPassword,
			Enabled:      lu.Enabled,
			TrafficLimit: lu.TrafficLimit,
			TrafficUsed:  lu.TrafficUsed,
			ExpireAt:     lu.ExpireAt,
		}
	}

	// 转换为列表
	users := make([]config.User, 0, len(userMap))
	for _, u := range userMap {
		users = append(users, u)
	}

	log.Printf("Merged users: %d remote + %d local = %d total",
		len(resp.Users), len(localUsers), len(users))

	// 更新限额监控
	a.monitor.UpdateUsers(users)

	// 缓存远程用户
	if err := a.cache.SaveUsers(resp); err != nil {
		log.Printf("Failed to cache users: %v", err)
	}

	// 生成配置
	singboxCfg := a.generator.Generate(users, resp.Config.RealitySNI)

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
		if a.cfg.ManagementMode == config.ModeHybrid {
			a.syncAndApplyHybrid()
		} else {
			a.syncAndApply()
		}
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
