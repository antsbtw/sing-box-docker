package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"otun-node-agent/internal/config"
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

// TestGeneratorStatsUsers 测试所有启用的用户都被加入统计列表
func TestGeneratorStatsUsers(t *testing.T) {
	expireTime := time.Now().Add(30 * 24 * time.Hour)
	users := []config.User{
		{
			UUID:      "user1-vless-ss",
			Protocols: []string{"vless", "shadowsocks"},
			SSPassword: "pass1",
			Enabled:   true,
			ExpireAt:  &expireTime,
		},
		{
			UUID:      "user2-vless-only",
			Protocols: []string{"vless"},
			Enabled:   true,
		},
		{
			UUID:      "user3-ss-only",
			Protocols: []string{"shadowsocks"},
			SSPassword: "pass3",
			Enabled:   true,
		},
		{
			UUID:      "user4-disabled",
			Protocols: []string{"vless"},
			Enabled:   false, // 禁用的用户不应被统计
		},
	}

	gen := config.NewGenerator(443, 8388, "test-key", []string{"test-short-id"})
	cfg := gen.Generate(users, "www.microsoft.com")

	// 检查 experimental.v2ray_api.stats.users 是否包含所有启用的用户
	experimental, ok := cfg["experimental"].(map[string]any)
	if !ok {
		t.Fatal("experimental config not found")
	}

	v2rayAPI, ok := experimental["v2ray_api"].(map[string]any)
	if !ok {
		t.Fatal("v2ray_api config not found")
	}

	stats, ok := v2rayAPI["stats"].(map[string]any)
	if !ok {
		t.Fatal("stats config not found")
	}

	statsUsers, ok := stats["users"].([]string)
	if !ok {
		t.Fatal("stats users not found or wrong type")
	}

	// 验证所有启用的用户（无论协议）都在统计列表中
	expectedUsers := []string{"user1-vless-ss", "user2-vless-only", "user3-ss-only"}
	if len(statsUsers) != len(expectedUsers) {
		t.Errorf("Expected %d users in stats, got %d", len(expectedUsers), len(statsUsers))
	}

	userMap := make(map[string]bool)
	for _, u := range statsUsers {
		userMap[u] = true
	}

	for _, expected := range expectedUsers {
		if !userMap[expected] {
			t.Errorf("User %s not found in stats list", expected)
		}
	}

	// 验证禁用的用户不在统计列表中
	if userMap["user4-disabled"] {
		t.Error("Disabled user should not be in stats list")
	}

	t.Logf("✅ All %d enabled users are correctly added to stats list", len(expectedUsers))
	t.Logf("Stats users: %v", statsUsers)
}
