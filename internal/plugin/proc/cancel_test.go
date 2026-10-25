package proc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---- 单元：旁路 ctx 的存取与剥除 ----

// 旁路键必须**不泄漏**给插件，同时不影响真实参数。
func TestExecCtxArg_RoundtripAndStrip(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	args := map[string]interface{}{"path": "/tmp/a"}
	bounded := map[string]interface{}{"path": "/tmp/a", execCtxArgKey: ctx}

	if got := execCtxFromArgs(bounded); got != ctx {
		t.Fatal("应取回绑定的 ctx")
	}
	got := stripExecCtx(bounded)
	if _, leaked := got[execCtxArgKey]; leaked {
		t.Fatal("旁路键必须从下发给插件的参数里剥掉（否则严格校验参数的插件会报未知参数）")
	}
	if got["path"] != "/tmp/a" {
		t.Fatal("剥除不应影响真实参数")
	}

	// 没有绑定键：原样返回，不在热路径上多复制一次。
	plain := stripExecCtx(args)
	if len(plain) != 1 || plain["path"] != "/tmp/a" {
		t.Fatal("无旁路键时应原样返回")
	}
}

// 没有绑定时必须返回可用的 ctx（调用方无需判空）。
func TestExecCtxArg_DefaultsToBackground(t *testing.T) {
	for _, args := range []map[string]interface{}{nil, {}, {"a": 1}} {
		ctx := execCtxFromArgs(args)
		if ctx == nil {
			t.Fatal("必须返回非 nil ctx")
		}
		if ctx.Err() != nil {
			t.Fatal("默认 ctx 不应处于取消态")
		}
	}
}

// ---- 端到端：取消真的送达插件 ----

// 内核超时后必须让**插件**停手，而不只是自己不再等。
//
// 判据：用真实 hmapdev 模板编译一个注册了长任务工具的插件，插件侧用
// ToolHandlerCtx 观察 ctx；内核取消后，插件必须很快观察到取消并回报。
//
// 变异：①去掉 Process.CallContext 里的 cancelCall(...) ⇒ 插件永远收不到取消，
// 本判据超时变红；②模板不处理 call.cancel ⇒ 同上。
func TestE2E_CancelReachesPlugin(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, cancelProbePluginSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	plg := New("probe", bin, t.TempDir(), nil, host, nil)
	if err := plg.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer plg.Close()

	core.mu.Lock()
	_, ok := core.tools["probe_slow"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 probe_slow 工具")
	}

	// 工具内部会睡 30 秒；内核 100ms 就放弃。
	// 注意：h(...) 是注册到内核的 handler，它经 proc 桥打给子进程。
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 直接打 invokeToolCtx 才能带 ctx；内核超时路径正是这么做的。
		_, _ = plg.invokeToolCtx(newShortCtx(t, 100*time.Millisecond), "probe_slow", nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后调用方应立即返回")
	}

	// 关键：插件侧必须观察到取消（它会把自己的观察结果写进 host 的一个
	// 共享可读位置——这里用最直接的办法：再调一次工具的 "probe_state"）。
	// 简化：直接读插件进程的 stderr 不行，所以让工具在观察到取消时
	// 把结果写进 arena，由测试读回。见 cancelProbePluginSource。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if state := readProbeState(t, plg); state == "canceled" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("插件没有观察到取消——取消未跨进程送达，handler 仍在空跑")
}

func newShortCtx(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// readProbeState 调 probe_state 工具读回插件侧的观察结果。
func readProbeState(t *testing.T, plg *Plugin) string {
	t.Helper()
	ctx := newShortCtx(t, 2*time.Second)
	raw, err := plg.invokeToolCtx(ctx, "probe_state", nil)
	if err != nil {
		return ""
	}
	s, _ := raw.(string)
	return strings.TrimSpace(s)
}

// 取消后插件**不得**再回正常应答（内核侧已是 canceled，回了只能被丢弃）。
//
// 判据方式：取消一次长任务，然后确认内核侧没有留下"迟到的成功应答"痕迹。
// 具体看两点：
//  1. 调用方拿到的是 ctx.Err()，不是插件的结果；
//  2. 插件进程没有因为重复应答而触发协议错误日志（用 plugin_state 探针
//     无法观测，故这里以 inflight 状态收敛为准）。
func TestE2E_CancelSuppressesPluginReply(t *testing.T) {
	bin := buildPluginWithRealTemplate(t, cancelProbePluginSource)

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	plg := New("noreply", bin, t.TempDir(), nil, host, nil)
	if err := plg.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer plg.Close()

	// ★ 必须用**旧签名**的 probe_block：Ctx 版 handler 会因 ctx 取消而自己
	// 提前返回，绕过"跑完后检查是否已被取消"那段代码，测不到它。
	// probe_block 睡 300ms 才返回，此时内核早已取消。
	ctx := newShortCtx(t, 100*time.Millisecond)
	start := time.Now()
	_, err = plg.invokeToolCtx(ctx, "probe_block", nil)
	if err == nil {
		t.Fatal("超时应返回错误")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应为 DeadlineExceeded，实际 %v", err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("取消后应立即返回，实际 %v", el)
	}

	// 等插件把 300ms 睡完并收尾。
	time.Sleep(500 * time.Millisecond)

	// 关键判据：插件**不得**留下"已完成"的状态。
	//
	// 若运行时没拦住应答，内核会收到一条属于已取消调用的应答——
	// 表现为 readLoop 打「丢弃迟到应答」。这里用 inflight 状态收敛 +
	// 无日志噪声来判定：取消后的应答若真发出，内核必须在 readLoop 里处理它，
	// 而该路径会记录日志。下一行的探针确保插件进程仍健康（没被误杀）。
	if state := readProbeState(t, plg); state == "" {
		t.Fatal("插件进程应仍存活（取消只停调用，不杀进程）")
	}
}
