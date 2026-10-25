package proc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Process 的测试用真实子进程（go build 出的小二进制），而非 mock：
// 崩溃隔离、EOF 感知、Kill 回收这些性质只有真进程才能验证——
// 它们恰是迁移相对 C ABI 的核心收益（§9.5）。

// buildTestPlugin 编译 testdata 下的假插件，返回二进制路径。
func buildTestPlugin(t *testing.T, srcName string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("环境无 go 工具链，跳过子进程测试")
	}

	src := filepath.Join("testdata", srcName)
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("测试插件源码缺失 %s: %v", src, err)
	}

	bin := filepath.Join(t.TempDir(), strings.TrimSuffix(srcName, ".go"))
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("编译测试插件 %s 失败: %v\n%s", srcName, err, out)
	}
	return bin
}

// noopHandler 是最简的内核侧 handler（测试中不需要真实 core.* 能力）。
func noopHandler(method string, params json.RawMessage) (interface{}, error) {
	return nil, fmt.Errorf("测试环境未实现 %s", method)
}

func TestProcess_SpawnHandshakeAndToolInvoke(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")

	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	if p.PID() == 0 {
		t.Error("PID 应非零")
	}

	raw, err := p.Call(MethodToolInvoke, ToolInvokeParams{
		Name: "echo_tool",
		Args: map[string]interface{}{"text": "你好"},
	})
	if err != nil {
		t.Fatalf("tool.invoke: %v", err)
	}
	var res ToolInvokeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("解析应答: %v", err)
	}
	if res.Result != "你好" {
		t.Fatalf("工具应回显 '你好'，实际 %v", res.Result)
	}
}

// 插件返回错误时调用方必须收到 error —— 对比 C ABI 路径的 output_send
// 永远返回成功（§9.4，现网 2 次消息发不出而模型以为成功）。
func TestProcess_PluginErrorIsReported(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	_, err = p.Call(MethodToolInvoke, ToolInvokeParams{Name: "fail_tool"})
	if err == nil {
		t.Fatal("插件返回错误时调用方应收到 error")
	}
	if !strings.Contains(err.Error(), "故意失败") {
		t.Errorf("错误信息应透传插件的原因，实际: %v", err)
	}
}

