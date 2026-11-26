package test

// 「状态读取不写共享状态」的判据（2026-11-20 实测缺陷）。
//
// ## 事故
//
// `GetKernelStatus()` 是**高频读路径**（cli /status 15s 轮询、webui 面板、
// healthcheck）。里面顺带注册了两个段：
//
//	a.RegisterStatusSection(StatusSection{Key: "runtime", ...})
//	a.RegisterStatusSection(StatusSection{Key: "llm", ...})
//
// 后果（真二进制实测）：
//
//	· 每读一次就往共享段表写一次 —— 读路径改共享状态，竞争面
//	· 每读一次分配两个闭包
//	· 每次写触发一条告警日志：实测 15s 两条 ⇒ 一天 11520 行
//
// ## 判据
//
// 源码级：`GetKernelStatus` 的函数体里**不许**出现 `RegisterStatusSection`。
// 行为级做不到（要起内核 + 轮询 15s），而这条约束一旦回退，症状是
// "日志被刷满 + 偶发竞争"，都是最难查的那类。

import (
	"encoding/json"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// TestGetKernelStatusDoesNotRegisterSections：读状态**没有注册副作用**。
//
// ★ 黑盒（2026-11-25 改写）：反复调 `GetKernelStatus()`，观测 `Sections`
//
//	的 key 集合是否**稳定**。不读 `kernelstatus.go` 的源码文本。
//
// ## 原判据为什么不合格
//
// 它 `os.ReadFile("../kernelstatus.go")`，正则抓 `GetKernelStatus` 的函数体，
// 再 `strings.Contains` 找 `RegisterStatusSection`。那是在测
// **"函数体里有没有这个词"** —— 换个写法（辅助函数、条件分支外提）就绕过，
// 而真正要防的东西是**可观测的**：读两次，段表会不会变。
//
// ## 事故本身
//
// 此前 `GetKernelStatus` 里有两行 `RegisterStatusSection`：
//
//	每读一次 ⇒ 写共享段表 + 分配闭包 + 刷一条"重复注册"告警日志
//	实测 15 秒刷出两条 → `/status` 轮询把日志刷满
//
// 判据因此是：**读不写**。连读 N 次，key 集合与第一次完全一致。
func TestGetKernelStatusDoesNotRegisterSections(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "t-status-purity"})

	first := keysOf(a.GetKernelStatus().Sections)
	for i := 0; i < 5; i++ {
		again := keysOf(a.GetKernelStatus().Sections)
		if !sameKeys(first, again) {
			t.Fatalf("★ 第 %d 次读状态时段表变了 —— 读路径有注册副作用\n"+
				"  第一次: %v\n  这一次: %v", i+1, first, again)
		}
	}
}

// TestBaseSectionsRegisteredOnce：基座三段在**装配期**注册好（读之前就在）。
//
// ★ 黑盒：读一次状态，`Sections` 里必须已经有 security / runtime / llm。
//
//	不读 `agent.go` 找 `StatusSection{Key: "security"` 这个字面量。
//
// ★ 与上一条配对：上一条说"读不新增"，这一条说"读之前已经有了"。
//
//	只测后者会漏掉"每次读都重注册"，只测前者会漏掉"根本没注册"。
func TestBaseSectionsRegisteredOnce(t *testing.T) {
	a := agent.New(agent.AgentConfig{ID: "t-status-sections"})
	// 段是**装配期**注册的（Start），不是读的时候。
	a.Start()
	defer a.Stop()
	got := keysOf(a.GetKernelStatus().Sections)
	for _, key := range []string{"security", "runtime", "llm"} {
		if !got[key] {
			t.Fatalf("★ 基座段 %q 没有在装配期注册（现有段: %v）", key, got)
		}
	}
}

// keysOf 把段表拍成集合。
func keysOf(m map[string]json.RawMessage) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sameKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}
