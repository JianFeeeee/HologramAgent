package supervisor

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/network"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

const directAgentID types.AgentID = "main"

type Daemon struct {
	cfg             *types.Config
	nm              *network.Monitor
	trk             *tracker.Tracker
	agents          map[types.AgentID]*agentInstance
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	heartbeatSource func(id types.AgentID) (time.Time, types.HealthStatus, error)
	restartHandler  func(id types.AgentID)
}

type agentInstance struct {
	cfg        *types.AgentConfig
	state      types.AgentState
	health     types.HealthStatus
	lastHB     time.Time
	failCount  int
	useTracker bool
}

func New(cfg *types.Config) *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	nm := network.NewMonitor(cfg.Daemon.CheckInterval)

	return &Daemon{
		cfg:    cfg,
		nm:     nm,
		agents: make(map[types.AgentID]*agentInstance),
		ctx:    ctx,
		cancel: cancel,
	}
}

func (d *Daemon) SetTracker(trk *tracker.Tracker) {
	d.trk = trk
}

// SetHeartbeatSource 接入 agent 真实存活探测源（direct 模式下为同进程 agent core 状态）。
// 成功返回时 lastHB 才更新；无源时 agent 健康保持未知。
func (d *Daemon) SetHeartbeatSource(fn func(id types.AgentID) (time.Time, types.HealthStatus, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.heartbeatSource = fn
}

// SetRestartHandler 接入真实重启动作（direct 模式下由 homed 注册"清理后以特殊码退出"，交给 guard/systemd 重建）。
func (d *Daemon) SetRestartHandler(fn func(id types.AgentID)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restartHandler = fn
}

func (d *Daemon) Start() error {
	log.Println("[homed] starting HomeAgent daemon")

	go d.nm.Start(d.ctx, d.cfg.Defaults.LLMEndpoints)

	go d.healthLoop()

	log.Println("[homed] daemon started successfully")
	return nil
}

func (d *Daemon) Shutdown() {
	log.Println("[homed] shutting down...")
	d.cancel()

	d.mu.Lock()
	defer d.mu.Unlock()

	for id, agent := range d.agents {
		if agent.state == types.AgentStateRunning {
			log.Printf("[homed] stopping agent %s", id)
			agent.state = types.AgentStateStopped
		}
	}
}

func (d *Daemon) RegisterAgent(id types.AgentID) {
	d.mu.Lock()
	defer d.mu.Unlock()

	useTrk := d.trk != nil
	maxRetries := d.cfg.Defaults.RollbackPolicy.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	cfg := &types.AgentConfig{ID: id}
	cfg.RollbackPolicy.MaxRetries = maxRetries

	d.agents[id] = &agentInstance{
		cfg:        cfg,
		state:      types.AgentStateRunning,
		health:     types.HealthHealthy,
		lastHB:     time.Now(),
		useTracker: useTrk,
	}
	log.Printf("[homed] agent %s registered (tracker=%v, maxRetries=%d)", id, useTrk, maxRetries)
}

func (d *Daemon) healthLoop() {
	interval := d.cfg.Daemon.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			d.checkAllAgents()
		case <-d.ctx.Done():
			return
		}
	}
}

func (d *Daemon) checkAllAgents() {
	d.mu.RLock()
	agents := make(map[types.AgentID]*agentInstance)
	for id, a := range d.agents {
		agents[id] = a
	}
	d.mu.RUnlock()

	for id, agent := range agents {
		d.checkAgent(id, agent)
	}
}

func (d *Daemon) checkAgent(id types.AgentID, agent *agentInstance) {
	// 1) agent 存活源：只有确认存活才更新 lastHB
	d.mu.RLock()
	hbSrc := d.heartbeatSource
	d.mu.RUnlock()

	if hbSrc != nil {
		hbTime, hbHealth, err := hbSrc(id)
		if err != nil || hbTime.IsZero() {
			agent.health = types.HealthDown
			log.Printf("[homed] agent %s heartbeat lost: %v", id, err)
		} else {
			agent.lastHB = hbTime
			agent.health = hbHealth
		}
	} else {
		agent.health = types.HealthUnknown
	}

	// 2) 网络：仅在配置了探活端点时才据此判定降级
	netStatus := d.nm.AggregateResult()
	if netStatus.EndpointsConfigured && !netStatus.LLMAPIReachable {
		if agent.health != types.HealthDown {
			agent.health = types.HealthDegraded
		}
		agent.failCount++
		log.Printf("[homed] agent %s: LLM API unreachable (fail %d)", id, agent.failCount)
	} else if agent.health == types.HealthHealthy {
		agent.failCount = 0
	}

	if agent.failCount >= agent.cfg.RollbackPolicy.MaxRetries {
		d.handleFailure(id, agent)
	}
}

