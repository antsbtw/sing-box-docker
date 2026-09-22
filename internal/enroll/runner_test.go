package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
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
