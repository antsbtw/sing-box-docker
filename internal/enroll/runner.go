package enroll

// Token 模式的注册与长轮询主循环。接线契约 §2.4、§4、§5。
//
// agent 只有这一个上行通道：一次 POST 同时承担心跳、统计上报、
// 指令回执与领取新指令。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"time"
)

// ExitRevoked 是收到 node_revoked / node_not_found 后的退出码。
//
// systemd 单元里配了 RestartPreventExitStatus=3：没有它，agent 会被
// 每 5 秒拉起一次去打一个已吊销的节点（契约 §5.3）。
const ExitRevoked = 3

// StatsSource 提供本次 boot 以来的累计流量。
type StatsSource interface {
	Collect() (map[string]UserStat, error)
}

// StoreView 是主循环需要的 store 只读视图。
type StoreView interface {
	UserCount() int
}

// Runner 驱动注册与长轮询。
type Runner struct {
	DataDir      string
	APIURL       string
	AgentVersion string
	Token        string // 仅首次注册用；已接入时为空

	Client   *Client
	Executor *Executor
	Info     NodeInfoProvider
	Stats    StatsSource
	Store    StoreView

	// SingboxRunning 报告数据面是否正常（/health 的同一判据）
	SingboxRunning func() bool

	// ExePath 当前可执行文件路径，用于升级后的回滚判定。为空则不回滚。
	ExePath string

	node      *NodeFile
	bootID    string
	startedAt time.Time

	// 连续轮询失败次数，用于判断刚升级的新版本是不是坏的
	consecutiveFailures int

	// 待回执队列：后端未确认的回执下次继续带（契约 §5.1）
	pendingAcks []CommandAck
}

// Prepare 决定走哪条路：已有 node.json 就用它，否则拿 token 注册。
//
// 契约 §2.3：node.json 存在即视为已接入，**优先**用它，不再读 token。
// install.sh 在带 token 重装时会先删掉旧文件（§2.4 第 0 步），
// 所以这里读到的一定是有效凭据，而不是上一轮已吊销的残留。
func (r *Runner) Prepare(ctx context.Context) error {
	r.startedAt = time.Now()

	bootID, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generate boot_id: %w", err)
	}
	r.bootID = bootID

	existing, err := LoadNodeFile(r.DataDir)
	if err != nil {
		// 文件损坏：当作未接入，走 token 重新注册，而不是抱着读不出的文件反复失败
		log.Printf("[enroll] node.json 无法读取（%v），尝试重新注册", err)
		existing = nil
	}
	if existing != nil {
		log.Printf("[enroll] 已接入，node_id=%s", existing.NodeID)
		r.node = existing
		return nil
	}

	if r.Token == "" {
		return errors.New("未接入且未提供 OTUN_ENROLL_TOKEN")
	}
	return r.register(ctx)
}

