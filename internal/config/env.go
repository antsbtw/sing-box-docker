package config

import (
	"os"
	"strconv"
	"time"
)

// LoadFromEnv 从环境变量加载配置
func LoadFromEnv() *AgentConfig {
	// 解析管理模式
	mode := ManagementMode(getEnv("MANAGEMENT_MODE", "local"))
	if mode != ModeLocal && mode != ModeRemote && mode != ModeHybrid {
		mode = ModeLocal // 默认使用本地模式
	}

	return &AgentConfig{
		APIURL:         getEnv("OTUN_API_URL", "https://saasapi.situstechnologies.com"),
		NodeAPIKey:     getEnv("NODE_API_KEY", ""),
		NodeID:         getEnv("NODE_ID", "node-default"),
		SyncInterval:   getDurationEnv("SYNC_INTERVAL", 60) * time.Second,
		StatsInterval:  getDurationEnv("STATS_INTERVAL", 300) * time.Second,
		VLESSPort:      getIntEnv("VLESS_PORT", 443),
		SSPort:         getIntEnv("SS_PORT", 8388),
		SingboxBin:     getEnv("SINGBOX_BIN", "/usr/local/bin/sing-box"),
		SingboxConfig:  getEnv("SINGBOX_CONFIG", "/etc/sing-box/config.json"),
		LogLevel:       getEnv("LOG_LEVEL", "info"),
		ManagementMode: mode,
		ServerIP:       getEnv("SERVER_IP", ""), // 服务器公网 IP，用于生成连接 URL
		// Reality 握手借用的真实站点域名。
		// ⚠️ 这是 local 模式下 SNI 的**唯一来源**：配置生成、分享链接、
		// 注册上报都取它（接线契约 §8）。此前 generator 与 local API 各写死
		// 一份 "www.microsoft.com"，改其一就会两边不一致、客户端连不上。
		RealitySNI: getEnv("REALITY_SNI", "www.microsoft.com"),
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getIntEnv(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultVal
}

func getDurationEnv(key string, defaultVal int) time.Duration {
	return time.Duration(getIntEnv(key, defaultVal))
}
