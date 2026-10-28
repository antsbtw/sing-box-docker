package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	// 节点名里是否加「· IPv6」。只在 v6Server() 复制出的实例上为 true
	ipv6Label bool
	// 节点名里是否不加协议名（OTun 契约要 url_v6 与 url 逐字相同，名字沿用）
	plainNames bool
}

// nodeName 链接里显示的节点名：「用户名 · 协议」，IPv6 再加「· IPv6」。
//
// 原来六个协议都只写用户名，Clash 只好把它们改名成「名字、名字-01、-02…」，
// 分不清哪个是哪个协议（2026-10-22 iPhone 回归）；现在又多了 IPv6 节点，更要区分。
func (s *LocalAPIServer) nodeName(u *local.LocalUser, proto string) string {
	if s.plainNames {
		return u.Name
	}
	name := u.Name + " · " + proto
	if s.ipv6Label {
		name += " · IPv6"
	}
	return name
}

// v6Server 地址换成公网 IPv6 的副本，用同一套生成函数出 IPv6 链接（除地址和名字外完全相同）。
// 服务器没有公网 IPv6 时返回 nil
func (s *LocalAPIServer) v6Server(label bool) *LocalAPIServer {
	if s.nodeConfig == nil || s.nodeConfig.ServerIPv6 == "" {
		return nil
	}
	nc := *s.nodeConfig
	nc.ServerIP = s.nodeConfig.ServerIPv6
	return &LocalAPIServer{store: s.store, apiKey: s.apiKey, nodeConfig: &nc, ipv6Label: label, plainNames: s.plainNames}
}

