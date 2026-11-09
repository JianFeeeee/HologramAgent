package test

// internal/supervisor 的黑盒判据：New(cfg) → RegisterAgent → 状态输出。
// 不启动后台循环、不碰 d.agents。

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func newDaemon() *supervisor.Daemon {
	return supervisor.New(&types.Config{Daemon: types.DaemonConfig{
		CheckInterval:     time.Minute,
		HeartbeatInterval: 30 * time.Second,
	}})
}

// TestFreshDaemonHasNoAgents：新 daemon 的 ListAgents 为空。
func TestFreshDaemonHasNoAgents(t *testing.T) {
	d := newDaemon()
	if got := d.ListAgents(); len(got) != 0 {
		t.Fatalf("★ 新 daemon 的 ListAgents = %v，期望空", got)
	}
}

// TestRegisterThenList：注册后 ListAgents 与 GetAgentStatus 反映该 agent。
func TestRegisterThenList(t *testing.T) {
	d := newDaemon()
	id := types.AgentID("contract-agent")
	d.RegisterAgent(id)
	list := d.ListAgents()
	if len(list) != 1 {
		t.Fatalf("★ ListAgents = %v，期望 1 个", list)
	}
	st, err := d.GetAgentStatus(id)
	if err != nil {
		t.Fatalf("GetAgentStatus: %v", err)
	}
	if st == nil {
		t.Fatal("★ 已注册 agent 的状态为 nil")
	}
}

// TestGetUnknownAgentReportsError：查未注册 agent 必须报错。
func TestGetUnknownAgentReportsError(t *testing.T) {
	d := newDaemon()
	if _, err := d.GetAgentStatus(types.AgentID("ghost")); err == nil {
		t.Fatal("★ 查未注册 agent 却返回成功")
	}
}

// TestSDKAdapterSeesSameAgents：适配层与 Daemon 看到同一份登记。
func TestSDKAdapterSeesSameAgents(t *testing.T) {
	d := newDaemon()
	d.RegisterAgent(types.AgentID("a1"))
	ad := supervisor.NewSDKAdapter(d)
	if got := ad.ListAgents(); len(got) != 1 {
		t.Fatalf("★ SDKAdapter.ListAgents = %v，期望 1 个", got)
	}
}
