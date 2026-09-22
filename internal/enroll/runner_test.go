package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"otun-node-agent/internal/local"
)

type fakeStats struct{ m map[string]UserStat }

func (f fakeStats) Collect() (map[string]UserStat, error) { return f.m, nil }

type fakeStore struct{ n int }

func (f fakeStore) UserCount() int { return f.n }

func newRunner(t *testing.T, srv *httptest.Server) *Runner {
	t.Helper()
	dir := t.TempDir()
	store := local.NewStore(dir, nil)
	return &Runner{
		DataDir:        dir,
		APIURL:         srv.URL,
		AgentVersion:   "v1.11.0",
		Token:          "obx1_test",
		Client:         NewClient(srv.URL, "v1.11.0"),
		Executor:       NewExecutor(store, fakeInfo{}, "v1.11.0", func() error { return nil }),
		Info:           fakeInfo{},
		Stats:          fakeStats{m: map[string]UserStat{}},
		Store:          fakeStore{},
		SingboxRunning: func() bool { return true },
	}
}

// 注册成功必须先落盘 node.json 再开始轮询 ——
// install.sh 正靠它判断何时删掉一次性 token（契约 §2.4）。
func TestPrepareRegistersAndPersists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			NodeID: "byo-abc123", NodeSecret: "nsk1_secret",
			PollURL: "http://x/poll", HeartbeatIntervalS: 30,
		})
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	if err := r.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	nf, err := LoadNodeFile(r.DataDir)
	if err != nil || nf == nil {
		t.Fatalf("node.json 应已写入: %v", err)
	}
	if nf.NodeID != "byo-abc123" || nf.NodeSecret != "nsk1_secret" {
		t.Errorf("凭据未正确保存: %+v", nf)
	}
	if len(r.bootID) != 32 {
		t.Errorf("boot_id 应为 32 hex，得到 %q", r.bootID)
	}
}

// 已接入时不得再消耗 token（契约 §2.3：node.json 优先）。
func TestPrepareSkipsRegisterWhenEnrolled(t *testing.T) {
	var registerCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registerCalls++
		w.WriteHeader(500)
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	if err := SaveNodeFile(r.DataDir, &NodeFile{
		NodeID: "existing", NodeSecret: "s", PollURL: srv.URL + "/poll",
	}); err != nil {
		t.Fatal(err)
	}

	if err := r.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if registerCalls != 0 {
		t.Errorf("已接入时不应调用注册，调用了 %d 次", registerCalls)
	}
}

// token_expired 等错误重试也是同样结果，应直接失败让 install.sh 呈现给用户。
func TestRegisterFatalErrorDoesNotRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(APIError{Code: ErrTokenExpired, Message: "token 已过期"})
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	err := r.Prepare(context.Background())
	if err == nil {
		t.Fatal("过期 token 应导致 Prepare 失败")
	}
	if calls != 1 {
		t.Errorf("不可重试的错误只应请求一次，请求了 %d 次", calls)
	}
}

// 收到 node_revoked 必须终止并返回 ErrRevoked，由 main 以退出码 3 结束。
func TestRunStopsOnRevoked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(APIError{Code: ErrNodeRevoked, Message: "已解绑"})
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	r.node = &NodeFile{NodeID: "n", NodeSecret: "s", PollURL: srv.URL + "/poll"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := r.Run(ctx); !errors.Is(err, ErrRevoked) {
		t.Fatalf("应返回 ErrRevoked，得到 %v", err)
	}

	// 清理后本地凭据应消失
	r.Cleanup()
	if nf, _ := LoadNodeFile(r.DataDir); nf != nil {
		t.Error("Cleanup 后 node.json 应已删除")
	}
}

