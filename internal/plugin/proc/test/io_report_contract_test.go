package test

// internal/plugin/proc 的**注入失败可见性**判据 —— 面向接口的黑盒。
//
// ## 被测的职责
//
// 子进程插件的 `IOInjector` 方法由公开 SDK 固定签名（`InjectText(source,
// channel, text)` 无返回值），因此注入失败**不能**靠返回值上报。但
// 「被权限闸门拒了」与「对方没回复」对插件作者是完全不同的两件事：
//
//	InjectInputSync 返回 ""  —— 可能是没人回，也可能是这次调用根本没被允许执行
//	InjectText               —— 什么都没发生，作者只会觉得"我那段逻辑没生效"
//
// §3.8 第三道闸要求「被拒时返回明确错误而不是静默」。无返回值的方法
// 无法返回错误，所以契约落到**stderr**：模板运行时必须把失败大声报出来，
// 而内核把子进程 stderr 直通到自己的日志（`process.go: cmd.Stderr = os.Stderr`），
// 因此运维在 homed 日志里能看到、插件作者本地也能看到。
//
// ## 这个判据的来历（真实缺陷）
//
// 2026-11-15 由同一目录下的能力阶梯判据实测发现：manifest 只声明 `memory`
// 的插件调 `InjectText` 拿到"成功"、调 `InjectInputSync` 拿到空字符串。
// 根因是模板里的 `_, _ = callWithTextOpts(...)` 与 `if err != nil { return "" }`
// —— 拒绝错误被**完全丢弃**。
//
// 本判据从**进程外**观测：换掉 os.Stderr 成管道 → 编译真实模板插件 → 让它
// 在被拒的能力面上做一次注入 → 断言子进程 stderr 里出现了可操作的告警。
// 不读模板源码，也不依赖任何私有符号。

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// injectionProbePluginSource 生成一个**只做注入**的探针插件。
//
// 两个入口分别覆盖两条不同的失败路径：
//
//	probe_inject_void   → InjectText（无返回值，只能靠 stderr）
//	probe_inject_sync   → InjectInputSync（有返回值，但失败也是空串）
func injectionProbePluginSource() string {
	return `package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type injector struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &injector{name: name}, nil
}

func (p *injector) Name() string { return p.name }
func (p *injector) Stop() error  { return nil }

func (p *injector) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("probe_inject_void", sdk.ToolDef{
		Description: "调用 InjectText（无返回值）",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		s.InjectText("probe-src", "probe-chan", "探针注入")
		return "OK", nil
	})

	s.RegisterTool("probe_inject_sync", sdk.ToolDef{
		Description: "调用 InjectInputSync（失败返回空串）",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		reply := s.InjectInputSync("probe-src", "probe-chan", "探针同步注入")
		return "ok:" + reply, nil
	})
	return nil
}
`
}

// stderrCapture 把 os.Stderr 换成管道并在后台持续收集。
//
// 为什么必须换**进程级** os.Stderr 而不是给 Spawn 传一个 writer：
// `proc.Spawn` 内部写死 `cmd.Stderr = os.Stderr`（那是"插件日志直通内核日志"
// 的实现方式），没有可注入的出口。这是被观测子系统的一部分，不改它。
type stderrCapture struct {
	orig   *os.File
	w      *os.File
	done   chan struct{}
	mu     sync.Mutex
	buf    strings.Builder
	closed bool
}

func captureStderr(t *testing.T) *stderrCapture {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("创建 stderr 管道: %v", err)
	}
	c := &stderrCapture{orig: os.Stderr, w: w, done: make(chan struct{})}
	os.Stderr = w
	// ★ 必须**流式**读进 buffer，不能用 io.ReadAll 收集后再写。
	//
	// io.ReadAll 只在 EOF（写端关闭）后才返回 —— 而判据是在子进程还活着、
	// 写端还开着的**那一刻**去查 stderr 的。用 ReadAll 的话，waitFor 永远
	// 读到空 buffer，判据会稳定地假红（我第一版正是这样，捕获始终 ""）。
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.buf.Write(buf[:n])
				c.mu.Unlock()
			}
			if err != nil {
				break
			}
		}
		_ = r.Close()
		close(c.done)
	}()
	t.Cleanup(func() { c.restore() })
	return c
}

func (c *stderrCapture) restore() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	os.Stderr = c.orig
	_ = c.w.Close()
	<-c.done
}