func (r *Runner) register(ctx context.Context) error {
	req := &RegisterRequest{
		Token:          r.Token,
		MachineID:      "",
		AgentVersion:   r.AgentVersion,
		SingboxVersion: r.Info.SingboxVersion(),
		OS:             detectOS(),
		Arch:           detectArch(),
		ListenPorts:    r.Info.ListenPorts(),
		Secrets:        r.Info.Secrets(),
		BootID:         r.bootID,
	}

	mid, err := MachineID(r.DataDir)
	if err != nil {
		return fmt.Errorf("compute machine_id: %w", err)
	}
	req.MachineID = mid

	log.Printf("[enroll] 开始注册，machine_id=%s…", mid[:12])

	// 网络错误 / 5xx / 429 必须退避重试（契约 §0）：装机时 DNS 未就绪、
	// 后端正在发版、撞限流都是瞬时故障，只试一次会把它们变成永久失败 ——
	// agent 进程还活着，systemd 不会重启它，unit 里的 token 也再没人读。
	//
	// 循环有界：token 15 分钟后自然变成 token_expired（终态），随即退出。
	var resp *RegisterResponse
	for attempt := 0; ; attempt++ {
		var err error
		resp, err = r.Client.Register(ctx, req)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.IsFatalRegister() {
			// 终态：重试也是同样结果。退出让 install.sh 的 60 秒等待失败，
			// 把真实原因呈现给用户（契约 §2.4-5、§4.3）。
			log.Printf("[enroll] 注册被拒绝：%s", apiErr.Error())
			if apiErr.Code == ErrAgentTooOld {
				log.Printf("[enroll] 需要 agent %s 及以上，当前 %s",
					apiErr.MinVersion, r.AgentVersion)
			}
			return fmt.Errorf("%w: %v", ErrEnrollFatal, err)
		}

		d := Backoff(attempt)
		// 429 带 retry_after_s 时听服务端的（S-4）
		if errors.As(err, &apiErr) && apiErr.RetryAfterS > 0 {
			if ra := time.Duration(apiErr.RetryAfterS) * time.Second; ra > d {
				d = ra
			}
		}
		log.Printf("[enroll] 注册失败（%v），%v 后重试", err, d.Truncate(time.Millisecond))
		if !sleepCtx(ctx, d) {
			return ctx.Err()
		}
	}

	nf := &NodeFile{
		NodeID:     resp.NodeID,
		NodeSecret: resp.NodeSecret,
		MachineID:  mid,
		APIURL:     r.APIURL,
		PollURL:    resp.PollURL,
		EnrolledAt: time.Now().UTC(),
	}
	// ⚠️ 必须先写盘再开始轮询：install.sh 正在轮询这个文件，
	// 看到它才会删掉 systemd 里的一次性 token（契约 §2.4 第 2 步）。
	if err := SaveNodeFile(r.DataDir, nf); err != nil {
		return fmt.Errorf("保存 node.json 失败：%w", err)
	}
	r.node = nf

	if resp.Reenrolled {
		log.Printf("[enroll] 重新注册成功（复用 node_id=%s，secret 已轮换）", resp.NodeID)
	} else {
		log.Printf("[enroll] 注册成功，node_id=%s", resp.NodeID)
	}
	return nil
}

