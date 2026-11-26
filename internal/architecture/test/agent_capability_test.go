package test

// agent_capability_test.go —— 能力面（docs §8.1.1）的可机械检验判据。
//
// 用户裁定 2026-11-20：
// 「基座还要要求 agent 声明自己的能力面……输入设备与输出设备依照能力面
//   决定能否划入对应的 agent」。

import (
	"errors"
	"reflect"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"

	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/agent"
)

// TestBaseDoesNotKnowAnyConcreteCapability：基座不认识任何具体能力名。
//
// 能力名（rt/audio/…）只在 io 包与声明者自己的包里出现。基座若出现
// `CapRT`，加第三种能力时它就要改 —— 那正是本条要拦的形态。
func TestBaseDoesNotKnowAnyConcreteCapability(t *testing.T) {
	// ① `AgentConfig` 里不许有 `IsRealtime`（能力不能是单个布尔）。
	//
	// ★ 黑盒（2026-11-25 改写）：`reflect` 读字段集，不读源码文本。
	//   字段集是编译期事实 —— 谁把结构体挪到别的文件都不该影响这条判据。
	if _, ok := reflect.TypeOf(agent.AgentConfig{}).FieldByName("IsRealtime"); ok {
		t.Fatal("★ AgentConfig 里还有 IsRealtime 字段 —— 能力面必须是位掩码")
	}

	// ② 基座**对外的方法集**里不许出现具体能力名。
	//
	// 这里刻意只查方法集而不是源码文本：能力名出现在注释里不算违规
	// （本仓大量注释在解释"为什么不要 CapRT"），出现在**可调用面**上才算。
	typ := reflect.TypeOf(&agent.Agent{})
	for _, name := range []string{"CapRT", "CapAudio", "CapImage", "CapStructured"} {
		if _, ok := typ.MethodByName(name); ok {
			t.Fatalf("★ 基座方法集里出现了具体能力名 %s —— 基座不该认识它", name)
		}
	}
}

// TestAssignRejectsWhenEitherSideLacksCapability：两侧都要过。
func TestAssignRejectsWhenEitherSideLacksCapability(t *testing.T) {
	reg := agentIO.NewChannelRegistry()
	// 通道【没有】声明 CapRT。
	if err := reg.Register(agentIO.InputChannel{Name: "plain", Capabilities: agentIO.CapText}); err != nil {
		t.Fatalf("登记通道: %v", err)
	}
	// agent 声明了 CapRT。
	err := reg.AssignWithCapability("plain", "a1", 0, agentIO.CapRT, agentIO.CapText|agentIO.CapRT)
	if !errors.Is(err, agentIO.ErrChannelLacksCapability) {
		t.Fatalf("★ 通道缺能力位时应报 ErrChannelLacksCapability，实得: %v", err)
	}

	// 通道声明了 CapRT，但 agent 没有。
	reg2 := agentIO.NewChannelRegistry()
	if err := reg2.Register(agentIO.InputChannel{Name: "rtch", Capabilities: agentIO.CapText | agentIO.CapRT}); err != nil {
		t.Fatalf("登记通道: %v", err)
	}
	err = reg2.AssignWithCapability("rtch", "a2", 0, agentIO.CapRT, agentIO.CapText)
	if !errors.Is(err, agentIO.ErrAgentLacksCapability) {
		t.Fatalf("★ agent 缺能力位时应报 ErrAgentLacksCapability，实得: %v", err)
	}

	// 两侧都有 ⇒ 划入成功。
	if err := reg2.AssignWithCapability("rtch", "a3", 0, agentIO.CapRT, agentIO.CapText|agentIO.CapRT); err != nil {
		t.Fatalf("★ 两侧都有能力位时应划入成功，实得: %v", err)
	}
}

// TestCapabilityNameRoundTrip：中立能力名 ↔ 位 的映射是双向齐全的。
//
// 加第三种能力时这条判据自动覆盖它（表在 io 包里，只有一处）。
func TestCapabilityNameRoundTrip(t *testing.T) {
	for _, name := range []string{"text", "rt", "audio", "image", "file", "structured"} {
		bit, ok := agentIO.CapabilityByName(name)
		if !ok {
			t.Fatalf("★ 能力名 %q 没有映射（插件声明它会失败）", name)
		}
		if bit == 0 {
			t.Fatalf("★ 能力名 %q 映射到了零位", name)
		}
	}
	if _, ok := agentIO.CapabilityByName("rt; DROP TABLE"); ok {
		t.Fatal("★ 未知能力名被接受了 —— 拼错必须响亮拒绝")
	}
}

// TestBuiltinTypesDeclareTheirCapability：内置四型各自声明能力面。
func TestBuiltinTypesDeclareTheirCapability(t *testing.T) {
	// rt 必须声明 CapRT；其余三型必须**不**声明它。
	want := map[string]bool{"main": false, "resident": false, "rt": true, "worker": false}
	for ty, needsRT := range want {
		d, ok := registry.Lookup(ty)
		if !ok {
			t.Fatalf("类型 %q 未注册", ty)
		}
		if got := d.AgentCap.Supports(agentIO.CapRT); got != needsRT {
			t.Fatalf("★ 类型 %q 的 CapRT 声明为 %v，应为 %v（能力面 = %s）",
				ty, got, needsRT, d.AgentCap)
		}
		if !d.AgentCap.Supports(agentIO.CapText) {
			t.Fatalf("★ 类型 %q 没有基础文本能力", ty)
		}
	}
}
