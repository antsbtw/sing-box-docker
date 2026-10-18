package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// HealthServer 提供健康检查接口
type HealthServer struct {
	startTime time.Time
	isHealthy func() bool
}

// NewHealthServer 创建健康检查服务
func NewHealthServer(isHealthy func() bool) *HealthServer {
	return &HealthServer{
		startTime: time.Now(),
		isHealthy: isHealthy,
	}
}

// Start 启动健康检查 HTTP 服务
func (s *HealthServer) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.HandleHealth)
	mux.HandleFunc("/ready", s.HandleReady)

	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}

	return server.ListenAndServe()
}

// HandleHealth 处理健康检查请求
func (s *HealthServer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	code := http.StatusOK

	if !s.isHealthy() {
		status = "unhealthy"
		code = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status":  status,
		"uptime":  time.Since(s.startTime).String(),
		"version": "1.1.0",
		// App 据此判断这台 agent 支持哪些功能。上面的 version 是写死的
		// API 版本号，不随 release 变，不能拿来判断新旧。
		"features": []string{"residential_exit"},
	})
}

// HandleReady 处理就绪检查请求
func (s *HealthServer) HandleReady(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
