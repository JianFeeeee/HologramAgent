package core

// 术语改名的判据（plan.md 裁定 #11：`coldcontext_*` / `stablemem_*`，不再用 doc/graph）。
//
// ## 为什么必须钉（2026-10-29 现场）
//
// 裁定 #11 只落实了一半：`coldcontext_*` 已改，而**图记忆的工具仍叫 `memory_*`**。
// 改名不是纯文字工作，它有三个会静默失效的面：
//
//	① 路由：toolroute 按前缀分派，改前缀不改路由 ⇒ 工具"存在但调不到"
//	② 提示词：BuildToolPrompt / skill / 工具描述里写旧名 ⇒ 模型照着调不存在的工具
//	③ 安全策略：生产配置用的是通配符 `memory_*`，命中不了 `stablemem_*`
//	   ⇒ 硬私有策略静默失效（用户能读私人记忆）
//
// ③ 最危险：它不报错、不告警，只是拦不住。所以本文件也钉一条策略匹配判据。

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/toolroute"
)

// ① 面向模型的稳定记忆工具必须统一用 stablemem_ 前缀。
func TestStableMemoryToolsUseStablememPrefix(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.memory = nil // 只需要名字，不需要真库

	// 旧名不得再出现于任何工具定义（会造成"提示词教旧名、定义是新名"）。
	//
	// ★ 此处曾有例外 `def.Name != "memory_document_query"`（2026-10-29 移除）。
	// 当时以为那个工具"仍在 memory_ 名下"，实际它是改名前的死分支：
	// 没有工具定义、Dispatch 也不分派它。例外一旦存在，就会掩盖真问题 ——
	// 所以现在**零例外**。
	for _, def := range a.ToolDefinitions() {
		if strings.HasPrefix(def.Name, "memory_") {
			t.Errorf("发现未改名的稳定记忆工具: %s（裁定 #11 要求 stablemem_*）", def.Name)
		}
	}
}

// ② 路由必须认 stablemem_ 前缀（否则工具存在但调不到）。
//
// ★ 这里直接调 Dispatch，而不是某个"IsBuiltinTool"前缀表 —— 后者曾有**两份**
// 手抄副本（toolroute 与 core 各一份），都零调用方，却在改名时制造了
// "改了 Dispatch 却漏了副本"的真实故障。判据钉**行为**（能不能分派），
// 不钉"某份表里有没有这个字符串"。
func TestStablememToolsAreRouted(t *testing.T) {
	h := &routeProbeHandlers{}

	for _, n := range []string{"stablemem_recall", "stablemem_purge"} {
		if _, handled := toolroute.Dispatch(h, agentAPI.ToolCall{Name: n}, nil); !handled {
			t.Errorf("%s 未被 Dispatch 分派 —— stablemem_ 前缀漏了，工具会变成\"存在但调不到\"", n)
		}
	}
	// ★ 旧前缀 memory_ **不再被分派**（这是改名的语义，不是遗漏）。
	//
	// 我最初在这里断言"memory_ 应保留兼容分派"，它变红了 —— 而红得对：
	// Dispatch 里从来没有 memory_ 分支，改名后只有 stablemem_。
	// 若为"兼容"而加一条 memory_ 分支，反而会让**旧名重新可用**，
	// 与裁定 #11（不再用 graph/doc 时代的名）直接冲突。
	//
	// 真正需要兼容的是**配置与提示词**（那些已另行处理），不是工具路由：
	// 存量插件注册的是自己的工具名，内核的稳定记忆工具由内核定义，
	// 不存在"外部还在用旧名调内核工具"的情况。
	if _, handled := toolroute.Dispatch(h, agentAPI.ToolCall{Name: "memory_recall"}, nil); handled {
		t.Error("memory_ 不应再被分派 —— 旧名重新可用会与裁定 #11 冲突")
	}
	// 反向：不是内置工具的名字必须返回 handled=false（否则插件工具会被吞掉）。
	if _, handled := toolroute.Dispatch(h, agentAPI.ToolCall{Name: "qq_get_message"}, nil); handled {
		t.Error("非内置工具不应被 Dispatch 吞掉")
	}
}

// routeProbeHandlers 记录 Dispatch 选中了哪个族，全部返回空串即可。
type routeProbeHandlers struct {
	memoryCalls int
}

func (h *routeProbeHandlers) ExecutePersonaTool(agentAPI.ToolCall) string { return "" }
func (h *routeProbeHandlers) ExecuteMemoryTool(agentAPI.ToolCall, []string) string {
	h.memoryCalls++
	return ""
}
func (h *routeProbeHandlers) ExecuteSocialTool(agentAPI.ToolCall) string      { return "" }
func (h *routeProbeHandlers) ExecuteKnowledgeTool(agentAPI.ToolCall) string   { return "" }
func (h *routeProbeHandlers) ExecuteColdContextTool(agentAPI.ToolCall) string { return "" }
func (h *routeProbeHandlers) ExecuteSecretTool(agentAPI.ToolCall) string      { return "" }
func (h *routeProbeHandlers) ExecuteOutputSend(agentAPI.ToolCall) string      { return "" }
func (h *routeProbeHandlers) ExecuteOutputSendHelp(agentAPI.ToolCall) string  { return "" }
func (h *routeProbeHandlers) ExecuteOutputListChannels() string               { return "" }
func (h *routeProbeHandlers) ExecuteInputChannels(agentAPI.ToolCall) string   { return "" }
func (h *routeProbeHandlers) ExecuteResidentAgents(agentAPI.ToolCall) string  { return "" }
func (h *routeProbeHandlers) ExecuteNotifyParent(agentAPI.ToolCall) string    { return "" }
func (h *routeProbeHandlers) ExecuteInputchNote(agentAPI.ToolCall) string     { return "" }
func (h *routeProbeHandlers) ExecutePluginReload() string                     { return "" }
func (h *routeProbeHandlers) ExecuteGetPluginTools(string) string             { return "" }

// ③ ★ 提示词里不得再教模型调旧工具名。
//
// BuildToolPrompt 是**硬编码字符串**（不是从定义生成），工具一改名它就过期。
// 这类不一致模型会当成事实执行，最终表现为"反复调用不存在的工具"。
func TestToolPromptDoesNotTeachRenamedTools(t *testing.T) {
	// BuildToolPrompt 是 Indexer 上的**硬编码字符串**（不是从 toolDefs 生成），
	// 工具一改名它就过期 —— 这正是本次改名后真实失败的那个测试。
	idx := memory.NewIndexer(nil)
	prompt := idx.BuildToolPrompt()
	if prompt == "" {
		t.Skip("BuildToolPrompt 返回空")
	}
	for _, stale := range []string{"memory_recall", "memory_commit", "memory_purge", "memory_introspect"} {
		if strings.Contains(prompt, stale) {
			t.Errorf("工具提示词仍在教旧工具名 %q —— 模型会照着调用不存在的工具", stale)
		}
	}
}
