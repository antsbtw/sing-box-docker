package stats

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Collector 从 sing-box V2Ray API 收集流量统计
type Collector struct {
	apiAddr    string
	httpClient *http.Client
}

// UserStats 用户流量统计
type UserStats struct {
	Upload   int64
	Download int64
}

// NewCollector 创建统计收集器
func NewCollector(apiAddr string) *Collector {
	return &Collector{
		apiAddr: apiAddr,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// V2RayStatsResponse sing-box V2Ray API 响应
type V2RayStatsResponse struct {
	Stat []struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	} `json:"stat"`
}

// Collect 收集所有用户的流量统计
func (c *Collector) Collect() (map[string]*UserStats, error) {
	url := fmt.Sprintf("http://%s/v2ray.core.app.stats.command.StatsService/QueryStats", c.apiAddr)

	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("request stats: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats API returned %d", resp.StatusCode)
	}

	var result V2RayStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode stats: %w", err)
	}

	// 解析统计数据。
	//
	// sing-box 的 v2ray_api 以 ">>>" 分段给出计数器名：
	//     user>>>{uuid}>>>traffic>>>uplink
	//     user>>>{uuid}>>>traffic>>>downlink
	//
	// ⚠️ 此前这里只建空对象、从不赋值，所有用户的 up/down 恒为 0 ——
	// 接入契约 §5.1 的 stats[] 全靠它，不修则后端永远收到 0。
	stats := make(map[string]*UserStats)

	for _, stat := range result.Stat {
		parts := strings.Split(stat.Name, ">>>")
		// 形如 [user, <uuid>, traffic, uplink]
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" {
			continue
		}
		uuid := parts[1]
		if uuid == "" {
			continue
		}

		us, ok := stats[uuid]
		if !ok {
			us = &UserStats{}
			stats[uuid] = us
		}

		switch parts[3] {
		case "uplink":
			us.Upload = stat.Value
		case "downlink":
			us.Download = stat.Value
		}
	}

	return stats, nil
}
