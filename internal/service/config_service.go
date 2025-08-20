package service

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"sing-box-manager/internal/models"
	"sing-box-manager/internal/storage"
)

// ConfigService sing-box配置服务
type ConfigService struct {
	storage     *storage.JSONStorage
	configPath  string
	templatePath string
	serverName  string
}

// NewConfigService 创建配置服务
func NewConfigService(storage *storage.JSONStorage, configPath, templatePath, serverName string) *ConfigService {
	return &ConfigService{
		storage:      storage,
		configPath:   configPath,
		templatePath: templatePath,
		serverName:   serverName,
	}
}

// SingBoxConfig sing-box配置结构
type SingBoxConfig struct {
	Log struct {
		Level     string `json:"level"`
		Timestamp bool   `json:"timestamp"`
	} `json:"log"`
	DNS struct {
		Servers []struct {
			Tag     string `json:"tag"`
			Address string `json:"address"`
			Detour  string `json:"detour,omitempty"`
		} `json:"servers"`
		Rules []struct {
			DomainSuffix []string `json:"domain_suffix,omitempty"`
			Server       string   `json:"server"`
		} `json:"rules"`
	} `json:"dns"`
	Inbounds []Inbound `json:"inbounds"`
	Outbounds []struct {
		Type string `json:"type"`
		Tag  string `json:"tag"`
	} `json:"outbounds"`
	Route struct {
		Rules []struct {
			Protocol    string `json:"protocol,omitempty"`
			IPIsPrivate bool   `json:"ip_is_private,omitempty"`
			Outbound    string `json:"outbound"`
		} `json:"rules"`
	} `json:"route"`
}

// Inbound 入站配置
type Inbound struct {
	Type      string    `json:"type"`
	Tag       string    `json:"tag"`
	Listen    string    `json:"listen"`
	ListenPort int      `json:"listen_port"`
	Sniff     bool      `json:"sniff"`
	SniffOverrideDestination bool `json:"sniff_override_destination"`
	TLS       *TLSConfig `json:"tls,omitempty"`
	Users     []UserConfig `json:"users"`
}

// TLSConfig TLS配置
type TLSConfig struct {
	Enabled         bool          `json:"enabled"`
	ServerName      string        `json:"server_name"`
	CertificatePath string        `json:"certificate_path,omitempty"`
	KeyPath         string        `json:"key_path,omitempty"`
	Reality         *RealityConfig `json:"reality,omitempty"`
}

// RealityConfig Reality配置
type RealityConfig struct {
	Enabled    bool              `json:"enabled"`
	Handshake  RealityHandshake  `json:"handshake"`
	PrivateKey string            `json:"private_key"`
	ShortID    []string          `json:"short_id"`
}

// RealityHandshake Reality握手配置
type RealityHandshake struct {
	Server     string `json:"server"`
	ServerPort int    `json:"server_port"`
}

// UserConfig 用户配置
type UserConfig struct {
	Name     string `json:"name"`
	Password string `json:"password,omitempty"`
	UUID     string `json:"uuid,omitempty"`
}

// getRealityPrivateKey 获取Reality私钥
func (s *ConfigService) getRealityPrivateKey() string {
	keyFile := "configs/reality_private.key"
	
	// 如果私钥文件存在，读取它
	if data, err := os.ReadFile(keyFile); err == nil {
		return strings.TrimSpace(string(data))
	}
	
	// 返回默认测试密钥 (生产环境应该用真实密钥)
	return "oNglJo5OpvsjAjIWnGgVaQ_EUQP5_YKnGmnNdvhTCXM"
}

// GenerateConfig 生成sing-box配置
func (s *ConfigService) GenerateConfig() error {
	// 获取所有活跃用户
	users, err := s.storage.ListUsers()
	if err != nil {
		return fmt.Errorf("failed to get users: %v", err)
	}

	// 过滤活跃且未过期的用户
	activeUsers := make([]*models.User, 0)
	for _, user := range users {
		if user.IsActive && !user.IsExpired() && !user.IsTrafficExceeded() {
			activeUsers = append(activeUsers, user)
		}
	}

	// 生成配置
	config := s.buildConfig(activeUsers)

	// 保存配置文件
	configData, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(s.configPath, configData, 0644); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	fmt.Printf("Generated sing-box config with %d active users\n", len(activeUsers))
	return nil
}

