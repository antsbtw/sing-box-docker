package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"otun-node-agent/internal/local"
)

// LocalAPIServer 本地管理 API 服务
type LocalAPIServer struct {
	store      *local.Store
	apiKey     string
	nodeConfig *NodeConfig
}

// NodeConfig 节点配置信息
type NodeConfig struct {
	NodeID    string `json:"node_id"`
	ServerIP  string `json:"server_ip"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
	VLESSPort int    `json:"vless_port"`
	SSPort    int    `json:"ss_port"`
	SSMethod  string `json:"ss_method"`
}

// NewLocalAPIServer 创建本地 API 服务
func NewLocalAPIServer(store *local.Store, apiKey string, nodeConfig *NodeConfig) *LocalAPIServer {
	return &LocalAPIServer{
		store:      store,
		apiKey:     apiKey,
		nodeConfig: nodeConfig,
	}
}

// RegisterRoutes 注册路由到 mux
func (s *LocalAPIServer) RegisterRoutes(mux *http.ServeMux) {
	// 用户管理
	mux.HandleFunc("/api/local/users", s.authMiddleware(s.handleUsers))
	mux.HandleFunc("/api/local/users/", s.authMiddleware(s.handleUserByID))

	// 节点配置
	mux.HandleFunc("/api/local/config", s.authMiddleware(s.handleConfig))

	// 流量统计
	mux.HandleFunc("/api/local/stats", s.authMiddleware(s.handleStats))
}

// authMiddleware Bearer Token 认证中间件
func (s *LocalAPIServer) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			s.jsonError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		parts := strings.SplitN(auth, " ", 2)
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			s.jsonError(w, http.StatusUnauthorized, "invalid authorization format")
			return
		}

		if parts[1] != s.apiKey {
			s.jsonError(w, http.StatusUnauthorized, "invalid api key")
			return
		}

		next(w, r)
	}
}

// handleUsers 处理 /api/local/users
func (s *LocalAPIServer) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w, r)
	case http.MethodPost:
		s.createUser(w, r)
	default:
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleUserByID 处理 /api/local/users/{uuid}
func (s *LocalAPIServer) handleUserByID(w http.ResponseWriter, r *http.Request) {
	// 提取 UUID
	path := strings.TrimPrefix(r.URL.Path, "/api/local/users/")
	uuid := strings.TrimSuffix(path, "/")

	if uuid == "" {
		s.jsonError(w, http.StatusBadRequest, "missing user uuid")
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getUser(w, r, uuid)
	case http.MethodPut:
		s.updateUser(w, r, uuid)
	case http.MethodDelete:
		s.deleteUser(w, r, uuid)
	default:
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// listUsers 获取用户列表
func (s *LocalAPIServer) listUsers(w http.ResponseWriter, r *http.Request) {
	users := s.store.ListUsers()

	// 转换为响应格式，包含连接 URL
	response := make([]UserResponse, 0, len(users))
	for _, u := range users {
		response = append(response, s.toUserResponse(&u))
	}

	s.jsonSuccess(w, map[string]any{
		"users": response,
		"total": len(response),
	})
}

// createUser 创建用户
func (s *LocalAPIServer) createUser(w http.ResponseWriter, r *http.Request) {
	var req local.CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		s.jsonError(w, http.StatusBadRequest, "name is required")
		return
	}

	user, err := s.store.CreateUser(&req)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.jsonSuccess(w, s.toUserResponse(user))
}

// getUser 获取单个用户
func (s *LocalAPIServer) getUser(w http.ResponseWriter, r *http.Request, uuid string) {
	user, ok := s.store.GetUser(uuid)
	if !ok {
		s.jsonError(w, http.StatusNotFound, "user not found")
		return
	}

	s.jsonSuccess(w, s.toUserResponse(user))
}

// updateUser 更新用户
func (s *LocalAPIServer) updateUser(w http.ResponseWriter, r *http.Request, uuid string) {
	var req local.UpdateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.jsonError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	user, err := s.store.UpdateUser(uuid, &req)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			s.jsonError(w, http.StatusNotFound, err.Error())
		} else {
			s.jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	s.jsonSuccess(w, s.toUserResponse(user))
}

// deleteUser 删除用户
func (s *LocalAPIServer) deleteUser(w http.ResponseWriter, r *http.Request, uuid string) {
	if err := s.store.DeleteUser(uuid); err != nil {
		if strings.Contains(err.Error(), "not found") {
			s.jsonError(w, http.StatusNotFound, err.Error())
		} else {
			s.jsonError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	s.jsonSuccess(w, map[string]any{
		"message": "user deleted",
		"uuid":    uuid,
	})
}

// handleConfig 获取节点配置
func (s *LocalAPIServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	s.jsonSuccess(w, s.nodeConfig)
}

// handleStats 获取流量统计（预留接口）
func (s *LocalAPIServer) handleStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	users := s.store.ListUsers()

	stats := make([]map[string]any, 0, len(users))
	for _, u := range users {
		stats = append(stats, map[string]any{
			"uuid":          u.UUID,
			"name":          u.Name,
			"traffic_used":  u.TrafficUsed,
			"traffic_limit": u.TrafficLimit,
		})
	}

	s.jsonSuccess(w, map[string]any{
		"stats": stats,
	})
}

// UserResponse 用户响应格式
type UserResponse struct {
	UUID         string     `json:"uuid"`
	Name         string     `json:"name"`
	Protocols    []string   `json:"protocols"`
	SSPassword   string     `json:"ss_password"`
	Enabled      bool       `json:"enabled"`
	TrafficLimit int64      `json:"traffic_limit"`
	TrafficUsed  int64      `json:"traffic_used"`
	ExpireAt     *time.Time `json:"expire_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	// 连接 URL
	VLESSUrl string `json:"vless_url,omitempty"`
	SSUrl    string `json:"ss_url,omitempty"`
}