// text 返回目前收集到的 stderr 全文。
func (c *stderrCapture) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// waitFor 在预算内等待 stderr 出现包含 needle 的内容。
func (c *stderrCapture) waitFor(t *testing.T, needle string, budget time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(budget)
	for {
		got := c.text()
		if strings.Contains(got, needle) {
			return got
		}
		if !time.Now().Before(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ---- 判据 1：被拒的无返回值注入必须在 stderr 留下可操作的告警 ----

func TestDeniedVoidInjectionIsReportedOnStderr(t *testing.T) {
	caps := captureStderr(t)
	results := runInjectionProbes(t, []string{"memory"}, caps)

	if _, ok := results["probe_inject_void"]; !ok {
		t.Fatal("插件未注册 probe_inject_void")
	}

	got := caps.waitFor(t, "[plugin-io]", 3*time.Second)
	if !strings.Contains(got, "[plugin-io]") {
		t.Fatalf("★ 无返回值的注入被拒后**没有任何声音**（stderr 里找不到 [plugin-io]）。\n"+
			"  作者只会看到自己的逻辑「没生效」，永远查不到是权限问题。\n"+
			"  捕获到的 stderr: %q", truncate(got, 500))
	}
	// 告警必须点名**哪个方法**、**哪个通道**，以及**怎么修**。
	for _, want := range []string{"io.injectText", "probe-chan", "capabilities"} {
		if !strings.Contains(got, want) {
			t.Fatalf("★ 注入失败告警缺少 %q（作者无法自助定位/修复）：%q",
				want, truncate(got, 500))
		}
	}
}

// ---- 判据 2：同步注入的空回复不能掩盖「被拒」 ----

func TestDeniedSyncInjectionIsReportedOnStderr(t *testing.T) {
	caps := captureStderr(t)
	results := runInjectionProbes(t, []string{"memory"}, caps)

	out, ok := results["probe_inject_sync"]
	if !ok {
		t.Fatal("插件未注册 probe_inject_sync")
	}
	// 返回值本身仍是空串 —— 那是公开 SDK 固定的语义，不能改。
	if !strings.HasPrefix(out, "ok:") || strings.TrimPrefix(out, "ok:") != "" {
		t.Fatalf("前置：被拒时同步注入应回空串（公开 SDK 语义），实际 %q", out)
	}

	got := caps.waitFor(t, "io.injectInputSync", 3*time.Second)
	if !strings.Contains(got, "io.injectInputSync") {
		t.Fatalf("★ 同步注入被拒后 stderr 没有点名 io.injectInputSync。\n"+
			"  空串与「对方没回复」无法区分 ⇒ 作者会去查通道实现而不是查权限。\n"+
			"  捕获到的 stderr: %q", truncate(got, 500))
	}
}

// ---- 判据 3：声明了 io 之后不该再刷告警 ----

func TestSuccessfulInjectionIsSilent(t *testing.T) {
	caps := captureStderr(t)
	results := runInjectionProbes(t, []string{"io"}, caps)

	for _, name := range []string{"probe_inject_void", "probe_inject_sync"} {
		if _, ok := results[name]; !ok {
			t.Fatalf("插件未注册 %s", name)
		}
	}

	// 给告警一点时间冒出来（若实现是"无论如何都报"就会被抓到）。
	time.Sleep(300 * time.Millisecond)
	if got := caps.text(); strings.Contains(got, "[plugin-io]") {
		t.Fatalf("★ 权限正常时仍打告警 —— 告警会变成背景噪声，真正的失败就看不见了：%q",
			truncate(got, 500))
	}
}

// ---- 测试支撑 ----

// runInjectionProbes 编译并运行注入探针插件，返回「工具名 → 返回串」。
//
// caps 是 manifest 声明的能力集；stderr 由调用方自己的 capture 接管，
// 这里只用它来让失败路径真的走一遍。
func runInjectionProbes(t *testing.T, declared []string, _ *stderrCapture) map[string]string {
	t.Helper()
	host := newExpandHost(t)
	core := newCapableCore()
	bin := buildTemplatePlugin(t, injectionProbePluginSource())

	p := startNamedPlugin(t, host, core, "inject-probe", bin, declared)
	t.Cleanup(func() { _ = p.Close() })

	results := map[string]string{}
	for _, name := range []string{"probe_inject_void", "probe_inject_sync"} {
		h, ok := core.recordingCore.tool(name)
		if !ok {
			continue
		}
		res, err := h(map[string]interface{}{})
		if err != nil {
			t.Fatalf("%s: 调用失败: %v", name, err)
		}
		s, _ := res.(string)
		results[name] = s
	}
	return results
}
