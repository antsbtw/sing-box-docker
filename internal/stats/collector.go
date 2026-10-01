package stats

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/http2"
)

// Collector 从 sing-box 的 V2Ray API 收集流量统计。
//
// ⚠️ 那是 **gRPC** 接口（sing-box 以 with_v2ray_api 编译，监听 127.0.0.1:10085）。
// v1.14.0 及之前用 HTTP/1.1 GET 去请求它，必然失败 —— 所有版本的流量统计一直是空的，
// 用户的流量用量恒为 0、流量限额从未生效（1.4.0 回归 I3：agent 每 25 秒报「读取统计失败」）。
//
// 这里用明文 HTTP/2（h2c）手写一次 unary gRPC 调用：只有一个方法、两个很小的消息，
// 不值得为此引入整套 grpc / protobuf 依赖。
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
			Transport: &http2.Transport{
				// h2c：不走 TLS，直接在 TCP 上说 HTTP/2
				AllowHTTP: true,
				DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				},
			},
		},
	}
}

// statEntry 一条计数器
type statEntry struct {
	Name  string
	Value int64
}

// Collect 收集所有用户的流量统计（累计值，不清零）
func (c *Collector) Collect() (map[string]*UserStats, error) {
	entries, err := c.queryStats("user>>>", false)
	if err != nil {
		return nil, err
	}
	return parseUserStats(entries), nil
}

// queryStatsPaths QueryStats 的服务路径。
//
// ⚠️ sing-box 的 proto 包名是 `experimental.v2rayapi`（不是 v2ray 的 `v2ray.core.app.stats.command`），
// 所以路径是前者；后者留作兜底（服务端回 404 / Unimplemented 时再试），兼容可能改过包名的构建。
// 消息字段号两边一致：QueryStatsRequest{pattern=1, reset=2}，Stat{name=1, value=2}。
var queryStatsPaths = []string{
	"/experimental.v2rayapi.StatsService/QueryStats",
	"/v2ray.core.app.stats.command.StatsService/QueryStats",
}

// errUnimplemented 服务端不认这个路径
var errUnimplemented = fmt.Errorf("stats API: method not implemented")

// queryStats 调 StatsService/QueryStats，按 queryStatsPaths 依次尝试。
func (c *Collector) queryStats(pattern string, reset bool) ([]statEntry, error) {
	var lastErr error
	for _, path := range queryStatsPaths {
		entries, err := c.queryStatsAt(path, pattern, reset)
		if err == errUnimplemented {
			lastErr = err
			continue
		}
		return entries, err
	}
	return nil, lastErr
}

func (c *Collector) queryStatsAt(path, pattern string, reset bool) ([]statEntry, error) {
	url := fmt.Sprintf("http://%s%s", c.apiAddr, path)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(grpcFrame(encodeQueryStatsRequest(pattern, reset))))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request stats: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errUnimplemented
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("stats API returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read stats: %w", err)
	}
	// gRPC 的结果码在 trailer 里（body 读完才有）；出错时也可能直接放在 header 里
	status := resp.Trailer.Get("Grpc-Status")
	if status == "" {
		status = resp.Header.Get("Grpc-Status")
	}
	if status == "12" { // UNIMPLEMENTED
		return nil, errUnimplemented
	}
	if status != "" && status != "0" {
		msg := resp.Trailer.Get("Grpc-Message")
		if msg == "" {
			msg = resp.Header.Get("Grpc-Message")
		}
		return nil, fmt.Errorf("stats API grpc-status %s: %s", status, msg)
	}
	payload, err := grpcUnframe(body)
	if err != nil {
		return nil, err
	}
	return decodeQueryStatsResponse(payload)
}

