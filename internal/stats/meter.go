package stats

import "sync"

// Meter 把 sing-box 的累计计数器换算成增量。
//
// sing-box 的 user>>>{uuid}>>>traffic 计数器从进程启动开始累计，
// 而 agent 每次改配置都会重启 sing-box（Manager.Reload = Stop + Start），计数器随之归零。
// 本地用户的「已用流量」要跨重启一直累加，所以记账只能记增量：
// 本次读数减上次读数；读数比上次小就是重启过，整段读数都算新增。
//
// ⚠️ 只用于本地记账（local.Store 的 traffic_used 与限额判定）。
// 上报后端的仍是累计值，后端按 boot 合并（enroll 契约 §5.1），两者互不影响。
type Meter struct {
	mu   sync.Mutex
	last map[string]UserStats
}

// NewMeter 创建增量换算器
func NewMeter() *Meter {
	return &Meter{last: make(map[string]UserStats)}
}

// Deltas 返回自上次调用以来每个用户的上下行增量，只含有增量的用户
func (m *Meter) Deltas(current map[string]*UserStats) map[string]UserStats {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]UserStats)
	for uuid, cur := range current {
		if cur == nil {
			continue
		}
		prev := m.last[uuid]
		d := UserStats{
			Upload:   delta(cur.Upload, prev.Upload),
			Download: delta(cur.Download, prev.Download),
		}
		m.last[uuid] = *cur
		if d.Upload > 0 || d.Download > 0 {
			out[uuid] = d
		}
	}
	return out
}

func delta(cur, prev int64) int64 {
	if cur < prev {
		// 计数器归零过（sing-box 重启）：这次读数全是重启后的新增
		return cur
	}
	return cur - prev
}

// Reset 忘掉上次读数。agent 自己重启 sing-box **之后**调用（重启前先记一次账）：
// 计数器已归零，下次读数整段都是新增。
//
// 不调也不会多算（读数变小时按归零处理），但重启后某用户的新读数若已超过旧读数，
// 会少算一截 —— 自己重启时明确清掉更准。
func (m *Meter) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.last = make(map[string]UserStats)
}