// 指令执行后回执要带上；后端确认(acked)之后才从队列移除（契约 §5.2）。
func TestAckRetriedUntilAcknowledged(t *testing.T) {
	var mu sync.Mutex
	var round int
	var sawAckRounds []int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		round++
		cur := round
		var req PollRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Acks) > 0 {
			sawAckRounds = append(sawAckRounds, cur)
		}
		mu.Unlock()

		resp := PollResponse{ServerTime: time.Now(), NextWaitS: 25}
		switch cur {
		case 1:
			resp.Commands = []Command{{
				CommandID: "cmd-1", Type: CmdPing,
				ExpiresAt: time.Now().Add(time.Minute),
			}}
		case 2:
			// 故意不确认，回执应在第 3 轮再次出现
		default:
			resp.Acked = []string{"cmd-1"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	r.node = &NodeFile{NodeID: "n", NodeSecret: "s", PollURL: srv.URL + "/poll"}

	// 轮询之间有 minPollGap（1s）兜底限速，要覆盖"第 2 轮送出、
	// 第 2 轮不确认、第 3 轮再送"这个序列，窗口需留足 3 轮以上。
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_ = r.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(sawAckRounds) < 2 {
		t.Fatalf("未被确认的回执应重复携带，实际只在第 %v 轮出现", sawAckRounds)
	}
	if len(r.pendingAcks) != 0 {
		t.Errorf("确认后队列应清空，仍有 %d 条", len(r.pendingAcks))
	}
}

func TestClampWait(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 25}, {-5, 25}, {10, 10}, {25, 25}, {99, 25},
	} {
		if got := clampWait(tc.in); got != tc.want {
			t.Errorf("clampWait(%d)=%d，期望 %d", tc.in, got, tc.want)
		}
	}
}

// H-5：瞬时故障（5xx / 网络 / 429）必须退避重试，不能一次失败就永久放弃。
// 装机时 DNS 未就绪、后端正在发版都属此类。
func TestRegisterRetriesTransient(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(503)
			_ = json.NewEncoder(w).Encode(APIError{Code: ErrServerInternal, Message: "发版中"})
			return
		}
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			NodeID: "byo-retry", NodeSecret: "nsk1_ok", PollURL: "http://x/poll",
		})
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := r.Prepare(ctx); err != nil {
		t.Fatalf("瞬时故障后应最终成功，得到 %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("应重试到第 3 次成功，实际请求 %d 次", got)
	}
	if nf, _ := LoadNodeFile(r.DataDir); nf == nil || nf.NodeID != "byo-retry" {
		t.Error("重试成功后应写入 node.json")
	}
}

// H-5：终态错误要能被 main 识别为 ErrEnrollFatal，据此以退出码 3 结束 ——
// 不能让进程继续以普通 local 模式活着，那是"看着正常其实没接入"。
func TestRegisterFatalIsTagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_ = json.NewEncoder(w).Encode(APIError{
			Code: ErrAgentTooOld, Message: "版本过低", MinVersion: "v1.12.0",
		})
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	err := r.Prepare(context.Background())
	if !errors.Is(err, ErrEnrollFatal) {
		t.Fatalf("终态错误应包装为 ErrEnrollFatal，得到 %v", err)
	}
}

// M-3：后端一直不确认回执时，不能变成以 HTTP 速度空转的热循环。
func TestNoHotLoopWhenAcksNeverAcknowledged(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&polls, 1)
		resp := PollResponse{ServerTime: time.Now(), NextWaitS: 25}
		if n == 1 {
			resp.Commands = []Command{{
				CommandID: "cmd-x", Type: CmdPing,
				ExpiresAt: time.Now().Add(time.Minute),
			}}
		}
		// 永远不回 acked
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	r := newRunner(t, srv)
	r.node = &NodeFile{NodeID: "n", NodeSecret: "s", PollURL: srv.URL + "/poll"}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	// 第 1 轮领指令、第 2 轮 wait=0 送回执，此后应回到正常 wait，
	// 1.5 秒内不该打出几十次请求
	if n := atomic.LoadInt32(&polls); n > 5 {
		t.Errorf("回执未被确认时出现热循环：1.5 秒内轮询了 %d 次", n)
	}
	if len(r.pendingAcks) == 0 {
		t.Error("未被确认的回执应继续保留在队列中")
	}
}
