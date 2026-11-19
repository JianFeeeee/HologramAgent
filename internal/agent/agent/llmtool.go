package agent

// llmtool.go —— `llm_*` 工具的**基座侧实现**（走内核的 LLM 抽象）。
//
// ## 归属：基座，不是某个 agent 类型
//
// 它们问的是**内核的 LLM 设施**（哪些源、当前用哪个），而基座本来就持有
// 那个抽象：
//
//	AgentConfig.LLM  kernelapi.LLMClient   ← Complete / Stream / ActiveSource
//
// 所以这些工具与"是主 agent 还是工作式子"无关 —— 任何 agent 都能问
// "我当前用哪个源"。改造前它们躺在 `spawn.go`（工作式子的文件）里，
// 只因为那里碰巧有 `a.providerManager` 字段，那不是归属。

import (
	"fmt"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ExecuteLLMTool 是 `llm_*` 工具的实现（基座层）。
//
// ★ 读的是**内核设施**（`LLMClient.ActiveSource`）与配置管理器，
// 不是某个类型的私有状态。
func (a *Agent) ExecuteLLMTool(tc agentAPI.ToolCall) string {
	switch tc.Name {
	case "llm_list_sources", "llm_set_source", "llm_current_source":
		return a.llmSourcesTool(tc)
	}
	return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
}

func (a *Agent) llmSourcesTool(tc agentAPI.ToolCall) string {
	pm := a.Config().ProviderManager
	if pm == nil && a.Config().LLM == nil {
		return "LLM 设施未装配"
	}
	switch tc.Name {
	case "llm_list_sources":
		if pm == nil {
			return "LLM 源管理器不可用"
		}
		sources := pm.List()
		if len(sources) == 0 {
			return "没有可用的 LLM 源"
		}
		// 活动源问**内核设施**（它是事实），不问本地字段。
		active := ""
		if a.Config().LLM != nil {
			active = a.Config().LLM.ActiveSource(string(a.ID()))
		}
		parts := []string{"可用 LLM 源:"}
		for _, name := range sources {
			mark := " "
			if name == active {
				mark = "→"
			}
			parts = append(parts, "  "+mark+" "+name)
		}
		return strings.Join(parts, "\n")
	case "llm_current_source":
		if a.Config().LLM == nil {
			return "LLM 设施未装配"
		}
		if s := a.Config().LLM.ActiveSource(string(a.ID())); s != "" {
			return "当前 LLM 源: " + s
		}
		return "当前 LLM 源: （未确定）"
	case "llm_set_source":
		name, _ := tc.Arguments["name"].(string)
		if name == "" {
			return "请提供源名称"
		}
		if pm == nil {
			return "LLM 源管理器不可用"
		}
		if err := pm.SetDefault(name); err != nil {
			return fmt.Sprintf("切换失败: %v", err)
		}
		return fmt.Sprintf("已切换到 LLM 源: %s", name)
	}
	return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
}