// 插件反向调用内核（51 个 core.* method 的机制验证）。
func TestProcess_PluginCallsBackIntoKernel(t *testing.T) {
	bin := buildTestPlugin(t, "callbackplugin.go")

	var (
		mu      sync.Mutex
		gotCall []string
	)
	handler := func(method string, params json.RawMessage) (interface{}, error) {
		mu.Lock()
		gotCall = append(gotCall, method)
		mu.Unlock()
		switch method {
		case MethodSettingsGet:
			return map[string]interface{}{"value": "配置值"}, nil
		case MethodToolRegister:
			return nil, nil
		}
		return nil, fmt.Errorf("未实现 %s", method)
	}

	p, err := Spawn("cb", bin, Options{Handler: handler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	// plugin.start 期间插件会回调 tool.register + settings.get
	if _, err := p.Call(MethodPluginStart, nil); err != nil {
		t.Fatalf("plugin.start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotCall) < 2 {
		t.Fatalf("内核应收到插件的反向调用，实际 %v", gotCall)
	}
	hasRegister, hasSettings := false, false
	for _, m := range gotCall {
		if m == MethodToolRegister {
			hasRegister = true
		}
		if m == MethodSettingsGet {
			hasSettings = true
		}
	}
	if !hasRegister || !hasSettings {
		t.Errorf("应收到 tool.register 与 settings.get，实际 %v", gotCall)
	}
}

// 崩溃隔离：插件 panic 只让子进程退出，内核存活并收到 onExit（§9.5 表格第 2 行）。
// C ABI 路径下 panic 跨 C 栈，recover 兜不住会带崩整个 homed（§1.4）。
func TestProcess_CrashIsolationAndExitDetection(t *testing.T) {
	bin := buildTestPlugin(t, "crashplugin.go")

	exitCh := make(chan error, 1)
	p, err := Spawn("crash", bin, Options{
		Handler: noopHandler,
		OnExit:  func(name string, err error) { exitCh <- err },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// 触发插件 panic
	_, callErr := p.Call(MethodToolInvoke, ToolInvokeParams{Name: "boom"})
	if callErr == nil {
		t.Error("插件崩溃时在途调用应返回错误，而非挂死")
	}

	select {
	case exitErr := <-exitCh:
		if exitErr == nil {
			t.Error("panic 退出应报告非 nil 错误（供 recordCrash 使用）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未在 5s 内检测到进程退出（EOF 感知失效）")
	}

	select {
	case <-p.Exited():
	case <-time.After(time.Second):
		t.Error("Exited() 通道应已关闭")
	}

	// 进程已退出后继续调用应立即失败，不能挂死
	if _, err := p.Call(MethodToolInvoke, ToolInvokeParams{Name: "echo_tool"}); !errors.Is(err, ErrProcessExited) {
		t.Errorf("退出后调用应返回 ErrProcessExited，实际 %v", err)
	}
}

// 优雅停止：plugin.stop 后进程自行退出。
func TestProcess_GracefulStop(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Exited():
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 后进程应退出")
	}
	if err := p.ExitError(); err != nil {
		t.Errorf("优雅停止应无错误退出，实际 %v", err)
	}
}

// **真正的取消**：卡死的插件可被 Kill 回收（§9.3 对照 cgo 超时线程永久泄漏）。
func TestProcess_KillHungPlugin(t *testing.T) {
	bin := buildTestPlugin(t, "hangplugin.go")
	p, err := Spawn("hang", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// 调用会卡住，用 context 超时返回（调用方不被拖死）
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err = p.CallContext(ctx, MethodToolInvoke, ToolInvokeParams{Name: "hang_tool"})
	if err == nil {
		t.Fatal("卡死的调用应因 ctx 超时返回")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("应为 DeadlineExceeded，实际 %v", err)
	}

	// 在途条目必须被推进到**可解释的状态**，且最终会被回收（不泄漏）。
	//
	// 判据从"表必须立刻清空"改成了下面两条，因为语义改了：
	// 取消后条目**刻意保留**一会（cancelRetainFor），以便迟到的应答能被
	// 认出归属，而不是被当成"未知 id"丢弃。真正要防的泄漏是
	// "条目永远留着、状态说不清"，而不是"取消后短暂保留"。
	p.mu.Lock()
	residual := make([]*call, 0, len(p.inflight))
	for _, c := range p.inflight {
		residual = append(residual, c)
	}
	p.mu.Unlock()
	for _, c := range residual {
		// 允许 pending（应答还在路上）或 canceled（已放弃、等迟到应答）；
		// done/failed 留在表里才是真泄漏（它们终态且不会再有后续）。
		if st := c.State(); st == callDone || st == callFailed {
			t.Errorf("终态调用 id=%d 仍留在 inflight 表里（泄漏）", c.id)
		}
	}

	// Kill 真正回收资源
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-p.Exited():
	case <-time.After(3 * time.Second):
		t.Fatal("Kill 后进程应退出")
	}
}

// 通知（ID=0）不等应答——事件投递路径必须 post-and-forget（§2.4 约束 B）。
func TestProcess_NotifyDoesNotWait(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	start := time.Now()
	for i := 0; i < 100; i++ {
		if err := p.Notify("event.deliver", map[string]interface{}{"seq": i}); err != nil {
			t.Fatalf("Notify: %v", err)
		}
	}
	elapsed := time.Since(start)
	// 100 条通知若每条都等应答，至少要 100 个往返；post-and-forget 应远快于此
	if elapsed > time.Second {
		t.Errorf("100 条通知耗时 %v，疑似在等应答（应 post-and-forget）", elapsed)
	}

	// 通知不占在途表
	p.mu.Lock()
	inflightCount := len(p.inflight)
	p.mu.Unlock()
	if inflightCount != 0 {
		t.Errorf("通知不应占用 inflight 表，实际 %d 条", inflightCount)
	}
}

// 并发调用：pending 表按 ID 正确路由，应答不串。
func TestProcess_ConcurrentCallsRouteCorrectly(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	p, err := Spawn("echo", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := fmt.Sprintf("msg-%d", i)
			raw, err := p.Call(MethodToolInvoke, ToolInvokeParams{
				Name: "echo_tool",
				Args: map[string]interface{}{"text": want},
			})
			if err != nil {
				errs <- err
				return
			}
			var res ToolInvokeResult
			if err := json.Unmarshal(raw, &res); err != nil {
				errs <- err
				return
			}
			if res.Result != want {
				errs <- fmt.Errorf("应答串了：期望 %q，实际 %v", want, res.Result)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发调用失败: %v", err)
	}
}

// 主版本不匹配必须显式拒绝，不能半兼容运行。
//
// 为什么需要显式拦下：主版本两端（如 C ABI 时代与子进程时代）的帧格式与
// 共享段布局都不同，错配不会报错、只会静默失效（空参数、字段被忽略）。
// 判据是双方版本号的**主版本段**，不是任何自定协议号。
func TestProcess_MajorVersionMismatchRejected(t *testing.T) {
	bin := buildTestPlugin(t, "badprotoplugin.go")
	_, err := Spawn("badproto", bin, Options{Handler: noopHandler})
	if err == nil {
		t.Fatal("主版本不匹配应拒绝建链")
	}
	if !strings.Contains(err.Error(), "主版本不匹配") {
		t.Errorf("错误应说明主版本不匹配，实际: %v", err)
	}
	// 运维可读性：光报“不匹配”不能定位到行动。生产上碰到它的现场是
	// “只更新了内核没重编插件”，所以错误里必须带出这条修复指令。
	if !strings.Contains(err.Error(), "hmapdev") || !strings.Contains(err.Error(), "重编") {
		t.Errorf("错误应给出重编插件的修复指令，实际: %v", err)
	}
}

// 同主版本内的中版本差异（内核 1.4.0 vs 插件 SDK 1.1.0）必须**允许**建链。
//
// 这是 §七「1.x 内纯追加、不调新面就不用重编」规约的落地：若不校验主版本
// 而是全等比较，未重编的存量插件会被无谓地全部拒绝。
func TestProcess_SameMajorDifferentMinorAccepted(t *testing.T) {
	bin := buildTestPlugin(t, "oldminorplugin.go")
	p, err := Spawn("oldminor", bin, Options{Handler: noopHandler})
	if err != nil {
		t.Fatalf("同主版本（1.x）应允许建链: %v", err)
	}
	defer func() { _ = p.Stop() }()
}

func TestProcess_SpawnRequiresHandler(t *testing.T) {
	if _, err := Spawn("x", "/bin/true", Options{}); err == nil {
		t.Fatal("缺少 Handler 应报错（插件无法回调内核）")
	}
}

// 插件死亡但孙子进程仍持有 stdout 写端时，内核必须仍能感知退出。
//
// 这是「EOF 不等于进程死亡」的回归测试。旧实现只在 readLoop 读到 EOF 后
// 才 markExited，而 exec.Command 起的孙子进程默认继承插件的 stdout：
// 插件本体退出后写端仍被孙子持有，EOF 永不到来，于是
//   - 在途调用挂到自己的超时；
//   - OnExit 不触发 → 崩溃计数、工具摘除、自动重启全都不发生；
//   - 进程表里插件已是僵尸，注册表里却一切正常。
//
// 生产上 browser 拉 chromium、editdoc 拉 python 正是这个形状。
// 现在由专职 waitLoop 直接 wait4(2) 判定，不再依赖 fd 生命周期。
func TestProcess_ExitDetectedDespiteInheritedStdout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("环境无 sleep，跳过")
	}
	bin := buildTestPlugin(t, "forkplugin.go")

	exitCh := make(chan error, 1)
	p, err := Spawn("fork", bin, Options{
		Handler: noopHandler,
		OnExit:  func(name string, err error) { exitCh <- err },
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Kill()

	// 让插件本体退出（孙子 sleep 300 仍活着，继续持有 stdout 写端）
	if _, callErr := p.Call(MethodToolInvoke, ToolInvokeParams{Name: "die"}); callErr == nil {
		t.Error("插件退出时在途调用应返回错误")
	}

	select {
	case exitErr := <-exitCh:
		if exitErr == nil {
			t.Error("非零退出码应报告为错误（供崩溃计数使用）")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("孙子进程持有 stdout 时未能感知插件退出——退化回只靠 EOF 判定")
	}

	if _, err := p.Call(MethodToolInvoke, ToolInvokeParams{Name: "x"}); !errors.Is(err, ErrProcessExited) {
		t.Errorf("退出后调用应返回 ErrProcessExited，实际 %v", err)
	}
}

// Supervisor 台账：握手成功即在册，进程退出即注销。
func TestSupervisor_TrackAndUntrack(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	sup := NewSupervisor()

	p, err := Spawn("echo", bin, Options{Handler: noopHandler, Supervisor: sup})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sup.Count() != 1 {
		t.Fatalf("握手成功后应在册，实际 %d", sup.Count())
	}
	got, ok := sup.Get("echo")
	if !ok || got.PID() != p.PID() {
		t.Errorf("台账里的进程应是刚 spawn 的那个")
	}
	list := sup.List()
	if len(list) != 1 || !list[0].Alive || list[0].PID != p.PID() {
		t.Errorf("List 应报告存活与 PID，实际 %+v", list)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 退出回调在 markExited 里注销，等它落地
	deadline := time.Now().Add(3 * time.Second)
	for sup.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sup.Count() != 0 {
		t.Errorf("进程退出后应注销，实际仍有 %d 个在册", sup.Count())
	}
}

// StopAll 必须停掉全部在册子进程——内核关停时不留孤儿。
func TestSupervisor_StopAllLeavesNoSurvivor(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	sup := NewSupervisor()

	var procs []*Process
	for i := 0; i < 3; i++ {
		p, err := Spawn(fmt.Sprintf("echo%d", i), bin, Options{Handler: noopHandler, Supervisor: sup})
		if err != nil {
			t.Fatalf("Spawn %d: %v", i, err)
		}
		procs = append(procs, p)
	}
	if sup.Count() != 3 {
		t.Fatalf("应有 3 个在册，实际 %d", sup.Count())
	}

	sup.StopAll(5 * time.Second)

	for _, p := range procs {
		select {
		case <-p.Exited():
		case <-time.After(2 * time.Second):
			t.Errorf("%s 未被 StopAll 停掉（会成为孤儿进程）", p.Name())
		}
	}
}

// 卡死插件（不响应 plugin.stop）必须在 StopAll 的预算内被强杀。
func TestSupervisor_StopAllKillsUnresponsive(t *testing.T) {
	bin := buildTestPlugin(t, "hangplugin.go")
	sup := NewSupervisor()

	p, err := Spawn("hang", bin, Options{Handler: noopHandler, Supervisor: sup})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// 预算给足以覆盖 stopGracePeriod，之后剩下的一律 Kill
	sup.StopAll(500 * time.Millisecond)

	select {
	case <-p.Exited():
	case <-time.After(10 * time.Second):
		t.Error("不响应 plugin.stop 的插件应被强制结束，否则 homed 关停会被它拖住")
	}
}

// 关停后完成握手的进程不得留存：立即被结束，不能活过内核。
func TestSupervisor_TrackAfterCloseKillsProcess(t *testing.T) {
	bin := buildTestPlugin(t, "echoplugin.go")
	sup := NewSupervisor()
	sup.StopAll(time.Second) // 置 closed

	p, err := Spawn("late", bin, Options{Handler: noopHandler, Supervisor: sup})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sup.Count() != 0 {
		t.Errorf("关停后不应再纳管新进程，实际在册 %d", sup.Count())
	}
	select {
	case <-p.Exited():
	case <-time.After(3 * time.Second):
		t.Error("关停后冒出的进程应被立即结束")
	}
}

// ★★★ 判据：**非法帧的报错必须可定位**（含内容摘要与修法）。
//
// ## 为什么必须有这条
//
// 原报错只有「收到非法 JSON 帧（N 字节）: invalid character 'x'」——
//
//	· **不显示内容** ⇒ 看不到是哪句话混进来了；
//	· **不提原因** ⇒ 看起来像协议/内核故障，不像"插件自己 printf"。
//
// 实测代价：生产环境累计 **1038 条**这种报错（weather 113 / rss 103 /
// calendar 102 / 第三方 huawei_smarthome 720），定位真因花了一整轮排查。
//
// ⇒ 本判据守两件事：
//
//	① `summarizeFrame` 真的能把坏帧压成可读单行（含控制字符处理与截断）；
//	② readLoop 的报错**文本里含**原因与修法（否则等于没改）。
func TestSummarizeFrameIsReadable(t *testing.T) {
	cases := []struct {
		in       string
		wantPart string
	}{
		{"[weather] started\n", "[weather] started"},
		{"line1\r\nline2\n", "line1  line2"},
		{"\x00\x01binary", "binary"},
		{"  padded  ", "padded"},
	}
	for _, c := range cases {
		got := summarizeFrame([]byte(c.in), 160)
		if !strings.Contains(got, c.wantPart) {
			t.Errorf("summarizeFrame(%q) = %q，期望含 %q", c.in, got, c.wantPart)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Errorf("摘要不得含换行（会打散日志）: %q", got)
		}
	}
	// 截断：长输入必须标注原长
	long := strings.Repeat("x", 500)
	got := summarizeFrame([]byte(long), 160)
	if len(got) > 260 {
		t.Errorf("超长输入未被截断: %d 字节", len(got))
	}
	if !strings.Contains(got, "500") {
		t.Errorf("截断时必须标注原始字节数: %q", got)
	}
}

// ★★★ 判据：readLoop 的非法帧报错**必须点明原因与修法**。
//
// ★ 源码扫描在此**恰当**：断言的是"报错文案里是否含某关键词"，
//
//	属于"模式存在性"，不是接线/顺序断言。
func TestIllegalFrameErrorIsActionable(t *testing.T) {
	src, err := os.ReadFile("process.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)
	// 必须提到"stdout 是协议通道"这个原因
	for _, kw := range []string{"协议通道", "log.Printf"} {
		if !strings.Contains(code, kw) {
			t.Errorf("★ 非法帧报错未提及 %q —— 插件作者看不到"+
				"「这是我自己 printf 造成的」这个关键信息", kw)
		}
	}
	// 必须真的调用 summarizeFrame（否则摘要不会出现在日志里）
	if !strings.Contains(code, "summarizeFrame(line") {
		t.Error("★ readLoop 必须调用 summarizeFrame 把坏帧内容打进日志" +
			"（否则只看到 invalid character 'x'，不知道是哪句话）")
	}
}