// toUserResponse 转换为响应格式
func (s *LocalAPIServer) toUserResponse(u *local.LocalUser) UserResponse {
	resp := UserResponse{
		UUID:         u.UUID,
		Name:         u.Name,
		Protocols:    u.Protocols,
		SSPassword:   u.SSPassword,
		Enabled:      u.Enabled,
		TrafficLimit: u.TrafficLimit,
		TrafficUsed:  u.TrafficUsed,
		ExpireAt:     u.ExpireAt,
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}

	// 生成连接 URL
	for _, proto := range u.Protocols {
		switch proto {
		case "vless":
			resp.VLESSUrl = s.generateVLESSUrl(u)
		case "shadowsocks":
			resp.SSUrl = s.generateSSUrl(u)
		}
	}

	return resp
}

// generateVLESSUrl 生成 VLESS 连接 URL
// 格式: vless://uuid@server:port?encryption=none&flow=xtls-rprx-vision&security=reality&sni=sni&fp=chrome&pbk=publickey&sid=shortid&type=tcp#name
func (s *LocalAPIServer) generateVLESSUrl(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" {
		return ""
	}

	params := url.Values{}
	params.Set("encryption", "none")
	params.Set("flow", "xtls-rprx-vision")
	params.Set("security", "reality")
	params.Set("sni", "www.microsoft.com") // 默认 SNI
	params.Set("fp", "chrome")
	params.Set("pbk", s.nodeConfig.PublicKey)
	params.Set("sid", s.nodeConfig.ShortID)
	params.Set("type", "tcp")

	return fmt.Sprintf("vless://%s@%s:%d?%s#%s",
		u.UUID,
		s.nodeConfig.ServerIP,
		s.nodeConfig.VLESSPort,
		params.Encode(),
		url.PathEscape(u.Name),
	)
}

// generateSSUrl 生成 Shadowsocks 连接 URL
// 格式: ss://base64(method:password)@server:port#name
func (s *LocalAPIServer) generateSSUrl(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" {
		return ""
	}

	method := s.nodeConfig.SSMethod
	if method == "" {
		method = "chacha20-ietf-poly1305"
	}

	// base64 编码 method:password
	userInfo := fmt.Sprintf("%s:%s", method, u.SSPassword)
	encoded := base64.URLEncoding.EncodeToString([]byte(userInfo))

	return fmt.Sprintf("ss://%s@%s:%d#%s",
		encoded,
		s.nodeConfig.ServerIP,
		s.nodeConfig.SSPort,
		url.PathEscape(u.Name),
	)
}

// jsonSuccess 返回成功响应
func (s *LocalAPIServer) jsonSuccess(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(data)
}

// jsonError 返回错误响应
func (s *LocalAPIServer) jsonError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error":   true,
		"message": message,
	})
}
