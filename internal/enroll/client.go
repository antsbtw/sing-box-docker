package enroll

// 与后端的 HTTP 交互。接线契约 §0、§4、§5。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const (
	pathRegister = "/api/v1/obox/agent/register"

	// 轮询最长挂 25 秒（契约 §5.1），客户端超时要留足余量，
	// 否则会在服务端正常挂起时自己先断开。
	pollHTTPTimeout     = 40 * time.Second
	registerHTTPTimeout = 20 * time.Second
)

type Client struct {
	apiURL       string
	agentVersion string
	http         *http.Client
	pollHTTP     *http.Client
}

func NewClient(apiURL, agentVersion string) *Client {
	return &Client{
		apiURL:       strings.TrimRight(apiURL, "/"),
		agentVersion: agentVersion,
		http:         &http.Client{Timeout: registerHTTPTimeout},
		pollHTTP:     &http.Client{Timeout: pollHTTPTimeout},
	}
}

// userAgent 形如 obox-agent/v1.11.0 (linux; amd64)（契约 §0）
func (c *Client) userAgent() string {
	return fmt.Sprintf("obox-agent/%s (%s; %s)", c.agentVersion, runtime.GOOS, runtime.GOARCH)
}

// Register 用一次性 token 换取长期 node_secret（契约 §4）。
func (c *Client) Register(ctx context.Context, req *RegisterRequest) (*RegisterResponse, error) {
	req.Contract = ContractVersion

	var out RegisterResponse
	err := c.do(ctx, c.http, http.MethodPost, c.apiURL+pathRegister, "", req, &out)
	if err != nil {
		return nil, err
	}
	if out.NodeID == "" || out.NodeSecret == "" {
		return nil, fmt.Errorf("register: 响应缺少 node_id 或 node_secret")
	}
	return &out, nil
}

// Poll 一次长轮询：心跳 + 统计上报 + 指令回执 + 领取新指令（契约 §5）。
//
// wait 为服务端最长挂起秒数，0 表示立即返回（用于紧急回执）。
func (c *Client) Poll(ctx context.Context, pollURL, nodeSecret string, wait int, req *PollRequest) (*PollResponse, error) {
	if wait < 0 {
		wait = 0
	}
	if wait > 25 {
		wait = 25 // 契约 §5.1：超出按 25
	}
	req.AgentVersion = c.agentVersion

	url := fmt.Sprintf("%s?wait=%d", pollURL, wait)
	var out PollResponse
	if err := c.do(ctx, c.pollHTTP, http.MethodPost, url, nodeSecret, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, url, bearer string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("User-Agent", c.userAgent())
	if bearer != "" {
		httpReq.Header.Set("Authorization", "Bearer "+bearer)
	}

	resp, err := hc.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	// 限制读取量，避免异常响应撑爆内存
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Code: ErrServerInternal}
		// 后端约定非 2xx 必带 {error, message}；解析失败则退回状态码描述
		if jsonErr := json.Unmarshal(raw, apiErr); jsonErr != nil || apiErr.Code == "" {
			apiErr.Code = ErrServerInternal
			apiErr.Message = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
		}
		return apiErr
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Backoff 指数退避：1s → 2s → 4s …… 上限 60s，±20% 抖动（契约 §0）。
//
// 抖动是必要的：大量节点同时因后端重启而断开时，没有抖动会让它们
// 在同一秒集体重连，把刚起来的后端再打挂。
func Backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := time.Second << uint(min(attempt, 6)) // 1,2,4,8,16,32,64
	jitter := 1 + (rand.Float64()*0.4 - 0.2)  // ±20%
	d = time.Duration(float64(d) * jitter)

	// ⚠️ 封顶必须在抖动**之后**：先封顶再乘 1.2 会让上界变成 72s，
	// 违反契约 §0 的「上限 60s」。测试 TestBackoffGrowsAndCaps 锁住这点。
	if d > 60*time.Second {
		d = 60 * time.Second
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