// buildConfig 构建配置文件
func (s *ConfigService) buildConfig(users []*models.User) *SingBoxConfig {
	config := &SingBoxConfig{}

	// 基础配置
	config.Log.Level = "info"
	config.Log.Timestamp = true

	// DNS配置
	config.DNS.Servers = []struct {
		Tag     string `json:"tag"`
		Address string `json:"address"`
		Detour  string `json:"detour,omitempty"`
	}{
		{Tag: "cloudflare", Address: "1.1.1.1"},
		{Tag: "local", Address: "local", Detour: "direct"},
	}

	config.DNS.Rules = []struct {
		DomainSuffix []string `json:"domain_suffix,omitempty"`
		Server       string   `json:"server"`
	}{
		{DomainSuffix: []string{".cn"}, Server: "local"},
	}

	// 入站配置
	config.Inbounds = []Inbound{
		{
			Type:                     "mixed",
			Tag:                      "mixed-in",
			Listen:                   "::",
			ListenPort:               1080,
			Sniff:                    true,
			SniffOverrideDestination: true,
			Users:                    []UserConfig{},
		},
		{
			Type:                     "trojan",
			Tag:                      "trojan-in",
			Listen:                   "::",
			ListenPort:               443,
			Sniff:                    true,
			SniffOverrideDestination: true,
			TLS: &TLSConfig{
				Enabled:         true,
				ServerName:      s.serverName,
				CertificatePath: "configs/cert.pem",
				KeyPath:         "configs/key.pem",
			},
			Users: s.buildTrojanUsers(users),
		},
		{
			Type:                     "vless",
			Tag:                      "vless-in",
			Listen:                   "::",
			ListenPort:               8443,
			Sniff:                    true,
			SniffOverrideDestination: true,
			TLS: &TLSConfig{
				Enabled:         true,
				ServerName:      s.serverName,
				CertificatePath: "configs/cert.pem",
				KeyPath:         "configs/key.pem",
			},
			Users: s.buildVlessUsers(users),
		},
		{
			Type:                     "vless",
			Tag:                      "vless-reality-in",
			Listen:                   "::",
			ListenPort:               4433,
			Sniff:                    true,
			SniffOverrideDestination: true,
			TLS: &TLSConfig{
				Enabled:    true,
				ServerName: "www.google.com",
				Reality: &RealityConfig{
					Enabled: true,
					Handshake: RealityHandshake{
						Server:     "www.google.com",
						ServerPort: 443,
					},
					PrivateKey: s.getRealityPrivateKey(),
					ShortID:    []string{"0123456789abcdef"},
				},
			},
			Users: s.buildVlessUsers(users),
		},
	}

	// 出站配置
	config.Outbounds = []struct {
		Type string `json:"type"`
		Tag  string `json:"tag"`
	}{
		{Type: "direct", Tag: "direct"},
		{Type: "block", Tag: "block"},
	}

	// 路由配置
	config.Route.Rules = []struct {
		Protocol    string `json:"protocol,omitempty"`
		IPIsPrivate bool   `json:"ip_is_private,omitempty"`
		Outbound    string `json:"outbound"`
	}{
		{Protocol: "dns", Outbound: "dns-out"},
		{IPIsPrivate: true, Outbound: "direct"},
	}

	return config
}

// buildTrojanUsers 构建Trojan用户配置
func (s *ConfigService) buildTrojanUsers(users []*models.User) []UserConfig {
	userConfigs := make([]UserConfig, 0)
	for _, user := range users {
		userConfigs = append(userConfigs, UserConfig{
			Name:     user.Username,
			Password: user.Password,
		})
	}
	return userConfigs
}

// buildVlessUsers 构建VLESS用户配置
func (s *ConfigService) buildVlessUsers(users []*models.User) []UserConfig {
	userConfigs := make([]UserConfig, 0)
	for _, user := range users {
		userConfigs = append(userConfigs, UserConfig{
			Name: user.Username,
			UUID: user.ID,
		})
	}
	return userConfigs
}

// ReloadSingBox 重载sing-box配置
func (s *ConfigService) ReloadSingBox() error {
	cmd := exec.Command("pkill", "-HUP", "sing-box")
	if err := cmd.Run(); err != nil {
		fmt.Printf("Failed to reload sing-box: %v\n", err)
		return s.RestartSingBox()
	}
	
	fmt.Println("sing-box configuration reloaded")
	return nil
}

// RestartSingBox 重启sing-box
func (s *ConfigService) RestartSingBox() error {
	exec.Command("pkill", "sing-box").Run()
	time.Sleep(2 * time.Second)
	
	cmd := exec.Command("sing-box", "run", "-c", s.configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start sing-box: %v", err)
	}
	
	fmt.Println("sing-box restarted")
	return nil
}

// GenerateRealityKeypair 生成Reality密钥对
func (s *ConfigService) GenerateRealityKeypair() (map[string]string, error) {
	cmd := exec.Command("sing-box", "generate", "reality-keypair")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reality keypair: %v", err)
	}
	
	result := make(map[string]string)
	lines := strings.Split(string(output), "\n")
	
	for _, line := range lines {
		if strings.Contains(line, "PrivateKey:") {
			result["private_key"] = strings.TrimSpace(strings.Split(line, ":")[1])
		}
		if strings.Contains(line, "PublicKey:") {
			result["public_key"] = strings.TrimSpace(strings.Split(line, ":")[1])
		}
	}
	
	// 保存私钥
	if privateKey, exists := result["private_key"]; exists {
		os.WriteFile("configs/reality_private.key", []byte(privateKey), 0600)
	}
	
	return result, nil
}

// AutoReloadConfig 自动重载配置
func (s *ConfigService) AutoReloadConfig() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		if err := s.GenerateConfig(); err != nil {
			fmt.Printf("Failed to generate config: %v\n", err)
			continue
		}
		
		if err := s.ReloadSingBox(); err != nil {
			fmt.Printf("Failed to reload sing-box: %v\n", err)
		}
	}
}
