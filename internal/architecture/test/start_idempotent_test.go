package test

// `Agent.Start()` **幂等**的判据（2026-11-26 实测缺陷）。
//
// # 缺陷实况
//
// 曾有**两个装配点**各自调一次 `Start()`：`kernelmod/agent` 的 `buildAgent`
// （装配层）与 `cmd/homed/runtime.go`（运行期起点）。真内核冷启动日志：
//
//	[main:main] main started          ← 装配层，此时 kernelloop 还没建
//	[agent] status section "security" re-registered ×3
//	[kernelloop] started
//	[main:main] main started          ← 运行期，这次才登记输入投递口
//
// 最严重的后果：`hooks.Runtime.Start` 跑两遍，而 mainagent 在那里
// `go m.SchedulerLoop()` 且**没有幂等守卫** ⇒ 同一主 agent **两个调度循环并发**。
// 压测当时测到 `max_concurrent=2`、响应时长成对（1615/1616/1617ms 各 ×2），
// 我把它误读成"抢占晋升"，真因就是它。
//
// # 为什么判据盯"类型 hook 调用次数"
//
// 那是**外部可观测**的事实（类型自己注册的回调被调了几次），
// 不依赖基座内部字段，也不数 goroutine。而它恰好是损害发生的地方：
// 类型在 hook 里起它的运行期循环。

import (
	"sync/atomic"
	"testing"

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// TestStartIsIdempotent 判 `Start()` 调 N 次，类型运行期只起一次。
//
// ★ 这条判据同时守住两件事：
//   - 装配层误调 `Start` 不会造成双循环（防守：判据在变异下会红）
//   - 真正该只有一处的地方（运行期起点）即使被将来的人再复制一份也炸不出来
func TestStartIsIdempotent(t *testing.T) {
	a := agentCore.New(agentCore.AgentConfig{ID: "idem-probe"})
	if a == nil {
		t.Fatal("New 返回 nil")
	}

	var starts int32
	// 用类型侧唯一的运行期钩子当探针：它就是 mainagent 起 SchedulerLoop 的地方。
	a.SetHooks(agentCore.Hooks{
		Runtime: agentCore.RuntimeHooks{
			Start: func(*agentCore.Agent) { atomic.AddInt32(&starts, 1) },
		},
	})

	a.Start()
	a.Start()
	a.Start()
	a.Stop()

	if got := atomic.LoadInt32(&starts); got != 1 {
		t.Fatalf("Start() 被调 3 次，类型运行期被启动 %d 次（应为 1）。\n"+
			"启动多次的后果：类型在 hook 里 `go ...Loop()` 通常没有幂等守卫，"+
			"同一个 agent 会有多个调度循环并发抢输入", got)
	}
}

// TestStartStillRegistersBaseSectionsOnce 判幂等**不削弱**副作用。
//
// 防御性守卫最容易顺手写坏的地方：用 once 把整段包住之后，
// 依赖它的东西（状态段 / 输入投递口）就再也补不上了。
// 这里正面钉住"Start 之后三段都在"。
func TestStartStillRegistersBaseSectionsOnce(t *testing.T) {
	a := agentCore.New(agentCore.AgentConfig{ID: "idem-base-probe"})
	if a == nil {
		t.Fatal("New 返回 nil")
	}
	a.Start()
	a.Stop()

	secs := a.CollectStatusSections()
	for _, k := range []string{"security", "runtime", "llm"} {
		if _, ok := secs[k]; !ok {
			t.Fatalf("Start 之后缺少基座状态段 %q —— once 守卫把副作用也一起挡掉了", k)
		}
	}
}
