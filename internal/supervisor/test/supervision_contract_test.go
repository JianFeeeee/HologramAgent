package test

// internal/supervisor 的**健康监督 → 恢复**判据 —— 面向接口的黑盒。
//
// 入口只有 supervisor.New(cfg) 与 Daemon 的导出方法（SetHeartbeatSource /
// SetRestartHandler / Start / Shutdown / GetAgentStatus）。agent 内部状态、
// failCount、agentInstance 都不在断言范围内。
//
// ## 判据要覆盖的职责链
//
//	心跳源报告异常 → 健康转 Down → 连续失败达阈值 → 触发恢复动作
//
// 这条链路此前完全没有判据（`daemon_contract_test.go` 只测「登记后能查」）。
// 而它是 homed 的**自愈能力本体**：判错了就没人重启 agent，判对了却重启错
// 就会掩盖真实问题。
//
// ## 为什么不等真实的 30s ticker
//
// 判据把 HeartbeatInterval 设成 10ms，由 Daemon 自己的 healthLoop 驱动。
// 不手动调用内部检查函数（那是私有符号），也不伪造时钟 —— 测的是真实循环。

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// tickBudgets 是判据等待 healthLoop 走到下一步的上限。
//
// 判据把心跳间隔设成 10ms；给 3s 是给 CI 上调度抖动留的余量，
// 不是"默认很慢所以多等等"。
const tickBudget = 3 * time.Second

// unhealthyEndpoint 是一个必然不可达的探活地址（本机 1 端口）。
// 用它驱动"LLM 不可达"这条分支，而不是伪造网点/改 DNS。
const unhealthyEndpoint = "http://127.0.0.1:1/"

// newFastDaemon 造一个巡检周期极短的 daemon。
//
// llmEndpoint 是喂给网络监视器的探活端点 —— 传空串表示"不配端点"。
//
// ★ 为什么恢复动作要靠端点而不是心跳：checkAgent 里 failCount 只因
// 「端点已配置且 LLM 不可达」递增；心跳报 Down 只改健康态，不累加失败。
// 这是既有设计（一个 Down 的 agent 不等同于要重启的 agent），
// 判据不该自己发明语义，所以恢复类判据一律配一个不可达端点来驱动。
func newFastDaemon(t *testing.T, maxRetries int, llmEndpoint string) *supervisor.Daemon {
	t.Helper()
	cfg := &types.Config{
		Daemon: types.DaemonConfig{
			HeartbeatInterval: 10 * time.Millisecond,
			CheckInterval:     time.Millisecond,
		},
		Defaults: types.AgentConfig{
			RollbackPolicy: types.RollbackPolicy{MaxRetries: maxRetries},
		},
	}
	if llmEndpoint != "" {
		cfg.Defaults.LLMEndpoints = []string{llmEndpoint}
	}
	d := supervisor.New(cfg)
	if err := d.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(d.Shutdown)
	return d
}

// healthOf 读出某 agent 的当前健康态。
func healthOf(t *testing.T, d *supervisor.Daemon, id types.AgentID) types.HealthStatus {
	t.Helper()
	st, err := d.GetAgentStatus(id)
	if err != nil {
		t.Fatalf("GetAgentStatus(%s): %v", id, err)
	}
	return st.Health
}

