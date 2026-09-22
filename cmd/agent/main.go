package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"otun-node-agent/internal/api"
	"otun-node-agent/internal/config"
	"otun-node-agent/internal/enroll"
	"otun-node-agent/internal/local"
	"otun-node-agent/internal/quota"
	"otun-node-agent/internal/singbox"
	"otun-node-agent/internal/stats"
	"otun-node-agent/internal/tunnel"
)

// Version 由构建时注入：go build -ldflags "-X main.Version=v1.11.0"
//
// ⚠️ 不要改名或移动：CI(.github/workflows/release.yml)按此路径注入，
// 注册请求的 agent_version 用它，后端据此做 agent_too_old 判定
// （接线契约 §4.1）。未注入时为 dev，后端会拒绝注册。
var Version = "dev"

// Agent 是主控制器
type Agent struct {
	cfg       *config.AgentConfig
	secrets   *config.NodeSecrets
	syncer    *config.Syncer
	cache     *config.Cache
	generator *config.Generator
	manager   *singbox.Manager
	connMgr   *singbox.ConnectionManager
	monitor   *quota.Monitor
	collector *stats.Collector
	reporter  *stats.Reporter

	// 本地用户管理
	localStore *local.Store
	localAPI   *api.LocalAPIServer

	// Token 接入（外连模式）。nil 表示走存量 API-Key 路径。
	enrollRunner *enroll.Runner

	// 反向隧道（tunnel v1）。App 经它 SSH 到本机。
	tunnelMgr *tunnel.Manager
	sshServer *tunnel.Server

	currentVersion string
	revoked        bool
	// shutdown 收束 Run 的根 context：解绑是终态，必须立刻退出，
	// 不能等到收到信号才被发现（接线契约 §5.3）。
	shutdown context.CancelFunc
	mu       sync.RWMutex
}

// Revoked 报告节点是否已被后端解绑。
func (a *Agent) Revoked() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.revoked
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

	// 登记收束句柄：解绑时由 enroll 回调触发，Run 随即返回。
	agent.mu.Lock()
	agent.shutdown = cancel
	agent.mu.Unlock()

	// 启动 Agent
	agent.Run(ctx)

	// 节点被后端解绑：停服、清凭据，并以约定退出码结束。
	// systemd 单元配了 RestartPreventExitStatus=3，据此不再拉起 ——
	// 否则会每 5 秒去打一个已吊销的节点（接线契约 §5.3）。
	if agent.Revoked() {
		log.Println("节点已解绑，agent 退出且不再重启")
		os.Exit(enroll.ExitRevoked)
	}
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
			NodeID:     cfg.NodeID,
			ServerIP:   cfg.ServerIP,
			PublicKey:  secrets.PublicKey,
			ShortID:    secrets.ShortIDs[0],
			VLESSPort:  cfg.VLESSPort,
			SSPort:     ssPort,
			SSMethod:   "chacha20-ietf-poly1305",
			RealitySNI: cfg.RealitySNI,
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

		// Token 接入：额外起一条到后端的长轮询通道。
		//
		// 它与本地用户管理并存 —— 指令最终也是落到同一个 local.Store，
		// 所以存量的 /api/local/* 与新的后端下发不会打架。
		// 未配置 token 且无 node.json 时静默跳过，存量节点行为不变。
		if err := a.startEnrollment(ctx); err != nil {
			log.Printf("[enroll] 未启用 Token 接入: %v", err)
		}

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
	singboxCfg := a.generator.Generate(users, a.cfg.RealitySNI)

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

// ── Token 接入（接线契约 §2、§4、§5）────────────────────────

// nodeInfo 把 Agent 已有的状态暴露给 enroll 包，避免它反向依赖 main。
type nodeInfo struct{ a *Agent }

func (n nodeInfo) ListenPorts() map[string]int {
	ports := map[string]int{"vless": n.a.cfg.VLESSPort}
	if p := n.a.effectiveSSPort(); p > 0 {
		ports["ss"] = p
	}
	return ports
}

func (n nodeInfo) Secrets() enroll.NodeSecrets {
	return enroll.NodeSecrets{
		RealityPublicKey: n.a.secrets.PublicKey,
		ShortIDs:         n.a.secrets.ShortIDs,
		// 与配置生成器同一来源，不写死（契约 §8）
		RealitySNI: n.a.cfg.RealitySNI,
		SSMethod:   "chacha20-ietf-poly1305",
	}
}

