package agent

// kernel_tools.go —— **内核设施类工具**的执行体（不属于任何 agent 类型）。
//
// ## 归属判据（用户裁定 2026-11-19）
//
// 「这个工具读的是不是某个类型的私有状态？」
//
//	llm_*        读 kernelapi.LLMClient + ProviderManager（AgentConfig 公有字段）→ **基座**
//	secret_*     读 SecurityGate.Store()（内核全局变量库）                → **基座**
//	plgreload    读插件注册表（AgentConfig.PluginReg）                    → **基座**
//	get_plugin_tools 同上                                                → **基座**
//	output_*     读基座 io（agent/output.go 已有）                        → **基座**
//
// 它们**从来不属于主 agent**：任何 agent 都能问"我当前用哪个模型源"、
// 都能登记凭据、都能重载插件。把它们留在 `mainagent` 的后果是
// rt / worker 拿不到（它们没有那些私有状态），于是只能各写一份重复实现
// （`worker/worker.go` 里就有一份 `ExecuteLLMTool`）。
//
// ★ 这里只放**执行体**，登记与路由在内核执行设施（`kernelmod/toolexec`）。

import (
	"fmt"
	"sort"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ExecuteLLMTool 是 `llm_*` 工具的执行体。
//
// ★ 只读**基座的能力面**（`LLMSources` / `ActiveLLMSource` / `LLM().SetDefault`），
// 基座再把它们转给内核设施。调用方与"是主 agent 还是工作式子"无关。
func (a *Agent) ExecuteLLMTool(tc agentAPI.ToolCall) string {
	llm := a.llm
	if llm == nil {
		return "LLM 设施未装配"
	}
	switch tc.Name {
	case "llm_list_sources":
		sources := a.LLMSources()
		if len(sources) == 0 {
			return "没有可用的 LLM 源"
		}
		active := llm.ActiveSource(string(a.id))
		parts := []string{"可用 LLM 源:"}
		for _, si := range sources {
			mark := " "
			if si.Name == active {
				mark = "→"
			}
			note := ""
			if !si.Available {
				note = "（当前不可用）"
			}
			parts = append(parts, "  "+mark+" "+si.Name+note)
		}
		return strings.Join(parts, "\n")

	case "llm_current_source":
		if s := llm.ActiveSource(string(a.id)); s != "" {
			return "当前 LLM 源: " + s
		}
		return "当前 LLM 源: （未确定）"

	case "llm_set_source":
		name, _ := tc.Arguments["name"].(string)
		if strings.TrimSpace(name) == "" {
			return "请提供源名称"
		}
		if err := llm.SetDefault(name); err != nil {
			return fmt.Sprintf("切换失败: %v", err)
		}
		return fmt.Sprintf("已切换到 LLM 源: %s", name)
	}
	return fmt.Sprintf("未知的 LLM 工具: %s", tc.Name)
}

// ExecuteSecretTool 是 `secret_*` 工具的执行体。
//
// 读的是**内核全局变量库**（`SecurityGate.Store()`），不是某个 agent 的私有状态。
//
// ## 设计红线：永不返回明文
//
//	secret_submit  接受明文（唯一入口，且必须如此才能登记）
//	secret_list    只给元信息（别名、来源、命中次数）
//	secret_forget  按别名删除
//	**没有** secret_get —— 一旦能取回明文，模型就能把明文写回上下文，整层防护作废。
func (a *Agent) ExecuteSecretTool(tc agentAPI.ToolCall) string {
	s := a.SecretStore()
	if s == nil {
		return "安全模块未启用（core.security.enabled=false），变量库不可用"
	}
	switch tc.Name {
	case "secret_submit":
		value, _ := tc.Arguments["value"].(string)
		if strings.TrimSpace(value) == "" {
			return "value 不能为空"
		}
		note, _ := tc.Arguments["note"].(string)
		alias, err := s.Submit(value, note)
		if err != nil {
			return fmt.Sprintf("登记失败（未以明文降级存储）: %v", err)
		}
		return fmt.Sprintf("已登记为变量 @{%s}。后续调用请用 @{%s} 引用，不要直接写明文。", alias, alias)

	case "secret_list":
		infos := s.List()
		if len(infos) == 0 {
			return "变量库为空。用 secret_submit 登记凭据。"
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Alias < infos[j].Alias })
		var b strings.Builder
		fmt.Fprintf(&b, "共 %d 个变量（只显示元信息，明文不可读回）：\n", len(infos))
		for _, in := range infos {
			fmt.Fprintf(&b, "  @{%s}", in.Alias)
			if in.Note != "" {
				fmt.Fprintf(&b, "  %s", in.Note)
			}
			fmt.Fprintf(&b, "  [来源 %s, 命中 %d]\n", in.Source, in.Hits)
		}
		b.WriteString("\n在工具参数里用 @{别名} 引用即可（内核会换回真实值，插件无感知）。")
		return b.String()

	case "secret_forget":
		alias, _ := tc.Arguments["alias"].(string)
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return "请提供要删除的别名"
		}
		if err := s.Forget(alias); err != nil {
			return fmt.Sprintf("删除失败: %v", err)
		}
		return fmt.Sprintf("已删除变量 @{%s}", alias)
	}
	return "未知的 secret 工具: " + tc.Name
}

