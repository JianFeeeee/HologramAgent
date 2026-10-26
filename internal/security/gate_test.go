package security

// 安全边界（Gate / Runtime）的判据。
//
// 这些判据守的是**用户裁定的架构不变量**（2026-10-26）：
//
// > 安全模块就是横在其他所有模块与 core 之间的一堵墙：
// > core 发出的请求和外部返回的请求都会经过它审查，通过才能进行下一步。
//
// 所以每一条都在问同一个问题：**这条数据流有没有可能绕过审查**。
// 判据都必须能红（变异实现即失败），否则等于没写。

import (
	"strings"
	"testing"
)

// testStore 造一个已装好主密钥的变量库（不落盘到真实数据目录）。
func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := InitGlobal(Options{
		Path:    dir + "/vars.json",
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("InitGlobal: %v", err)
	}
	// 全局单例是进程级的：测试之间要复位，否则互相污染。
	t.Cleanup(func() { SetGlobal(nil) })
	return s
}

// ① 出向：明文必须被换成 @{别名}，不能原样发给外部。
func TestGateOutboundBlocksPlaintext(t *testing.T) {
	s := testStore(t)
	g := NewGate(s)

	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyz"
	out, n := g.PrepareOutbound(Inspection{Direction: DirectionOutbound, Stage: "tool_args", Tool: "cmd_run"},
		"curl -H 'Authorization: Bearer "+secret+"' https://api.example.com")

	if n == 0 {
		t.Fatal("出向没有拦截明文 —— 凭据会直接发给外部（这堵墙没起作用）")
	}
	if strings.Contains(out, secret) {
		t.Fatalf("出向结果仍含明文: %q", out)
	}
	if !strings.Contains(out, "@{sec_") {
		t.Fatalf("出向结果应含变量别名，实际: %q", out)
	}
}

// ② 入向：外部返回的明文同样必须被换掉。
//
// ★ 为什么入向是必须的：入向内容直接进模型上下文（role=tool）。
// 只在"写记忆"时清洗是最坏组合 —— 记忆侧干净、当轮已泄漏、事后查不到。
func TestGateInboundScrubsPlaintext(t *testing.T) {
	s := testStore(t)
	g := NewGate(s)

	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	out, n := g.ScrubInbound(Inspection{Direction: DirectionInbound, Stage: "tool_result", Tool: "cmd_run"},
		"server said: token="+secret)

	if n == 0 {
		t.Fatal("入向没有清洗明文 —— 明文会原样进入模型上下文")
	}
	if strings.Contains(out, secret) {
		t.Fatalf("入向结果仍含明文: %q", out)
	}
}

// ③ 执行前回填：外部必须拿到真实值（"插件无感知"的既有语义）。
func TestGateFillsAliasBeforeExecution(t *testing.T) {
	s := testStore(t)
	g := NewGate(s)

	const secret = "ghp_FILLME0123456789abcdefghijklmnopqrst"
	alias, err := s.Submit(secret, "test")
	if err != nil {
		t.Fatal(err)
	}

	out, n := g.FillForExecution(Inspection{Direction: DirectionOutbound, Stage: "plugin_call"},
		"token=@{"+alias+"}")
	if n == 0 {
		t.Fatal("回填没有发生 —— 插件会收到字面量 @{别名} 而不是真实值")
	}
	if !strings.Contains(out, secret) {
		t.Fatalf("回填结果不含真实值: %q", out)
	}
}

// ④ 未启用时零成本透传（且**不是**"装配失败"）。
//
// 判据同时钉住两件事：Disabled 不是 nil（调用点只有一条路径），
// 且它真的不改写内容（不能假装审查过）。
func TestDisabledRuntimePassesThrough(t *testing.T) {
	r := NewDisabledRuntime()
	if r == nil {
		t.Fatal("Disabled Runtime 必须是有效对象，不能让调用点各自判 nil")
	}
	if r.Enabled() {
		t.Fatal("Disabled Runtime 不应报告 enabled")
	}

	const text = "ghp_SHOULDNOTBECHANGED0123456789abcdefgh"
	if out, n := r.PrepareOutbound(Inspection{Stage: "x"}, text); n != 0 || out != text {
		t.Errorf("disabled 出向应原样返回: n=%d out=%q", n, out)
	}
	if out, n := r.ScrubInbound(Inspection{Stage: "x"}, text); n != 0 || out != text {
		t.Errorf("disabled 入向应原样返回: n=%d out=%q", n, out)
	}
	if out, n := r.FillForExecution(Inspection{Stage: "x"}, text); n != 0 || out != text {
		t.Errorf("disabled 回填应原样返回: n=%d out=%q", n, out)
	}
}

// ⑤ 启用后 Runtime 必须报告 enabled —— 状态面据此证明"审查真的在生效"，
// 而不是靠读配置猜（配置开了但装配失败是另一件事）。
func TestEnabledRuntimeReportsEnabled(t *testing.T) {
	s := testStore(t)
	r := NewRuntime(s)
	if !r.Enabled() {
		t.Fatal("装好变量库的 Runtime 必须报告 enabled")
	}
}

// ⑥ nil Runtime 不得 panic（装配期错误不该变成崩溃）。
func TestNilRuntimeIsSafe(t *testing.T) {
	var r *Runtime
	if out, n := r.PrepareOutbound(Inspection{}, "x"); n != 0 || out != "x" {
		t.Errorf("nil runtime 出向应透传")
	}
	if out, n := r.ScrubInbound(Inspection{}, "x"); n != 0 || out != "x" {
		t.Errorf("nil runtime 入向应透传")
	}
	if r.Enabled() {
		t.Error("nil runtime 不应报告 enabled")
	}
	if r.Store() != nil {
		t.Error("nil runtime 不应给出 Store")
	}
}
