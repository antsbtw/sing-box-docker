package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/situstechnologies/otun-node-agent/internal/config"
)

func testGenerator() {
	// 模拟用户数据
	expireTime := time.Now().Add(30 * 24 * time.Hour)
	users := []config.User{
		{
			UUID:         "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
			Protocols:    []string{"vless", "shadowsocks"},
			SSPassword:   "test_password_123",
			Enabled:      true,
			TrafficLimit: 107374182400,
			TrafficUsed:  5368709120,
			ExpireAt:     &expireTime,
		},
		{
			UUID:         "b2c3d4e5-f6a7-8901-bcde-f12345678901",
			Protocols:    []string{"vless"},
			Enabled:      true,
			TrafficLimit: 0, // 无限制
		},
	}

	// 创建生成器（使用测试密钥）
	gen := config.NewGenerator(443, 8388, "test-private-key", []string{"0123456789abcdef"})
	
	// 生成配置
	cfg := gen.Generate(users, "www.microsoft.com")
	
	// 输出 JSON
	data, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Println(string(data))
}