// NodeConfig 节点配置信息
type NodeConfig struct {
	NodeID   string `json:"node_id"`
	ServerIP string `json:"server_ip"`
	// 公网 IPv6。空表示没有：此时不出 IPv6 链接，其他一切照旧
	ServerIPv6 string `json:"server_ipv6,omitempty"`
	PublicKey  string `json:"public_key"`
	ShortID    string `json:"short_id"`
	VLESSPort  int    `json:"vless_port"`
	SSPort     int    `json:"ss_port"`
	SSMethod   string `json:"ss_method"`
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

	// 返回格式判定：**默认返回通用 base64 节点列表**（机场订阅的标准行为，
	// 所有客户端——Shadowrocket/Clash/v2rayN/OTun-M 等都认），**只有显式要求
	// 才返回 sing-box JSON**。
	//
	// ⚠️ 之前按 User-Agent「含 sing-box 就给 JSON」是错的：OTun-M 等基于
	// sing-box 内核的 fork UA 也含 sing-box，会被误判成官方 sing-box 拿到 JSON
	// 而解不出（2026-10-12 OTun-M 反馈）。改为显式信号，不再猜 UA。
	//
	// 要 JSON 的唯一方式：URL 带 ?format=singbox。OBox App 给 sing-box 客户端
	// 的「打开 sing-box」链接会带上它（import-remote-profile 需要 sing-box
	// 自己的 JSON profile）。其余一律 base64。
	if r.URL.Query().Get("format") == "singbox" {
		if data, err := s.buildSingBoxProfile(matched); err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Profile-Update-Interval", "24")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
		// 生成失败则退回 base64，总比 500 好。
	}

	// OTun-M 专用：协议条目列表，带 url_v6（v6 优先、退回 v4）。只在显式要求时给 ——
	// 老版 OTun-M 见到以「{」开头的正文会当成 sing-box 配置（2026-10-12 的教训），
	// 所以默认响应保持 base64 不变。契约：OBox Documents/Refactor-v2/CONTRACT_OTUNM_OBOX_IPV6_2026-10-27.md
	if r.URL.Query().Get("format") == "otun" {
		data, err := json.Marshal(s.buildOTunSubscription(matched))
		if err == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Profile-Update-Interval", "24")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
	}

	sub := s.toUserResponse(matched).SubscriptionUrl
	// 默认：通用 base64 节点列表。text/plain，部分客户端认 profile 头。
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
	VLESSUrl     string `json:"vless_url,omitempty"`
	SSUrl        string `json:"ss_url,omitempty"`
	VMessUrl     string `json:"vmess_url,omitempty"`
	TrojanUrl    string `json:"trojan_url,omitempty"`
	Hysteria2Url string `json:"hysteria2_url,omitempty"`
	TUICUrl      string `json:"tuic_url,omitempty"`
	// IPv6 链接：服务器有公网 IPv6 时才有，用户在客户端里自己选 v4 或 v6
	VLESSUrlV6      string `json:"vless_url_v6,omitempty"`
	SSUrlV6         string `json:"ss_url_v6,omitempty"`
	VMessUrlV6      string `json:"vmess_url_v6,omitempty"`
	TrojanUrlV6     string `json:"trojan_url_v6,omitempty"`
	Hysteria2UrlV6  string `json:"hysteria2_url_v6,omitempty"`
	TUICUrlV6       string `json:"tuic_url_v6,omitempty"`
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

	// IPv6 链接：服务器有公网 IPv6 时，每个协议再给一条 IPv6 的，排在全部 IPv4 之后。
	// 一条链接只能写一个地址，所以 v4、v6 各一个节点，由用户在 Clash / Shadowrocket 里自己选
	if v6 := s.v6Server(true); v6 != nil {
		for _, proto := range u.Protocols {
			switch proto {
			case "vless":
				resp.VLESSUrlV6 = v6.generateVLESSUrl(u)
				links = appendIf(links, resp.VLESSUrlV6)
			case "shadowsocks":
				resp.SSUrlV6 = v6.generateSSUrl(u)
				links = appendIf(links, resp.SSUrlV6)
			case "vmess":
				resp.VMessUrlV6 = v6.generateVMessUrl(u)
				links = appendIf(links, resp.VMessUrlV6)
			case "trojan":
				resp.TrojanUrlV6 = v6.generateTrojanUrl(u)
				links = appendIf(links, resp.TrojanUrlV6)
			case "hysteria2":
				resp.Hysteria2UrlV6 = v6.generateHysteria2Url(u)
				links = appendIf(links, resp.Hysteria2UrlV6)
			case "tuic":
				resp.TUICUrlV6 = v6.generateTUICUrl(u)
				links = appendIf(links, resp.TUICUrlV6)
			}
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

// OTunProtocol 与 OTun 后端下发的 protocols[] 条目同形（OTun-M VPNProtocolConfig）
type OTunProtocol struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	URLV6    string `json:"url_v6,omitempty"`
	Node     string `json:"node"`
}

// OTunSubscription OTun-M 订阅（?format=otun）的响应体
type OTunSubscription struct {
	Version   int            `json:"version"`
	Name      string         `json:"name"`
	Protocols []OTunProtocol `json:"protocols"`
}

// otunV6Protocols 带 url_v6 的协议：只有 TCP 类。OTun-M 内核对 UDP（hy2 / tuic / ss 的 DNS）
// 只用第一个地址、没有退回 v4，v6 不通就整个连不上（OTun BASIC_NODES_IPV6_DELIVERY_DESIGN C1–C4）
var otunV6Protocols = map[string]bool{"vless": true, "vmess": true, "trojan": true}

// buildOTunSubscription 按用户启用的协议生成条目。url 是 v4 地址的标准链接；有公网 IPv6 时，
// TCP 类协议再带 url_v6 —— 用同一个生成函数、只换地址，保证除地址外逐字相同（契约 C3）
func (s *LocalAPIServer) buildOTunSubscription(u *local.LocalUser) OTunSubscription {
	sub := OTunSubscription{Version: 1, Name: u.Name, Protocols: []OTunProtocol{}}
	// 名字沿用用户名、不加协议和「IPv6」：契约 C3 要 url_v6 除地址外与 url 逐字相同
	plain := &LocalAPIServer{store: s.store, apiKey: s.apiKey, nodeConfig: s.nodeConfig, plainNames: true}
	v6 := plain.v6Server(false)
	s = plain
	gens := []struct {
		name string
		gen  func(*LocalAPIServer, *local.LocalUser) string
	}{
		{"vless", (*LocalAPIServer).generateVLESSUrl},
		{"shadowsocks", (*LocalAPIServer).generateSSUrl},
		{"vmess", (*LocalAPIServer).generateVMessUrl},
		{"trojan", (*LocalAPIServer).generateTrojanUrl},
		{"hysteria2", (*LocalAPIServer).generateHysteria2Url},
		{"tuic", (*LocalAPIServer).generateTUICUrl},
	}
	for _, g := range gens {
		if !userHasProtocol(u, g.name) {
			continue
		}
		url := g.gen(s, u)
		if url == "" {
			continue // 协议未启用（端口为 0）或缺地址
		}
		p := OTunProtocol{Protocol: g.name, URL: url, Node: "primary"}
		if v6 != nil && otunV6Protocols[g.name] {
			p.URLV6 = g.gen(v6, u)
		}
		sub.Protocols = append(sub.Protocols, p)
	}
	return sub
}

func userHasProtocol(u *local.LocalUser, name string) bool {
	for _, p := range u.Protocols {
		if p == name {
			return true
		}
	}
	return false
}

// hostPort 拼分享链接里的 host:port。IPv6 地址要加方括号（[2001:db8::1]:443），
// 原来写 "%s:%d"，v6 地址拼出来客户端分不清哪段是端口。
func (s *LocalAPIServer) hostPort(port int) string {
	return net.JoinHostPort(strings.Trim(s.nodeConfig.ServerIP, "[]"), strconv.Itoa(port))
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

	return fmt.Sprintf("vless://%s@%s?%s#%s",
		u.UUID,
		s.hostPort(s.nodeConfig.VLESSPort),
		params.Encode(),
		url.PathEscape(s.nodeName(u, "VLESS")),
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

	return fmt.Sprintf("ss://%s@%s#%s",
		encoded,
		s.hostPort(s.nodeConfig.SSPort),
		url.PathEscape(s.nodeName(u, "Shadowsocks")),
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
		"ps":   s.nodeName(u, "VMess"),
		"add":  s.nodeConfig.ServerIP,
		"port": fmt.Sprintf("%d", s.nodeConfig.VMessPort),
		"id":   config.VMessUUID(u.UUID),
		"aid":  "0",
		"net":  "ws",
		"type": "none",
		// 不写 host：空串会被部分客户端（OTun-M、sing-box 导入）原样写成空 Host 头，
		// WS 握手带两条 Host，VMess 全部不通（2026-10-28 实测）。没有 host 时客户端用连接地址。
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
	return fmt.Sprintf("trojan://%s@%s?%s#%s",
		config.TrojanPassword(u.UUID),
		s.hostPort(s.nodeConfig.TrojanPort),
		params.Encode(),
		url.PathEscape(s.nodeName(u, "Trojan")),
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
	return fmt.Sprintf("hysteria2://%s@%s?%s#%s",
		config.Hysteria2Password(u.UUID),
		s.hostPort(s.nodeConfig.Hysteria2Port),
		params.Encode(),
		url.PathEscape(s.nodeName(u, "Hysteria2")),
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
	return fmt.Sprintf("tuic://%s:%s@%s?%s#%s",
		config.TUICUUID(u.UUID),
		config.TUICPassword(u.UUID),
		s.hostPort(s.nodeConfig.TUICPort),
		params.Encode(),
		url.PathEscape(s.nodeName(u, "TUIC")),
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
