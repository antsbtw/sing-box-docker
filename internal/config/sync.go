package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Syncer 负责从管理服务器同步用户配置
type Syncer struct {
	apiURL      string
	apiKey      string
	httpClient  *http.Client
	lastVersion string
}

// NewSyncer 创建配置同步器
func NewSyncer(apiURL, apiKey string) *Syncer {
	return &Syncer{
		apiURL: apiURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegisterRequest 节点注册请求
type RegisterRequest struct {
	NodeID    string         `json:"node_id"`
	Version   string         `json:"version"`
	PublicKey string         `json:"public_key"`
	ShortIDs  []string       `json:"short_ids"`
	Protocols map[string]any `json:"protocols"`
}

// Register 向管理服务器注册节点
func (s *Syncer) Register(nodeID, publicKey string, shortIDs []string, vlessPort, ssPort int) error {
	url := fmt.Sprintf("%s/api/node/register", s.apiURL)

	req := RegisterRequest{
		NodeID:    nodeID,
		Version:   "1.0.0",
		PublicKey: publicKey,
		ShortIDs:  shortIDs,
		Protocols: map[string]any{
			"vless_reality": map[string]any{
				"port": vlessPort,
			},
			"shadowsocks": map[string]any{
				"port":   ssPort,
				"method": "chacha20-ietf-poly1305",
			},
		},
	}

	return s.postJSON(url, req, nil)
}

// Heartbeat 发送心跳
func (s *Syncer) Heartbeat(req *HeartbeatRequest) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/node/heartbeat", s.apiURL)

	var resp HeartbeatResponse
	if err := s.postJSON(url, req, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// ReportConnections 上报活跃连接
func (s *Syncer) ReportConnections(report *ConnectionsReport) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/node/connections", s.apiURL)

	var resp HeartbeatResponse
	if err := s.postJSON(url, report, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

// FetchUsers 从管理服务器获取用户列表
func (s *Syncer) FetchUsers() (*UsersResponse, error) {
	url := fmt.Sprintf("%s/api/node/users", s.apiURL)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result UsersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	s.lastVersion = result.Version
	return &result, nil
}

// HasNewVersion 检查是否有新版本配置
func (s *Syncer) HasNewVersion(version string) bool {
	return s.lastVersion != version
}

// postJSON 发送 JSON POST 请求
func (s *Syncer) postJSON(url string, reqBody any, respBody any) error {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	if respBody != nil {
		if err := json.NewDecoder(resp.Body).Decode(respBody); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}