func (d *Daemon) handleFailure(id types.AgentID, agent *agentInstance) {
	log.Printf("[homed] agent %s failed %d times, initiating recovery", id, agent.failCount)

	if agent.useTracker && d.trk != nil {
		log.Printf("[homed] rolling back agent %s via change tracker", id)
		if err := d.trk.Rollback(); err != nil {
			log.Printf("[homed] tracker rollback failed: %v — restarting agent", err)
			d.restartAgent(id, agent)
			return
		}
		log.Printf("[homed] agent %s tracker rollback complete", id)
		agent.failCount = 0
		return
	}

	d.restartAgent(id, agent)
}

// Restart 主动请求重建本单元（**热更新入口**）。
//
// ## 为什么需要这个导出方法
//
// 「换二进制而不 systemd restart」的能力一直都存在：worker 以
// exitRestartRequested(42) 优雅退出 → guard 检测到 42 → 用**新二进制**
// 重新拉起 worker（cmd/homed/guard.go 的 workerRestartRequested 分支）。
//
// 但此前**没有任何外部办法触发它** —— restartHandler 只被
// handleFailure（健康检查失败）调用。于是"热更新"成了一项
// 只有崩溃时才生效的能力，运维实际只能 systemctl restart。
//
// 本方法把那条通道接出来：语义与"健康检查判定需要重建"完全一致，
// 但由人主动发起。
//
// ## 与 systemctl restart 的区别（为何优先用它）
//
//   - 不经过 systemd，**guard 进程保持存活**（双进程架构里 guard 是父）
//   - 不计入失败轮次（agent.failCount 清零）
//   - 有优雅停机：停插件、flush 配置、关 tracker、sup.Shutdown()
//
// 返回错误表示"没有注册真实重启通道"——那种情况下**不会**假装成功。
func (d *Daemon) Restart(id types.AgentID) error {
	d.mu.RLock()
	agent, ok := d.agents[id]
	d.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent %s 未注册", id)
	}
	d.restartAgent(id, agent)
	return nil
}

func (d *Daemon) restartAgent(id types.AgentID, agent *agentInstance) {
	agent.state = types.AgentStateStopped
	agent.failCount = 0

	d.mu.RLock()
	handler := d.restartHandler
	d.mu.RUnlock()

	if handler != nil {
		log.Printf("[homed] agent %s restart handler invoked", id)
		handler(id)
		return
	}

	// 无真实重启通道：退回内存态复位（记录，不再假装成功）
	d.RegisterAgent(id)
	agent.failCount = 0
	log.Printf("[homed] agent %s reset in-memory only (no restart handler registered)", id)
}

func (d *Daemon) GetAgentStatus(id types.AgentID) (*AgentStatus, error) {
	d.mu.RLock()
	agent, ok := d.agents[id]
	d.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("agent %s not found", id)
	}

	netStatus := d.nm.AggregateResult()

	trackerStats := map[string]interface{}{"active": false}
	if d.trk != nil {
		trackerStats = d.trk.Stats()
	}

	return &AgentStatus{
		ID:           id,
		State:        agent.state,
		Health:       agent.health,
		Uptime:       time.Since(agent.lastHB),
		Network:      netStatus,
		TrackerStats: trackerStats,
	}, nil
}

func (d *Daemon) ListAgents() []AgentStatus {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var statuses []AgentStatus
	for id, agent := range d.agents {
		statuses = append(statuses, AgentStatus{
			ID:     id,
			State:  agent.state,
			Health: agent.health,
		})
	}
	return statuses
}

func (d *Daemon) PreActionSnapshot(id types.AgentID) (*types.Snapshot, error) {
	return nil, fmt.Errorf("snapshot not supported in direct mode — use tracker instead")
}

func (d *Daemon) RollbackAgent(id types.AgentID, snapID types.SnapshotID) error {
	if d.trk != nil {
		return d.trk.Rollback()
	}
	return fmt.Errorf("no tracker available for rollback")
}

// AgentStatus 已迁入内置 SDK，此处保留别名以兼容现有调用方。
type AgentStatus = sdk.AgentStatus
