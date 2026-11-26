package mainagent

import (
	"log"
	"strings"
)

// ★ `plgreload` 的执行体已删：它读的是内核插件注册表（`AgentConfig.PluginReg`），
//   归基座（`Agent.ExecutePluginReload`）。留在这里的后果是 rt/worker 拿不到。

func (m *Module) autoReloadPlugins() {
	if m.pluginRegNow() == nil {
		return
	}
	for _, name := range m.st.pluginHealth.PendingReloads() {
		if !m.pluginRegNow().AutoRestartEnabled(name) {
			log.Printf("[agent] skip auto-reload plugin %s: auto-restart disabled by plugin", name)
			continue
		}
		log.Printf("[agent] auto-reloading unhealthy plugin: %s", name)
		if m.stageHostNow() != nil {
			m.toolRegNow().UnregisterPluginTools(name)
		}
		if err := m.pluginRegNow().ReloadOne(name); err != nil {
			log.Printf("[agent] auto-reload plugin %s failed: %v", name, err)
		} else {
			m.st.pluginHealth.MarkReloaded(name)
			log.Printf("[agent] plugin %s reloaded successfully", name)
		}
	}
}

// builtinToolOwner 是**内置工具的显式归属表**。
//
// ★ 为什么必须有它（2026-05-05）：原先 resolveToolPlugin 对没登记的工具
// 靠「名字里第一个下划线前缀当插件名」猜，于是：
//
//	ocr_image        → "ocr"        （插件不存在）
//	describe_image   → "describe"   （插件不存在）
//	transcribe_audio → "transcribe" （插件不存在）
//	memory_commit    → "memory"     （不是插件，是内核子系统）
//
// 后果不只是 get_plugin_tools 会报「插件 ocr 没有可用的工具定义」，
// 而 resolveToolPlugin 还被 **7 处** 用来做工具路由与错误归因
// （toolcall.go:42 / task.go:710,892,939 / tooldefs.go:266），
// 猜错会把归属报给插件健康与监控，指向不存在的名字。
//
// 形态：**显式列举**而不是前缀规则。像 memory_* / doc_* / person_* 这些
// 根本不是插件，靠规则永远猜不对。
var builtinToolOwner = map[string]string{
	// ★ 这张表**只剩内核原语**：它们没有注册者（不是 registry.Tool），
	//   也没有能猜对的插件前缀（名字里第一个下划线后面不是插件名）。
	//
	// 已经**删掉**的项（现在由 registry.Tool.Owner 声明）：
	//
	//	admin_child 族     → mainagent/tools_register.go 的 DeclareOwnership
	//	stablemem_* 等私有族 → mainagent/tools_private.go
	//	llm_* / secret_* / plgreload / get_plugin_tools / output_*
	//	                   → 基座（kernel_tooldefs.go / iotooldefs.go）
	//
	// 判据：本表里出现一个"能被 registry 查到归属"的名字 ⇒ 红。
	"toolresult_read": "core",
}

func (m *Module) ResolveToolPlugin(name string) string {
	if m.stageHostNow() != nil {
		if plugin := m.toolRegNow().ToolPlugin(name); plugin != "" {
			return plugin
		}
	}
	// ★ 注册表优先：谁登记了这个工具，谁就是它的归属。
	//   这样加工具时**不必回来改这张表**（原先的形态是四处手抄，漏一处
	//   就会出现"工具能用但归属查不到"）。
	if owner := toolPluginOf(name); owner != "" {
		return owner
	}
	// 显式归属表优先于「按下划线猜」。
	// 未登记的内置工具归 core，而不是编出一个可能不存在的插件名。
	if owner, ok := builtinToolOwner[name]; ok {
		return owner
	}
	// 前缀规则只对「确实是 <plugin>_<tool>」形态的名字有意义
	// （例如外部插件的 email_email_list）。但它对内核自带的名字会造出
	// 假插件，所以限制在「该前缀是一个**已注册**的插件」时才采纳。
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		cand := name[:idx]
		if m.isRegisteredPlugin(cand) {
			return cand
		}
	}
	return "core"
}

// isRegisteredPlugin 判断名字是否是一个真实存在的插件。
func (m *Module) isRegisteredPlugin(name string) bool {
	if name == "" {
		return false
	}
	if m.pluginRegNow() != nil {
		for _, n := range m.pluginRegNow().List() {
			if n == name {
				return true
			}
		}
	}
	if m.stageHostNow() != nil {
		for _, d := range m.toolRegNow().GetToolDefs() {
			p := d.Plugin
			if p == "" {
				p = m.ResolveToolPlugin(d.Name)
			}
			if p == name {
				return true
			}
		}
	}
	return false
}

// extractToolSchema 从 buildToolDefs 的一项里取出 function 体。
// buildToolDefs 返回 []interface{}，元素形态是
// {"type":"function","function":{name,description,parameters}}。
func extractToolSchema(raw interface{}) map[string]interface{} {
	m, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	fn, ok := m["function"].(map[string]interface{})
	if !ok {
		return nil
	}
	return fn
}
