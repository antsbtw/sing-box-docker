package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"otun-node-agent/internal/config"
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
	// Reality SNI。与生成器用的是同一个值（来自 REALITY_SNI 配置），
	// 不能各写死一份 —— 两边不一致客户端就连不上（接线契约 §8）。
	RealitySNI string `json:"reality_sni"`
	// 新增四协议端口。为 0 表示该协议未启用，不生成对应链接。
	VMessPort     int `json:"vmess_port"`
	TrojanPort    int `json:"trojan_port"`
	Hysteria2Port int `json:"hysteria2_port"`
	TUICPort      int `json:"tuic_port"`
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

	// 订阅（需 API key，App 用）：返回某用户全部协议链接的 base64 汇总
	mux.HandleFunc("/api/local/subscription/", s.authMiddleware(s.handleSubscription))

	// 公开订阅（**免鉴权**，给第三方客户端订阅）：/sub/{token}
	// token 从 uuid+ss_password 派生，不可遍历；泄露只影响该用户。
	// 第三方客户端填 http://<ip>:8080/sub/<token> 即可一键导入六协议。
	mux.HandleFunc("/sub/", s.handlePublicSubscription)
}

// handlePublicSubscription 处理公开订阅 /sub/{token}（免鉴权）。
// 用 token 反查用户（遍历匹配派生值），返回其六协议订阅的 base64。
func (s *LocalAPIServer) handlePublicSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sub/"), "/")
	if token == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// 遍历用户，匹配派生 token。用户数通常很少，线性查找足够。
	var matched *local.LocalUser
	for _, u := range s.store.ListUsers() {
		if config.SubscriptionToken(u.UUID, u.SSPassword) == token {
			uu := u
			matched = &uu
			break
		}
	}
	if matched == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	sub := s.toUserResponse(matched).SubscriptionUrl
	// 标准订阅响应：text/plain，base64 内容。部分客户端认 profile 头。
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Profile-Update-Interval", "24")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(sub))
}

// handleSubscription 处理 /api/local/subscription/{uuid}
// 返回该用户全部协议链接的 base64 汇总（主流客户端订阅格式）。
func (s *LocalAPIServer) handleSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	uuid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/local/subscription/"), "/")
	if uuid == "" {
		s.jsonError(w, http.StatusBadRequest, "missing user uuid")
		return
	}
	user, ok := s.store.GetUser(uuid)
	if !ok {
		s.jsonError(w, http.StatusNotFound, "user not found")
		return
	}
	sub := s.toUserResponse(user).SubscriptionUrl
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(sub))
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
	VLESSUrl        string `json:"vless_url,omitempty"`
	SSUrl           string `json:"ss_url,omitempty"`
	VMessUrl        string `json:"vmess_url,omitempty"`
	TrojanUrl       string `json:"trojan_url,omitempty"`
	Hysteria2Url    string `json:"hysteria2_url,omitempty"`
	TUICUrl         string `json:"tuic_url,omitempty"`
	SubscriptionUrl string `json:"subscription_url,omitempty"`
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
	var links []string
	for _, proto := range u.Protocols {
		switch proto {
		case "vless":
			resp.VLESSUrl = s.generateVLESSUrl(u)
			links = appendIf(links, resp.VLESSUrl)
		case "shadowsocks":
			resp.SSUrl = s.generateSSUrl(u)
			links = appendIf(links, resp.SSUrl)
		case "vmess":
			resp.VMessUrl = s.generateVMessUrl(u)
			links = appendIf(links, resp.VMessUrl)
		case "trojan":
			resp.TrojanUrl = s.generateTrojanUrl(u)
			links = appendIf(links, resp.TrojanUrl)
		case "hysteria2":
			resp.Hysteria2Url = s.generateHysteria2Url(u)
			links = appendIf(links, resp.Hysteria2Url)
		case "tuic":
			resp.TUICUrl = s.generateTUICUrl(u)
			links = appendIf(links, resp.TUICUrl)
		}
	}

	// 订阅链接：把该用户全部协议链接 base64 汇总，主流客户端可一键导入。
	if len(links) > 0 {
		resp.SubscriptionUrl = base64.StdEncoding.EncodeToString([]byte(strings.Join(links, "\n")))
	}

	return resp
}