func (n nodeInfo) SingboxVersion() string { return n.a.singboxVersion() }

// statsAdapter 把 stats.Collector 的输出转成契约定义的形状。
type statsAdapter struct{ c *stats.Collector }

func (s statsAdapter) Collect() (map[string]enroll.UserStat, error) {
	raw, err := s.c.Collect()
	if err != nil {
		return nil, err
	}
	out := make(map[string]enroll.UserStat, len(raw))
	for uuid, st := range raw {
		// 累计值原样上报，不做差、不清零（契约 §5.1、§7）
		out[uuid] = enroll.UserStat{UUID: uuid, Up: st.Upload, Down: st.Download}
	}
	return out, nil
}

type storeAdapter struct{ s *local.Store }

func (s storeAdapter) UserCount() int {
	if s.s == nil {
		return 0
	}
	return s.s.GetUserCount()
}

// startEnrollment 在有 token 或已接入时启动长轮询通道。
func (a *Agent) startEnrollment(ctx context.Context) error {
	dataDir := "./data"
	token := os.Getenv("OTUN_ENROLL_TOKEN")

	// 既无 token 也未接入 → 存量 API-Key 路径，什么都不做
	existing, _ := enroll.LoadNodeFile(dataDir)
	if token == "" && existing == nil {
		return errors.New("未配置 OTUN_ENROLL_TOKEN 且未接入")
	}

	info := nodeInfo{a: a}

	exePath, err := os.Executable()
	if err != nil {
		// 拿不到路径就不开自升级，其余功能照常 —— 宁可不能远程升级，
		// 也不要在错误的路径上覆盖文件
		log.Printf("[upgrade] 无法确定可执行文件路径，自升级已禁用：%v", err)
		exePath = ""
	}

	executor := enroll.NewExecutor(a.localStore, info, Version, a.reloadForCommand)
	if exePath != "" {
		executor.EnableUpgrade(&enroll.UpgradeConfig{
			Repo:    "antsbtw/sing-box-docker",
			ExePath: exePath,
		})
	}

	runner := &enroll.Runner{
		DataDir:        dataDir,
		ExePath:        exePath,
		APIURL:         a.cfg.APIURL,
		AgentVersion:   Version,
		Token:          token,
		Client:         enroll.NewClient(a.cfg.APIURL, Version),
		Executor:       executor,
		Info:           info,
		Stats:          statsAdapter{c: a.collector},
		Store:          storeAdapter{s: a.localStore},
		SingboxRunning: a.manager.IsRunning,
	}

	// ── 反向隧道（tunnel v1）──────────────────────────────
	//
	// 内置 SSH 服务端不监听任何网络端口（S-1）：它只从隧道 WS 收连接，
	// 所以不会扩大这台机器的攻击面。
	var setupTunnel func(nodeID, nodeSecret string) error
	setupTunnel = func(nodeID, nodeSecret string) error {
		srv, err := tunnel.NewServer(nodeID, nodeSecret, tunnel.NewCredVerifier())
		if err != nil {
			return err
		}
		// 凭据有效期以**后端时间**判定，不信本机时钟（契约 §3.2 第 4 条）
		srv.ServerTime = time.Now

		a.sshServer = srv
		a.tunnelMgr = tunnel.NewManager(nodeID, nodeSecret, srv, 3)
		executor.EnableTunnel(a.tunnelMgr)
		return nil
	}

	applyTunnelKey := func(pubkeyB64, keyID string) {
		if a.sshServer == nil {
			return
		}
		pub, err := tunnel.ParsePubkey(pubkeyB64)
		if err != nil {
			log.Printf("[tunnel] 后端公钥无法解析：%v", err)
			return
		}
		a.sshServer.SetTunnelKey(pub, keyID)
		log.Printf("[tunnel] 已更新隧道签名公钥 (kid=%s)", keyID)
	}

	// 已接入的节点：先用 node.json 里缓存的公钥建起来，
	// 不必等第一次 poll —— 否则刚重启就开终端会被拒。
	if existing != nil {
		if err := setupTunnel(existing.NodeID, existing.NodeSecret); err != nil {
			log.Printf("[tunnel] 初始化失败：%v", err)
		} else if existing.TunnelPubkey != "" {
			applyTunnelKey(existing.TunnelPubkey, existing.TunnelKeyID)
		}
	}
	runner.OnTunnelKey = applyTunnelKey

	if err := runner.Prepare(ctx); err != nil {
		// 注册终态（token 已废、版本过低）：不能让进程继续以普通 local 模式
		// 活着 —— systemctl status 会显示 active，看着正常实则没接入。
		// 以退出码 3 结束，配合 RestartPreventExitStatus=3 不再拉起（契约 §4.3）。
		if errors.Is(err, enroll.ErrEnrollFatal) {
			log.Printf("[enroll] 注册被拒绝，agent 退出且不再重启：%v", err)
			os.Exit(enroll.ExitRevoked)
		}
		return err
	}
	a.enrollRunner = runner

	// 首次注册：此时才有 node_id / node_secret，补建隧道
	if a.sshServer == nil {
		if nf, err := enroll.LoadNodeFile(dataDir); err == nil && nf != nil {
			if err := setupTunnel(nf.NodeID, nf.NodeSecret); err != nil {
				log.Printf("[tunnel] 初始化失败：%v", err)
			} else if nf.TunnelPubkey != "" {
				applyTunnelKey(nf.TunnelPubkey, nf.TunnelKeyID)
			}
		}
	}

	go func() {
		err := runner.Run(ctx)

		// 新版本已就位且回执已确认：以 0 退出，systemd 拉起新二进制。
		// 用 0 而非 3 —— 3 会触发 RestartPreventExitStatus 不再重启（§7.4-1）。
		if errors.Is(err, enroll.ErrUpgraded) {
			log.Println("[upgrade] 退出以启用新版本")
			_ = a.manager.Stop()
			os.Exit(0)
		}

		if errors.Is(err, enroll.ErrRevoked) {
			a.handleRevoked(runner.Cleanup)
		}
	}()
	return nil
}

