package core

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

func (a *Agent) executePluginReload() string {
	if a.pluginReg == nil {
		return "插件系统未启用"
	}
	msg, err := a.pluginReg.Reload(a.pluginDir)
	if err != nil {
		return fmt.Sprintf("插件重载失败: %v", err)
	}
	return msg
}

func (a *Agent) autoReloadPlugins() {
	if a.pluginReg == nil {
		return
	}
	for _, name := range a.pluginHealth.pendingReloads() {
		if !a.pluginReg.AutoRestartEnabled(name) {
			log.Printf("[agent] skip auto-reload plugin %s: auto-restart disabled by plugin", name)
			continue
		}
		log.Printf("[agent] auto-reloading unhealthy plugin: %s", name)
		if a.stageHost != nil {
			a.stageHost.UnregisterPluginTools(name)
		}
		if err := a.pluginReg.ReloadOne(name); err != nil {
			log.Printf("[agent] auto-reload plugin %s failed: %v", name, err)
		} else {
			a.pluginHealth.markReloaded(name)
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
	// 记忆子系统（内核能力，不是插件）
	"memory_recall": "core", "memory_commit": "core", "memory_merge": "core",
	"memory_purge": "core", "memory_edit": "core", "memory_delete_entity": "core",
	"memory_block_merge": "core", "memory_introspect": "core",
	"memory_document_query": "core", "memory_document_commit": "core",
	// 知识库
	"knowledge_search": "core", "knowledge_create": "core", "knowledge_list": "core",
	"knowledge_delete": "core", "knowledge_import_dir": "core",
	// 文档记忆
	"doc_query": "core", "doc_commit": "core",
	// 社交 / 人格
	"person_query": "core", "person_set_trait": "core", "person_relate": "core",
	"persona_set": "core",
	// 通道 / 调度 / 驻留子
	"output_send__": "core", "output_list_channels": "core",
	"input_channels": "core", "inputch_note": "core",
	"spawn_child": "core", "child_result": "core", "notify_parent": "core",
	"resident_agents": "core",
	// LLM 源 / 插件管理
	"llm_list_sources": "core", "llm_set_source": "core", "llm_current_source": "core",
	"plgreload": "core", "get_plugin_tools": "core", "list_devices": "core",
	"devicedetect": "core", "device_ctl_status": "core", "device_ctl_cmdrun": "core",
	// 安全模块的变量库（内核能力，不是插件）
	"secret_submit": "core", "secret_list": "core", "secret_forget": "core",
	"device_ctl_cmdresult": "core", "screensee": "core",
}

func (a *Agent) resolveToolPlugin(name string) string {
	if a.stageHost != nil {
		if plugin := a.stageHost.ToolPlugin(name); plugin != "" {
			return plugin
		}
	}
	// ★ 显式归属表优先于「按下划线猜」。
	//   未登记的内置工具归 core，而不是编出一个可能不存在的插件名。
	if owner, ok := builtinToolOwner[name]; ok {
		return owner
	}
	// 前缀规则只对「确实是 <plugin>_<tool>」形态的名字有意义
	// （例如外部插件的 email_email_list）。但它对内核自带的名字会造出
	// 假插件，所以限制在「该前缀是一个**已注册**的插件」时才采纳。
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		cand := name[:idx]
		if a.isRegisteredPlugin(cand) {
			return cand
		}
	}
	return "core"
}

// isRegisteredPlugin 判断名字是否是一个真实存在的插件。
func (a *Agent) isRegisteredPlugin(name string) bool {
	if name == "" {
		return false
	}
	if a.pluginReg != nil {
		for _, n := range a.pluginReg.List() {
			if n == name {
				return true
			}
		}
	}
	if a.stageHost != nil {
		for _, d := range a.stageHost.GetToolDefs() {
			p := d.Plugin
			if p == "" {
				p = a.resolveToolPlugin(d.Name)
			}
			if p == name {
				return true
			}
		}
	}
	return false
}

// executeGetPluginTools 返回指定插件的完整工具定义(名称/参数/用途)。
// 支持按插件名拉取, 未指定时返回全部插件的工具摘要。
func (a *Agent) executeGetPluginTools(pluginName string) string {
	type toolItem struct {
		name, desc string
		params     interface{}
	}
	var items []toolItem

	// 收集 StageHost(SDK 插件)工具
	if a.stageHost != nil {
		for _, d := range a.stageHost.GetToolDefs() {
			plg := d.Plugin
			if plg == "" {
				plg = a.resolveToolPlugin(d.Name)
			}
			if pluginName != "" && plg != pluginName {
				continue
			}
			items = append(items, toolItem{d.Name, d.Description, d.Parameters})
		}
	}
	// 收集 IOManager(设备/通道)工具
	if a.io != nil {
		for _, d := range a.io.GetAllTools() {
			plg := a.resolveToolPlugin(d.Name)
			if pluginName != "" && plg != pluginName {
				continue
			}
			items = append(items, toolItem{d.Name, d.Description, d.Parameters})
		}
	}
	// ★★ 收集**内置工具**（2026-05-05）。
	//
	// 原实现只收 StageHost(SDK 插件) 与 IOManager(设备) 两处，
	// 而 describe_image / ocr_image / memory_recall / knowledge_* 等
	// 都是内核 buildToolDefs 里直接 append 的 ⇒ **一条都数不到**。
	// 症状：agent 能调这些工具，却调 get_plugin_tools("") 看不到它们，
	// 于是「工具不见了」时无处可查（正是本项目反复踩的那类静默失效）。
	//
	// 以 buildToolDefs() 为**单一真相源** —— 它就是发给模型的那份工具表，
	// 这里不再维护第二份清单（第二份必然会漂移）。
	seen := map[string]bool{}
	for _, it := range items {
		seen[it.name] = true
	}
	for _, raw := range a.buildToolDefs() {
		fn := extractToolSchema(raw)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" || seen[name] {
			continue
		}
		// 归属：设备/通道工具已在上面收过；剩下的按显式表（或 core）算。
		plg := a.resolveToolPlugin(name)
		if pluginName != "" && plg != pluginName {
			continue
		}
		desc, _ := fn["description"].(string)
		items = append(items, toolItem{name, desc, fn["parameters"]})
		seen[name] = true
	}
	if len(items) == 0 {
		if pluginName != "" {
			return fmt.Sprintf("插件 %s 没有可用的工具定义（它可能未加载/已禁用/已崩溃；用 get_plugin_tools(\"\") 看全量）", pluginName)
		}
		return "当前没有可用的工具定义"
	}
	var sb strings.Builder
	if pluginName == "" {
		sb.WriteString(fmt.Sprintf("共 %d 个工具:\n", len(items)))
	} else {
		sb.WriteString(fmt.Sprintf("插件 %s 共 %d 个工具:\n", pluginName, len(items)))
	}
	for _, it := range items {
		sb.WriteString(fmt.Sprintf("\n### %s\n", it.name))
		if it.desc != "" {
			sb.WriteString(it.desc + "\n")
		}
		if it.params != nil {
			if b, err := json.Marshal(it.params); err == nil && len(b) < 600 {
				sb.WriteString("参数: " + string(b) + "\n")
			}
		}
	}
	return sb.String()
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