func appendIf(links []string, url string) []string {
	if url != "" {
		return append(links, url)
	}
	return links
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
	// 取实际配置值，不写死（契约 §8）
	sni := s.nodeConfig.RealitySNI
	if sni == "" {
		// 与 config.LoadFromEnv 的默认值保持一致 —— 两处不一样
		// 就会"服务端用 A、分享链接写 B"，客户端必然连不上。
		sni = "www.apple.com"
	}
	params.Set("sni", sni)
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

// generateVMessUrl 生成 VMess 连接 URL（WS，无 TLS）。
// 格式: vmess://base64(json)
func (s *LocalAPIServer) generateVMessUrl(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" || s.nodeConfig.VMessPort == 0 {
		return ""
	}
	v := map[string]any{
		"v":    "2",
		"ps":   u.Name,
		"add":  s.nodeConfig.ServerIP,
		"port": fmt.Sprintf("%d", s.nodeConfig.VMessPort),
		"id":   config.VMessUUID(u.UUID),
		"aid":  "0",
		"net":  "ws",
		"type": "none",
		"host": "",
		"path": "/vmess",
		"tls":  "",
	}
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return "vmess://" + base64.StdEncoding.EncodeToString(data)
}

// generateTrojanUrl 生成 Trojan 连接 URL（自签 TLS，客户端 insecure）。
// 格式: trojan://password@server:port?security=tls&allowInsecure=1&sni=...&type=tcp#name
func (s *LocalAPIServer) generateTrojanUrl(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" || s.nodeConfig.TrojanPort == 0 {
		return ""
	}
	params := url.Values{}
	params.Set("security", "tls")
	params.Set("allowInsecure", "1")
	params.Set("sni", config.SelfCertCN)
	params.Set("type", "tcp")
	return fmt.Sprintf("trojan://%s@%s:%d?%s#%s",
		config.TrojanPassword(u.UUID),
		s.nodeConfig.ServerIP,
		s.nodeConfig.TrojanPort,
		params.Encode(),
		url.PathEscape(u.Name),
	)
}

// generateHysteria2Url 生成 Hysteria2 连接 URL（UDP/QUIC，自签 TLS）。
// 格式: hysteria2://password@server:port?insecure=1&sni=...#name
func (s *LocalAPIServer) generateHysteria2Url(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" || s.nodeConfig.Hysteria2Port == 0 {
		return ""
	}
	params := url.Values{}
	params.Set("insecure", "1")
	params.Set("sni", config.SelfCertCN)
	return fmt.Sprintf("hysteria2://%s@%s:%d?%s#%s",
		config.Hysteria2Password(u.UUID),
		s.nodeConfig.ServerIP,
		s.nodeConfig.Hysteria2Port,
		params.Encode(),
		url.PathEscape(u.Name),
	)
}

// generateTUICUrl 生成 TUIC 连接 URL（UDP/QUIC，自签 TLS）。
// 格式: tuic://uuid:password@server:port?congestion_control=bbr&allow_insecure=1&sni=...#name
func (s *LocalAPIServer) generateTUICUrl(u *local.LocalUser) string {
	if s.nodeConfig == nil || s.nodeConfig.ServerIP == "" || s.nodeConfig.TUICPort == 0 {
		return ""
	}
	params := url.Values{}
	params.Set("congestion_control", "bbr")
	params.Set("allow_insecure", "1")
	params.Set("sni", config.SelfCertCN)
	params.Set("alpn", "h3")
	return fmt.Sprintf("tuic://%s:%s@%s:%d?%s#%s",
		config.TUICUUID(u.UUID),
		config.TUICPassword(u.UUID),
		s.nodeConfig.ServerIP,
		s.nodeConfig.TUICPort,
		params.Encode(),
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
