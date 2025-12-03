package config

import (
	"os"
	"strconv"
	"time"
)

// LoadFromEnv 从环境变量加载配置
func LoadFromEnv() *AgentConfig {
	return &AgentConfig{
		APIURL:        getEnv("OTUN_API_URL", "https://saasapi.situstechnologies.com"),
		NodeAPIKey:    getEnv("NODE_API_KEY", ""),
		NodeID:        getEnv("NODE_ID", "node-default"),
		SyncInterval:  getDurationEnv("SYNC_INTERVAL", 60) * time.Second,
		StatsInterval: getDurationEnv("STATS_INTERVAL", 300) * time.Second,
		VLESSPort:     getIntEnv("VLESS_PORT", 443),
		SSPort:        getIntEnv("SS_PORT", 8388),
		SingboxBin:    getEnv("SINGBOX_BIN", "/usr/local/bin/sing-box"),
		SingboxConfig: getEnv("SINGBOX_CONFIG", "/etc/sing-box/config.json"),
		LogLevel:      getEnv("LOG_LEVEL", "info"),
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
