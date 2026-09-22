package stats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 此前解析是空实现（只建对象不赋值），所有用户 up/down 恒为 0。
// 接线契约 §5.1 的 stats[] 全靠它，这组测试锁住行为。
func TestCollectParsesUplinkDownlink(t *testing.T) {
	const uuidA = "0d3f1b2c-1111-4aaa-8bbb-000000000001"
	const uuidB = "0d3f1b2c-2222-4aaa-8bbb-000000000002"

	resp := V2RayStatsResponse{}
	resp.Stat = []struct {
		Name  string `json:"name"`
		Value int64  `json:"value"`
	}{
		{Name: "user>>>" + uuidA + ">>>traffic>>>uplink", Value: 1048576},
		{Name: "user>>>" + uuidA + ">>>traffic>>>downlink", Value: 20971520},
		{Name: "user>>>" + uuidB + ">>>traffic>>>uplink", Value: 512},
		// 非 user 前缀、段数不对、未知方向：都应被忽略而不是误记
		{Name: "inbound>>>vless-in>>>traffic>>>uplink", Value: 999},
		{Name: "user>>>" + uuidA + ">>>traffic", Value: 888},
		{Name: "user>>>" + uuidA + ">>>traffic>>>sideways", Value: 777},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewCollector(srv.Listener.Addr().String())
	got, err := c.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("应只解析出 2 个用户，得到 %d: %+v", len(got), got)
	}
	if got[uuidA].Upload != 1048576 || got[uuidA].Download != 20971520 {
		t.Errorf("uuidA 期望 up=1048576 down=20971520，得到 up=%d down=%d",
			got[uuidA].Upload, got[uuidA].Download)
	}
	// 只上报了 uplink 的用户，downlink 应为 0 而不是缺失
	if got[uuidB].Upload != 512 || got[uuidB].Download != 0 {
		t.Errorf("uuidB 期望 up=512 down=0，得到 up=%d down=%d",
			got[uuidB].Upload, got[uuidB].Download)
	}
	if _, ok := got["vless-in"]; ok {
		t.Error("inbound 计数器不应出现在用户统计里")
	}
}

func TestCollectEmptyStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"stat":[]}`))
	}))
	defer srv.Close()

	got, err := c(srv).Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("空统计应返回空 map，得到 %d 项", len(got))
	}
}

func c(srv *httptest.Server) *Collector {
	return NewCollector(srv.Listener.Addr().String())
}
