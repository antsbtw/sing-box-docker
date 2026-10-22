package stats

import "testing"

func TestMeterDeltasAcrossRestart(t *testing.T) {
	m := NewMeter()
	d := m.Deltas(map[string]*UserStats{"u": {Upload: 100, Download: 1000}})
	if d["u"] != (UserStats{Upload: 100, Download: 1000}) {
		t.Fatalf("first read = %+v", d["u"])
	}
	d = m.Deltas(map[string]*UserStats{"u": {Upload: 150, Download: 1500}})
	if d["u"] != (UserStats{Upload: 50, Download: 500}) {
		t.Fatalf("second read = %+v", d["u"])
	}
	// 没有新流量：不出现在结果里
	if d = m.Deltas(map[string]*UserStats{"u": {Upload: 150, Download: 1500}}); len(d) != 0 {
		t.Fatalf("idle read = %+v", d)
	}
	// sing-box 重启，计数器归零：整段读数都是新增
	d = m.Deltas(map[string]*UserStats{"u": {Upload: 10, Download: 20}})
	if d["u"] != (UserStats{Upload: 10, Download: 20}) {
		t.Fatalf("after restart = %+v", d["u"])
	}
	// 自己重启后 Reset：读数超过旧值也全算新增
	m.Reset()
	d = m.Deltas(map[string]*UserStats{"u": {Upload: 500, Download: 900}})
	if d["u"] != (UserStats{Upload: 500, Download: 900}) {
		t.Fatalf("after reset = %+v", d["u"])
	}
}
