package enroll

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchIPAcceptsValidIPv4(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.10\n"))
	}))
	defer srv.Close()

	got := fetchIP(context.Background(), srv.Client(), srv.URL)
	if got != "203.0.113.10" {
		t.Errorf("应解析并去除空白，得到 %q", got)
	}
}

// 端点返回垃圾时必须当作没拿到 —— 契约允许留空，
// 但不允许把一段 HTML 当成 IP 报给后端。
func TestFetchIPRejectsGarbage(t *testing.T) {
	for _, body := range []string{
		"<html>error</html>",
		"not-an-ip",
		"",
		"2001:db8::1", // IPv6：节点对外是 v4，报 v6 会误标 nat_suspected
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		if got := fetchIP(context.Background(), srv.Client(), srv.URL); got != "" {
			t.Errorf("body=%q 应返回空，得到 %q", body, got)
		}
		srv.Close()
	}
}

func TestFetchIPRejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("203.0.113.10"))
	}))
	defer srv.Close()

	if got := fetchIP(context.Background(), srv.Client(), srv.URL); got != "" {
		t.Errorf("非 200 应返回空，得到 %q", got)
	}
}

// 超大响应不能撑爆内存。
func TestFetchIPLimitsBodySize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("A", 10<<20)))
	}))
	defer srv.Close()

	if got := fetchIP(context.Background(), srv.Client(), srv.URL); got != "" {
		t.Errorf("超大响应应返回空，得到 %q", got)
	}
}

// 全部端点不可达时返回空而不是卡住 —— 这个字段绝不能阻塞注册。
func TestDetectPublicIPFailsFastAndEmpty(t *testing.T) {
	// 用一个立即拒绝连接的地址替换端点列表
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 关掉，后续连接会被拒绝

	orig := publicIPEndpoints
	publicIPEndpoints = []string{"http://" + addr}
	defer func() { publicIPEndpoints = orig }()

	start := time.Now()
	got := DetectPublicIP(context.Background())
	elapsed := time.Since(start)

	if got != "" {
		t.Errorf("不可达时应返回空，得到 %q", got)
	}
	if elapsed > 10*time.Second {
		t.Errorf("耗时 %v，不应长时间阻塞注册", elapsed)
	}
}