// Run 长轮询主循环。返回 nil 表示 ctx 取消（正常退出）；
// 返回 ErrRevoked 表示节点已被吊销，调用方应以 ExitRevoked 退出。
func (r *Runner) Run(ctx context.Context) error {
	if r.node == nil {
		return errors.New("Run 前必须先 Prepare")
	}

	log.Printf("[enroll] 开始长轮询 %s", r.node.PollURL)
	attempt := 0
	wait := 25

	for {
		if ctx.Err() != nil {
			return nil
		}

		roundStart := time.Now()
		resp, err := r.pollOnce(ctx, wait)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			var apiErr *APIError
			if errors.As(err, &apiErr) {
				if apiErr.IsTerminal() {
					log.Printf("[enroll] 节点已被吊销（%s），停止服务并清除凭据", apiErr.Code)
					return ErrRevoked
				}
				if apiErr.Code == ErrBootIDConflict {
					// 同一节点另有进程在轮询，退避久一点再试（契约 §5.3）
					log.Printf("[enroll] boot_id 冲突，60 秒后重试")
					if !sleepCtx(ctx, 60*time.Second) {
						return nil
					}
					continue
				}
			}

			attempt++
			r.consecutiveFailures++

			// 刚升级过且新版本连不上后端：换回旧版本再退出。
			// 没有这条路径，一个坏版本会让全部节点失联且无人能远程修
			// （复核 §7.4-2）。
			if r.shouldRollback() {
				log.Printf("[upgrade] 新版本连续 %d 次轮询失败，回滚到上一版本",
					r.consecutiveFailures)
				if err := Rollback(r.ExePath); err != nil {
					log.Printf("[upgrade] 回滚失败：%v", err)
				} else {
					return ErrUpgraded // 以 0 退出，systemd 拉起旧版本
				}
			}

			d := Backoff(attempt)
			log.Printf("[enroll] 轮询失败（%v），%v 后重试", err, d.Truncate(time.Millisecond))
			if !sleepCtx(ctx, d) {
				return nil
			}
			continue
		}

		attempt = 0
		// 能连上后端就说明当前版本是好的：清掉备份，避免日后误回滚
		if r.consecutiveFailures > 0 || r.ExePath != "" {
			r.consecutiveFailures = 0
			if r.ExePath != "" && HasPrevVersion(r.ExePath) {
				ClearPrevVersion(r.ExePath)
				log.Println("[upgrade] 新版本已确认可用，清除回滚备份")
			}
		}

		r.dropAcked(resp.Acked)

		// 升级回执已被后端确认 → 现在才能退出重启。
		// 顺序不能反：先退出的话回执丢失，后端会重投 upgrade_agent，
		// 而新版本已经在跑了（契约 §7.4-1）。
		if r.Executor.PendingExit() && r.allAcksConfirmed() {
			log.Println("[upgrade] 回执已确认，退出以启用新版本")
			return ErrUpgraded
		}

		newAcks := 0
		for _, cmd := range resp.Commands {
			ack := r.Executor.Execute(cmd, resp.ServerTime)
			r.pendingAcks = append(r.pendingAcks, ack)
			newAcks++
			log.Printf("[enroll] 指令 %s(%s) → %s", cmd.Type, shortID(cmd.CommandID), ack.Result)
		}

		// 刚产生回执时立即再轮一次把它送出去，不必挂满 25 秒。
		//
		// ⚠️ 只对**紧接着的那一次**生效：若判据写成"pendingAcks 非空就 wait=0"，
		// 后端一旦不把某个 id 放进 acked（后端 bug、契约版本错配），
		// agent 就会以 HTTP 请求的速度无限空转打后端。
		// 回执仍会在后续轮次继续携带，不会丢（契约 §5.2）。
		if newAcks > 0 {
			wait = 0
		} else {
			wait = clampWait(resp.NextWaitS)
		}

		// 兜底限速。
		//
		// 正常情况下服务端会挂满 wait 秒，这个 sleep 不会触发。
		// 但 agent 不能把限速完全托付给服务端：若后端提前返回
		// （挂起实现有 bug、走了缓存、或中间有代理不支持长连接），
		// 循环就会以 HTTP 请求的速度空转 —— 实测能到每秒两万次。
		// 这里保证两次轮询之间至少隔 minPollGap。
		if elapsed := time.Since(roundStart); elapsed < minPollGap {
			if !sleepCtx(ctx, minPollGap-elapsed) {
				return nil
			}
		}
	}
}

// minPollGap 是两次轮询之间的最小间隔，防止服务端提前返回时空转。
// 取 1 秒：远小于正常的 25 秒挂起，不影响指令延迟。
const minPollGap = time.Second

// shouldRollback 判断是否该回滚：存在备份且新版本连续多次连不上。
//
// 阈值 3 次：偶发网络抖动不该触发回滚，而真正的坏版本会持续失败。
func (r *Runner) shouldRollback() bool {
	const threshold = 3
	return r.ExePath != "" &&
		r.consecutiveFailures >= threshold &&
		HasPrevVersion(r.ExePath)
}

// allAcksConfirmed 报告是否所有回执都已被后端确认。
//
// 在 dropAcked 之后调用：队列为空即包括升级回执在内全部确认。
// 升级必须等到这一刻才能退出 —— 先退出则回执丢失，后端重投
// upgrade_agent，而新版本已经在跑了（契约 §7.4-1）。
func (r *Runner) allAcksConfirmed() bool {
	return len(r.pendingAcks) == 0
}