// waitHealth 在预算内等健康态变成 want，返回是否等到。
func waitHealth(t *testing.T, d *supervisor.Daemon, id types.AgentID, want types.HealthStatus) bool {
	t.Helper()
	deadline := time.Now().Add(tickBudget)
	for time.Now().Before(deadline) {
		if healthOf(t, d, id) == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ---- 判据 1：心跳源的判词必须真的改变健康态 ----

// TestHeartbeatVerdictDrivesHealth：注入心跳源后，它说的健康态就是对外
// 可见的健康态。
//
// 变异：checkAgent 里忽略 hbHealth（只更新 lastHB）⇒ 红。
//
// 这是"监督是否真的在看"的最低要求：源说 Down，查出来还是 Healthy，
// 那整条自愈链就是装饰。
func TestHeartbeatVerdictDrivesHealth(t *testing.T) {
	d := newFastDaemon(t, 5, "")
	const id = types.AgentID("watch-me")
	d.RegisterAgent(id)

	var down atomic.Bool
	d.SetHeartbeatSource(func(got types.AgentID) (time.Time, types.HealthStatus, error) {
		if down.Load() {
			return time.Now(), types.HealthDown, nil
		}
		return time.Now(), types.HealthHealthy, nil
	})

	if !waitHealth(t, d, id, types.HealthHealthy) {
		t.Fatalf("★ 心跳源报 Healthy，对外却是 %v —— 判词没被采纳", healthOf(t, d, id))
	}

	down.Store(true)
	if !waitHealth(t, d, id, types.HealthDown) {
		t.Fatalf("★ 心跳源改报 Down，对外仍是 %v —— 判词没被采纳", healthOf(t, d, id))
	}
}

// ---- 判据 2：心跳源报错（而不是报 Down）同样要落成 Down ----

// TestHeartbeatErrorMeansDown：心跳读取失败（agent 进程消失、通道断了）
// 与"读取成功但状态是 Down"必须同等对待 —— 都是"这个 agent 不可用"。
//
// 若实现把错误忽略（当 Healthy），一个已经消失的 agent 会永远显示健康。
func TestHeartbeatErrorMeansDown(t *testing.T) {
	d := newFastDaemon(t, 5, "")
	const id = types.AgentID("vanished")
	d.RegisterAgent(id)

	d.SetHeartbeatSource(func(got types.AgentID) (time.Time, types.HealthStatus, error) {
		return time.Time{}, types.HealthHealthy, errProbe
	})

	if !waitHealth(t, d, id, types.HealthDown) {
		t.Fatalf("★ 心跳读取报错，健康态却是 %v —— 消失的 agent 会永远显示健康",
			healthOf(t, d, id))
	}
}

// ---- 判据 3：没接心跳源 = 未知，不是健康 ----

// TestNoHeartbeatSourceIsUnknownNotHealthy：没接探测源时不能假装健康。
//
// 这条防的是"默认乐观"：如果无源当作 Healthy，一台什么都没接的 homed 会
// 报告一切正常，而它实际上没在监督任何东西。
func TestNoHeartbeatSourceIsUnknownNotHealthy(t *testing.T) {
	d := newFastDaemon(t, 5, "")
	const id = types.AgentID("unwatched")
	d.RegisterAgent(id)

	if !waitHealth(t, d, id, types.HealthUnknown) {
		t.Fatalf("★ 未接心跳源，健康态是 %v（应为 Unknown）—— 默认乐观会谎报健康",
			healthOf(t, d, id))
	}
}

// ---- 判据 4：连续失败达阈值必须触发恢复动作 ----

// TestPersistentFailureTriggersRestartHandler：连续 N 次失败后，注册的重启
// 通道必须被调用。
//
// 这是整条链的终点，也是唯一有实际副作用的一步：不调用它就等于"检测到了
// 但什么都不做"，agent 会一直躺在故障里。
//
// 阈值取 2（而非默认 3）让判据更快，语义不变。
func TestPersistentFailureTriggersRestartHandler(t *testing.T) {
	d := newFastDaemon(t, 2, unhealthyEndpoint)
	const id = types.AgentID("doomed")
	d.RegisterAgent(id)

	// 心跳正常，但 LLM 端点不可达 ⇒ 每轮累加失败，达阈值后必须恢复。
	d.SetHeartbeatSource(func(got types.AgentID) (time.Time, types.HealthStatus, error) {
		return time.Now(), types.HealthHealthy, nil
	})

	var restarts atomic.Int32
	done := make(chan struct{})
	d.SetRestartHandler(func(got types.AgentID) {
		if restarts.Add(1) == 1 {
			close(done)
		}
	})

	select {
	case <-done:
	case <-time.After(tickBudget):
		t.Fatalf("★ 连续失败达到阈值(%d)后没有触发重启通道（重试次数=%d）—— "+
			"检测到了却什么都不做", 2, restarts.Load())
	}
}

// ---- 判据 5：未注册的 agent 不参与监督 ----

// TestUnregisteredAgentNeverTriggersRecovery：Restart 对未注册 agent 必须报错，
// 且监督循环不会凭空造出一个 agent。
//
// 若循环遍历到不存在的 id，会给一个从未启动的东西发恢复动作 ——
// 在生产里表现为"日志里重启了一个没有的 agent"。
func TestUnregisteredAgentNeverTriggersRecovery(t *testing.T) {
	d := newFastDaemon(t, 1, unhealthyEndpoint)
	var touched atomic.Int32
	d.SetRestartHandler(func(got types.AgentID) { touched.Add(1) })

	if err := d.Restart(types.AgentID("ghost")); err == nil {
		t.Fatal("★ Restart 对未注册 agent 返回成功")
	}

	time.Sleep(50 * time.Millisecond) // 让 healthLoop 跑几轮
	if n := touched.Load(); n != 0 {
		t.Fatalf("★ 未注册任何 agent，重启通道却被调用 %d 次", n)
	}
}

// ---- 判据 6：Shutdown 后不再有监督动作 ----

// TestShutdownStopsSupervision：Shutdown 之后循环必须停。
//
// 不停的话，进程退出过程中仍会发恢复动作；而"退出中还要重启 agent"
// 正是 guard/systemd 那一层最难解释的现象。
func TestShutdownStopsSupervision(t *testing.T) {
	d := newFastDaemon(t, 1, unhealthyEndpoint)
	const id = types.AgentID("closing")
	d.RegisterAgent(id)

	var restarts atomic.Int32
	d.SetRestartHandler(func(got types.AgentID) { restarts.Add(1) })
	d.SetHeartbeatSource(func(got types.AgentID) (time.Time, types.HealthStatus, error) {
		return time.Now(), types.HealthHealthy, nil
	})

	select {
	case <-waitUntil(func() bool { return restarts.Load() > 0 }, tickBudget):
	case <-time.After(tickBudget):
		t.Fatal("前置：Shutdown 前应已触发过恢复动作")
	}

	d.Shutdown()
	before := restarts.Load()
	time.Sleep(100 * time.Millisecond)

	if after := restarts.Load(); after != before {
		t.Fatalf("★ Shutdown 之后仍触发恢复 %d 次（%d → %d）",
			after-before, before, after)
	}
}

// ---- 支撑 ----

type probeErr struct{}

func (probeErr) Error() string { return "probe: heartbeat channel broken" }

var errProbe error = probeErr{}

func waitUntil(cond func() bool, budget time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		deadline := time.Now().Add(budget)
		for time.Now().Before(deadline) {
			if cond() {
				close(ch)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}
