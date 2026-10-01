package stats

import (
	"io"
	"net"
	"net/http"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const (
	uuidA = "0d3f1b2c-1111-4aaa-8bbb-000000000001"
	uuidB = "0d3f1b2c-2222-4aaa-8bbb-000000000002"
)

// encodeStat 编码一条 Stat（测试用，与解码器互为逆运算）
func encodeStat(name string, value int64) []byte {
	var s []byte
	s = append(s, 0x0a)
	s = appendVarint(s, uint64(len(name)))
	s = append(s, name...)
	s = append(s, 0x10)
	s = appendVarint(s, uint64(value))
	var out []byte
	out = append(out, 0x0a)
	out = appendVarint(out, uint64(len(s)))
	return append(out, s...)
}

// fakeV2RayAPI 用 h2c 起一个只认 QueryStats 的假 gRPC 服务 —— 与 sing-box 的 v2ray_api 同一种协议。
//
// ⚠️ 原来的测试用 JSON over HTTP/1.1 冒充，正好把错误的假设锁进了测试：
// 线上 sing-box 只说 gRPC，collector 永远失败，而测试一直是绿的。
func fakeV2RayAPI(t *testing.T, handler func(w http.ResponseWriter, reqMsg []byte)) string {
	return fakeV2RayAPIAt(t, "/experimental.v2rayapi.StatsService/QueryStats", handler)
}

// fakeV2RayAPIAt 只在给定路径上回应；其他路径按 grpc-go 的行为回 Unimplemented（status 12）。
func fakeV2RayAPIAt(t *testing.T, path string, handler func(w http.ResponseWriter, reqMsg []byte)) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		writeGRPC(w, nil, "12")
	})
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("want HTTP/2, got %s", r.Proto)
		}
		if r.Header.Get("Content-Type") != "application/grpc" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		msg, err := grpcUnframe(body)
		if err != nil {
			t.Errorf("unframe request: %v", err)
		}
		handler(w, msg)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func writeGRPC(w http.ResponseWriter, msg []byte, status string) {
	w.Header().Set("Content-Type", "application/grpc")
	w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
	w.WriteHeader(http.StatusOK)
	if msg != nil {
		w.Write(grpcFrame(msg))
	}
	w.Header().Set("Grpc-Status", status)
	if status != "0" {
		w.Header().Set("Grpc-Message", "boom")
	}
}

func TestCollectOverGRPC(t *testing.T) {
	var resp []byte
	for _, s := range []struct {
		name  string
		value int64
	}{
		{"user>>>" + uuidA + ">>>traffic>>>uplink", 1048576},
		{"user>>>" + uuidA + ">>>traffic>>>downlink", 20971520},
		{"user>>>" + uuidB + ">>>traffic>>>uplink", 512},
		// 非 user 前缀、段数不对、未知方向：都应被忽略
		{"inbound>>>vless-in>>>traffic>>>uplink", 999},
		{"user>>>" + uuidA + ">>>traffic", 888},
		{"user>>>" + uuidA + ">>>traffic>>>sideways", 777},
	} {
		resp = append(resp, encodeStat(s.name, s.value)...)
	}

	addr := fakeV2RayAPI(t, func(w http.ResponseWriter, req []byte) {
		// 请求：pattern = "user>>>"，不清零（上报的是累计值）
		var pattern string
		var reset bool
		walkFields(req, func(f, wire int, v uint64, d []byte) error {
			if f == 1 {
				pattern = string(d)
			}
			if f == 2 {
				reset = v != 0
			}
			return nil
		})
		if pattern != "user>>>" || reset {
			t.Errorf("request pattern=%q reset=%v", pattern, reset)
		}
		writeGRPC(w, resp, "0")
	})

	got, err := NewCollector(addr).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 users, got %d: %+v", len(got), got)
	}
	if a := got[uuidA]; a == nil || a.Upload != 1048576 || a.Download != 20971520 {
		t.Errorf("user A = %+v", a)
	}
	if b := got[uuidB]; b == nil || b.Upload != 512 || b.Download != 0 {
		t.Errorf("user B = %+v", b)
	}
}

func TestCollectEmptyResponse(t *testing.T) {
	addr := fakeV2RayAPI(t, func(w http.ResponseWriter, _ []byte) { writeGRPC(w, nil, "0") })
	got, err := NewCollector(addr).Collect()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestCollectGRPCError(t *testing.T) {
	addr := fakeV2RayAPI(t, func(w http.ResponseWriter, _ []byte) { writeGRPC(w, nil, "13") })
	if _, err := NewCollector(addr).Collect(); err == nil {
		t.Fatal("grpc-status 13 应当报错")
	}
}

func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 35, 1<<63 - 1} {
		got, n := readVarint(appendVarint(nil, v))
		if got != v || n <= 0 {
			t.Errorf("varint %d -> %d (n=%d)", v, got, n)
		}
	}
}

// sing-box 用 experimental.v2rayapi；若服务端只认 v2ray 的包名，要能退到第二个路径。
func TestCollectFallsBackToV2RayPath(t *testing.T) {
	addr := fakeV2RayAPIAt(t, "/v2ray.core.app.stats.command.StatsService/QueryStats", func(w http.ResponseWriter, _ []byte) {
		writeGRPC(w, encodeStat("user>>>"+uuidA+">>>traffic>>>uplink", 42), "0")
	})
	got, err := NewCollector(addr).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got[uuidA] == nil || got[uuidA].Upload != 42 {
		t.Fatalf("got %+v", got)
	}
}

func TestCollectBothPathsUnimplemented(t *testing.T) {
	addr := fakeV2RayAPIAt(t, "/nowhere", func(w http.ResponseWriter, _ []byte) {})
	if _, err := NewCollector(addr).Collect(); err == nil {
		t.Fatal("两个路径都不认时应当报错")
	}
}