// handleRevoked 执行解绑终态：清用户 → 停数据面 → 清凭据 → 收束主循环（契约 §5.3）。
//
// 先清用户再停服务：不清的话机器重启后 agent 会以普通 local 模式
// 把老用户全部重新拉起，一个已删除的节点继续给人当出口。
//
// 最后必须收束主循环 —— 只置 revoked 标志是不够的：Run 仅在收到信号时返回，
// main 里的退出码 3 检查永远执行不到，进程会一直挂着（后端联调实测）。
func (a *Agent) handleRevoked(cleanup func()) {
	log.Println("[enroll] 收到解绑指令，关闭隧道、清除本地用户并停止 sing-box")
	if a.tunnelMgr != nil {
		a.tunnelMgr.CloseAll()
	}
	if a.localStore != nil {
		if err := a.localStore.Clear(); err != nil {
			log.Printf("[enroll] 清除本地用户失败：%v", err)
		}
	}
	if a.manager != nil {
		_ = a.manager.Stop()
	}
	if cleanup != nil {
		cleanup()
	}

	a.mu.Lock()
	a.revoked = true
	stop := a.shutdown
	a.mu.Unlock()

	if stop != nil {
		stop()
	}
}

// reloadForCommand 供 reload 指令调用：重新生成配置并应用。
func (a *Agent) reloadForCommand() error {
	a.regenerateConfig()
	return nil
}

// effectiveSSPort 返回实际监听的 SS 端口（可能是随机分配的）。
func (a *Agent) effectiveSSPort() int {
	if a.cfg.SSPort != 8388 {
		return a.cfg.SSPort
	}
	if a.secrets != nil {
		return a.secrets.SSPort
	}
	return a.cfg.SSPort
}

// singboxVersion 取 `sing-box version` 的版本号，供注册上报（契约 §4.1）。
func (a *Agent) singboxVersion() string {
	out, err := exec.Command(a.cfg.SingboxBin, "version").Output()
	if err != nil {
		return "unknown"
	}
	// 首行形如：sing-box version 1.10.7
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	if fields := strings.Fields(line); len(fields) >= 3 {
		return fields[2]
	}
	return "unknown"
}