// ExecutePluginReload 是 `plgreload` 工具的执行体（读内核插件注册表）。
func (a *Agent) ExecutePluginReload() string {
	reg := a.pluginRegForStatus()
	if reg == nil {
		return "插件系统未启用"
	}
	msg, err := reg.Reload(a.pluginDirField())
	if err != nil {
		return fmt.Sprintf("插件重载失败: %v", err)
	}
	return msg
}

// ExecuteGetPluginTools 是 `get_plugin_tools` 工具的执行体。
//
// ## 三处来源，一份事实
//
//	① toolReg    插件工具（SDK 注册的）
//	② io         通道/设备工具
//	③ ToolDefs   本类型自己的工具（**经类型注册的只读面**，基座不自己拼）
//
// ★ 为什么 ③ 要经 `cfg.ToolDefs` 而不是调类型的 `BuildToolDefs`：
//
//	基座不认识任何类型的渲染函数（渲染归类型）。而 `ToolDefs` 面本来就是
//	类型为 `api.ToolHost` 契约注册的 —— 这里正好复用，不新增接线。
//
// ★ 原实现（在 mainagent 里）只收 ①②，漏掉了内核内置工具 ⇒
//
//	agent 能调 `stablemem_recall`，却调 `get_plugin_tools("")` 看不到它，
//	"工具不见了"时无处可查。现在三处合并，单一事实。
func (a *Agent) ExecuteGetPluginTools(pluginName string) string {
	type item struct{ name, plugin, desc string }
	var items []item
	seen := map[string]bool{}

	// ① 插件工具。
	if tr := a.ToolRegistry(); tr != nil {
		for _, td := range tr.GetToolDefs() {
			items = append(items, item{td.Name, td.Plugin, td.Description})
			seen[td.Name] = true
		}
	}
	// ② io / 设备工具。
	if ioM := a.IO(); ioM != nil {
		for _, td := range ioM.GetAllTools() {
			if seen[td.Name] {
				continue
			}
			items = append(items, item{td.Name, "", td.Description})
			seen[td.Name] = true
		}
	}
	// ③ 本类型的工具（只读面；未注册 ⇒ 该类型不暴露工具清单）。
	if a.toolDefsFace() != nil {
		for _, td := range a.toolDefsFace()() {
			if seen[td.Name] {
				continue
			}
			items = append(items, item{td.Name, "", td.Description})
			seen[td.Name] = true
		}
	}

	if len(items) == 0 {
		return "当前没有任何可用工具"
	}
	// 过滤：指定插件名时只列该插件的（插件归属未知的不算）。
	if pluginName != "" {
		kept := items[:0]
		for _, it := range items {
			if it.plugin == pluginName {
				kept = append(kept, it)
			}
		}
		items = kept
		if len(items) == 0 {
			return fmt.Sprintf("插件 %s 没有可用的工具定义（它可能未加载、已崩溃，或声明了 auto_start=false）", pluginName)
		}
	}

	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	var b strings.Builder
	if pluginName == "" {
		fmt.Fprintf(&b, "可用工具（%d 个；插件归属未知的归 core）:\n", len(items))
	} else {
		fmt.Fprintf(&b, "插件 %s 的工具（%d 个）:\n", pluginName, len(items))
	}
	for _, it := range items {
		b.WriteString("  · " + it.name)
		if it.desc != "" {
			// 只给一句话（完整 schema 走各自的 _help 或 skill）。
			b.WriteString(" —— " + firstLines(it.desc, 1, 100))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// firstLines 取前 n 行并截断到 maxRunes（说明文字常常是多行长文）。
func firstLines(s string, n, maxRunes int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	out := strings.Join(lines, " / ")
	return TruncateStr(out, maxRunes)
}

// pluginDirField 返回插件目录（plgreload 用）。
func (a *Agent) pluginDirField() string {
	if a == nil {
		return ""
	}
	a.semanticMu.RLock()
	defer a.semanticMu.RUnlock()
	return a.pluginDir
}