// parseUserStats 把计数器按用户归并。
//
// sing-box 的 v2ray_api 以 ">>>" 分段给出计数器名：
//
//	user>>>{uuid}>>>traffic>>>uplink
//	user>>>{uuid}>>>traffic>>>downlink
func parseUserStats(entries []statEntry) map[string]*UserStats {
	stats := make(map[string]*UserStats)
	for _, stat := range entries {
		parts := strings.Split(stat.Name, ">>>")
		// 形如 [user, <uuid>, traffic, uplink]
		if len(parts) != 4 || parts[0] != "user" || parts[2] != "traffic" || parts[1] == "" {
			continue
		}
		us, ok := stats[parts[1]]
		if !ok {
			us = &UserStats{}
			stats[parts[1]] = us
		}
		switch parts[3] {
		case "uplink":
			us.Upload = stat.Value
		case "downlink":
			us.Download = stat.Value
		}
	}
	return stats
}

// MARK: - gRPC 帧与 protobuf（只覆盖 QueryStats 用到的部分）

// grpcFrame 加 gRPC 消息前缀：1 字节「是否压缩」(0) + 4 字节大端长度。
func grpcFrame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// grpcUnframe 取出第一条消息。unary 调用只有一条。
func grpcUnframe(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, nil // 空响应 = 没有计数器
	}
	if len(b) < 5 {
		return nil, fmt.Errorf("grpc frame too short: %d", len(b))
	}
	if b[0] != 0 {
		return nil, fmt.Errorf("grpc compressed frame not supported")
	}
	n := binary.BigEndian.Uint32(b[1:5])
	if uint64(len(b)-5) < uint64(n) {
		return nil, fmt.Errorf("grpc frame truncated: want %d, have %d", n, len(b)-5)
	}
	return b[5 : 5+n], nil
}

// encodeQueryStatsRequest：message QueryStatsRequest { string pattern = 1; bool reset = 2; }
func encodeQueryStatsRequest(pattern string, reset bool) []byte {
	var out []byte
	if pattern != "" {
		out = append(out, 0x0a) // field 1, wire type 2
		out = appendVarint(out, uint64(len(pattern)))
		out = append(out, pattern...)
	}
	if reset {
		out = append(out, 0x10, 0x01) // field 2, wire type 0, true
	}
	return out
}

// decodeQueryStatsResponse：message QueryStatsResponse { repeated Stat stat = 1; }
// message Stat { string name = 1; int64 value = 2; }
func decodeQueryStatsResponse(b []byte) ([]statEntry, error) {
	var out []statEntry
	err := walkFields(b, func(field int, wire int, val uint64, data []byte) error {
		if field != 1 || wire != 2 {
			return nil
		}
		var e statEntry
		if err := walkFields(data, func(f int, w int, v uint64, d []byte) error {
			switch {
			case f == 1 && w == 2:
				e.Name = string(d)
			case f == 2 && w == 0:
				e.Value = int64(v)
			}
			return nil
		}); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// walkFields 逐个读 protobuf 字段：varint（0）、定长 64（1）、长度前缀（2）、定长 32（5）。
func walkFields(b []byte, fn func(field int, wire int, val uint64, data []byte) error) error {
	for len(b) > 0 {
		key, n := readVarint(b)
		if n <= 0 {
			return fmt.Errorf("protobuf: bad key")
		}
		b = b[n:]
		field, wire := int(key>>3), int(key&7)
		switch wire {
		case 0:
			v, n := readVarint(b)
			if n <= 0 {
				return fmt.Errorf("protobuf: bad varint")
			}
			b = b[n:]
			if err := fn(field, wire, v, nil); err != nil {
				return err
			}
		case 1:
			if len(b) < 8 {
				return fmt.Errorf("protobuf: short fixed64")
			}
			b = b[8:]
		case 2:
			l, n := readVarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return fmt.Errorf("protobuf: bad length")
			}
			data := b[n : n+int(l)]
			b = b[n+int(l):]
			if err := fn(field, wire, 0, data); err != nil {
				return err
			}
		case 5:
			if len(b) < 4 {
				return fmt.Errorf("protobuf: short fixed32")
			}
			b = b[4:]
		default:
			return fmt.Errorf("protobuf: unsupported wire type %d", wire)
		}
	}
	return nil
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func readVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