// ErrRevoked 表示后端已吊销本节点。
var ErrRevoked = errors.New("node revoked")

// ErrUpgraded 表示新版本已就位且回执已被后端确认，可以退出重启了。
//
// 调用方应以退出码 0 结束：unit 的 Restart=always 会拉起新二进制，
// 而 0 不触发 RestartPreventExitStatus=3（复核 §7.4-1）。
var ErrUpgraded = errors.New("agent upgraded, restart required")

// ErrEnrollFatal 表示注册遇到终态错误（token 已废、版本过低等）。
//
// 调用方应以 ExitRevoked 退出：token 不会自己变好，重试只会每 5 秒
// 重打同一个废 token，12 次后撞注册口的 30 次/小时 IP 限流；
// 而 install.sh 退出后这个循环还会一直跑下去。
var ErrEnrollFatal = errors.New("enrollment rejected")

func (r *Runner) pollOnce(ctx context.Context, wait int) (*PollResponse, error) {
	req := &PollRequest{
		BootID: r.bootID,
		Status: PollStatus{
			SingboxRunning: r.singboxRunning(),
			ListenPorts:    r.Info.ListenPorts(),
			UserCount:      r.Store.UserCount(),
			UptimeS:        int64(time.Since(r.startedAt).Seconds()),
		},
		Stats: r.collectStats(),
		Acks:  append([]CommandAck(nil), r.pendingAcks...),
	}
	return r.Client.Poll(ctx, r.node.PollURL, r.node.NodeSecret, wait, req)
}

// collectStats 读取本次 boot 以来的累计用量。
//
// ⚠️ 不做差、不清零：后端按 (node, uuid, boot_id) 取最大值并在 boot 变化时
// 并入历史基数（契约 §5.1、§7）。做差会在丢包时把用量算丢。
func (r *Runner) collectStats() []UserStat {
	if r.Stats == nil {
		return []UserStat{}
	}
	m, err := r.Stats.Collect()
	if err != nil {
		// 统计读不到不应中断心跳：节点在线状态比用量更要紧
		log.Printf("[enroll] 读取统计失败：%v", err)
		return []UserStat{}
	}
	out := make([]UserStat, 0, len(m))
	for _, st := range m {
		out = append(out, st)
	}
	return out
}

// dropAcked 删除后端已确认的回执；未确认的留到下次继续带（契约 §5.2）。
func (r *Runner) dropAcked(acked []string) {
	if len(acked) == 0 || len(r.pendingAcks) == 0 {
		return
	}
	done := make(map[string]struct{}, len(acked))
	for _, id := range acked {
		done[id] = struct{}{}
	}
	kept := r.pendingAcks[:0]
	for _, a := range r.pendingAcks {
		if _, ok := done[a.CommandID]; !ok {
			kept = append(kept, a)
		}
	}
	r.pendingAcks = kept
}

// Cleanup 终态清理：删除本地凭据（契约 §5.3）。
func (r *Runner) Cleanup() {
	if err := RemoveNodeFile(r.DataDir); err != nil {
		log.Printf("[enroll] 清除 node.json 失败：%v", err)
	}
}

func (r *Runner) singboxRunning() bool {
	if r.SingboxRunning == nil {
		return false
	}
	return r.SingboxRunning()
}

// ── 辅助 ──────────────────────────────────────────────────

func clampWait(n int) int {
	if n <= 0 || n > 25 {
		return 25
	}
	return n
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// detectOS 取 /etc/os-release 的 PRETTY_NAME，小写、≤64（契约 §4.1）
func detectOS() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			v = strings.Trim(strings.TrimSpace(v), `"`)
			v = strings.ToLower(v)
			if len(v) > 64 {
				v = v[:64]
			}
			return v
		}
	}
	return "unknown"
}

// detectArch 返回契约 §4.1 允许的取值：amd64 / arm64
func detectArch() string {
	return runtime.GOARCH
}
