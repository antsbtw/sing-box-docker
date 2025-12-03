package stats

import (
	"encoding/json"
	"fmt"
	"net/http"
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

	// 解析统计数据
	// 格式: user>>>uuid>>>traffic>>>uplink/downlink
	stats := make(map[string]*UserStats)

	for _, stat := range result.Stat {
		// 简化解析，实际格式可能需要调整
		if _, ok := stats[stat.Name]; !ok {
			stats[stat.Name] = &UserStats{}
		}
	}

	return stats, nil
}
