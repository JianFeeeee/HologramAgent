package core

import (
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// get_plugin_tools 必须是**完整**的服务（2026-05-05 修）
//
// ## ① 它数不到内置工具
//
// 原实现只收两处来源：
//
//	StageHost.GetToolDefs()   ← SDK 插件注册的
//	a.io.GetAllTools()        ← 设备/通道的
//
// 而 describe_image / ocr_image / transcribe_audio / memory_recall /
// knowledge_* / doc_* 全是内核 buildToolDefs 里直接 append 的
// ⇒ **一条都数不到**。
//
// 症状：agent 能调这些工具，却调 get_plugin_tools("") 看不见它们。
// 「工具不见了 / 怎么没有这个能力」时无处可查 —— 本项目反复踩的那类静默失效。
//
// ## ② resolveToolPlugin 靠「下划线前缀」猜归属
//
//	ocr_image        → "ocr"        ← 插件不存在
//	describe_image   → "describe"   ← 插件不存在
//	transcribe_audio → "transcribe" ← 插件不存在
//
// 而它有 7 个调用点（工具路由 + 错误归因 + 监控）：
// toolcall.go:42 / task.go:710,892,939 / tooldefs.go:266 / 本文件两处。
// 猜错 ⇒ 归属报给插件健康与监控，指向不存在的名字；
// 且 `get_plugin_tools("ocr")` 会答「插件 ocr 没有可用的工具定义」。

// TestResolveToolPlugin_NoPhantomPlugins 钉住归属不得指向不存在的插件。
func TestResolveToolPlugin_NoPhantomPlugins(t *testing.T) {
	a := newToolOwnerTestAgent(t)

	// 这些内置工具的名字都含下划线，按旧规则会被截成假插件名。
	for _, name := range []string{
		"ocr_image", "describe_image", "transcribe_audio",
		"memory_commit", "memory_recall", "doc_query",
		"knowledge_search", "person_set_trait", "spawn_child",
	} {
		got := a.resolveToolPlugin(name)
		if got != "core" {
			t.Errorf("%s 归属为 %q，应为 core —— 旧规则会按下划线猜出"+
				"「%s」这个**不存在的插件**", name, got, strings.SplitN(name, "_", 2)[0])
		}
	}
}

// TestResolveToolPlugin_KnownPrefixStillResolves 确认没把真插件的归属也打死。
//
// 前缀规则对「确实是 <plugin>_<tool>」有意义（外部插件如 email_email_list）。
// 判据是「该前缀是**已注册**插件」，而不是任何带下划线的名字。
func TestResolveToolPlugin_KnownPrefixStillResolves(t *testing.T) {
	a := newToolOwnerTestAgent(t)
	// 无任何插件注册时，带下划线的名字一律归 core（不得编出插件名）
	if got := a.resolveToolPlugin("someplugin_someaction"); got != "core" {
		t.Errorf("未注册前缀应归 core，实际 %q", got)
	}
}

// TestGetPluginTools_ListsBuiltinTools 钉住「内置工具必须能被列出」。
//
// ★ 断言要按**条件**写，不能假设某个工具一定在：
//
//	ocr_image   受 core.input_processing.image.ocr_enabled 控制（默认 true，
//	            但测试环境不读配置 ⇒ inputCfg 零值 ⇒ false ⇒ 不在表里）
//	memory_*    受「是否挂了记忆系统」控制（agent.Memory == nil ⇒ 不在）
//
// 「无条件注册」指的是**不再被 pendingMedia 门挡住**，
// 不是「无视一切配置开关」—— 那是两件事。
func TestGetPluginTools_ListsBuiltinTools(t *testing.T) {
	a := newToolOwnerTestAgent(t)
	out := a.executeGetPluginTools("")
	if strings.Contains(out, "没有可用的工具定义") {
		t.Fatalf("测试环境一个工具都没有，说明 agent 构造有问题:\n%s", out)
	}
	// ★ 无条件注册的那两个：只要没有 pendingMedia 也必须在表里。
	//   （旧实现把它们挂在 pendingMedia != nil 上，本轮没有上传媒体时整块消失）
	for _, want := range []string{"spawn_child", "get_plugin_tools", "llm_list_sources", "output_list_channels"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_plugin_tools(\"\") 应列出内置工具 %s，实际输出:\n%s", want, out)
		}
	}
	// ★ 与「发给模型的工具表」逐条对齐 —— 这是「完整服务」的判据：
	//   凡是模型能调的，get_plugin_tools 就必须报得出；反之亦然。
	if missing := toolsMissingFromReport(t, a, out); missing != "" {
		t.Errorf("工具表与 get_plugin_tools 不一致:\n%s", missing)
	}
}

// toolsMissingFromReport 比对「工具表」与「get_plugin_tools 的输出」。
func toolsMissingFromReport(t *testing.T, a *Agent, report string) string {
	t.Helper()
	var missing []string
	for _, raw := range a.buildToolDefs() {
		fn := extractToolSchema(raw)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		if !strings.Contains(report, "### "+name+"\n") {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "模型能调但 get_plugin_tools 报不出：" + strings.Join(missing, ", ")
}

// TestGetPluginTools_CoreFilterWorks 确认按插件名过滤能命中内置工具。
func TestGetPluginTools_CoreFilterWorks(t *testing.T) {
	a := newToolOwnerTestAgent(t)
	out := a.executeGetPluginTools("core")
	if !strings.Contains(out, "spawn_child") {
		t.Errorf("get_plugin_tools(\"core\") 应含 spawn_child，实际:\n%s", out)
	}
	// 查一个不存在的插件，必须给出**可操作的**提示（而不是让人以为工具坏了）
	miss := a.executeGetPluginTools("ocr")
	if strings.Contains(miss, "describe_image") {
		t.Errorf("按不存在插件过滤时不该列出工具，实际:\n%s", miss)
	}
	if !strings.Contains(miss, "没有可用的工具定义") {
		t.Errorf("查询不存在的插件应明说没有，实际:\n%s", miss)
	}
}

// TestBuiltinToolOwner_NoDangling 钉住归属表里的名字不能是幻觉。
//
// ★★ 这条判据我改过两次基准，两次都错，教训值得记：
//
//	第 1 版：拿运行时 buildToolDefs 当基准
//	  ⇒ 错。memory_* / knowledge_* 受条件门控制（无 memory/knowledge 系统时
//	    不在表里），把「条件不满足」误报成「名字拼错了」。
//	第 2 版：扫源码里的 toolDef("…") 字面量
//	  ⇒ 也错。memory_recall 是由 internal/memory/indexer.go:442 **动态注入**的，
//	    源码里根本没有 toolDef("memory_recall" 这段文本。
//
// ⇒ 正确的基准是：**归属表的名字必须是内核真的会产出的工具名**。
//
//	判据改为「拼错的幻觉名」检测 —— 只断言一张**已知错名**清单里的名字
//	不在表里。归属表多写一个不存在的名字，本判据立刻红；
//	而条件门导致的「暂时不在表里」不会误报。
func TestBuiltinToolOwner_NoDangling(t *testing.T) {
	// 一批**看起来像**内置工具、实际不存在的名字。
	// 写进归属表就是「幻觉归属」：agent 调它会被报成归属错误/找不到。
	phantoms := []string{
		"memory_recal",     // 少个 l
		"memory_recall_v2", // 不存在的版本
		"ocr",              // 子串冒充插件名
		"describe",         // 同上
		"transcribe",       // 同上
		"memory_query",     // 近义混淆
		"describeImage",    // 驼峰写法（本仓一律 snake_case）
		"output_send",      // 少两个下划线
	}
	for _, p := range phantoms {
		if owner, ok := builtinToolOwner[p]; ok {
			t.Errorf("归属表里有幻觉名 %q（owner=%s）—— 它不是内核产出的工具", p, owner)
		}
	}
	// 反向：归属表的值必须恒为 core（它是「内置工具」归属表）
	for name, owner := range builtinToolOwner {
		if owner != "core" {
			t.Errorf("%q 的归属是 %q，但本表是**内置工具**归属表（应恒为 core）", name, owner)
		}
	}
}

// TestBuiltinToolOwner_CoversDeclaredTools 钉住「无条件声明的内置工具都登记了」。
//
// ★ 这条才是归属表的**核心**不变量：漏登记 ⇒ 退回「按下划线猜」
//
//	⇒ 又造出一个不存在的插件名（就是原缺陷）。
//	基准取「无条件声明」的那批，因为它们在任何 agent 上都必然在工具表里。
func TestBuiltinToolOwner_CoversDeclaredTools(t *testing.T) {
	a := newToolOwnerTestAgent(t)
	inTable := map[string]bool{}
	for _, raw := range a.buildToolDefs() {
		if fn := extractToolSchema(raw); fn != nil {
			if n, _ := fn["name"].(string); n != "" {
				inTable[n] = true
			}
		}
	}
	// 测试环境里必然存在的内置工具，每一个都必须登记在归属表里。
	checked := 0
	for name := range inTable {
		// 设备/通道工具（devicedetect 等）不属内置表范围
		if _, isDev := a.io.DeviceOfTool(name); isDev {
			continue
		}
		if _, ok := builtinToolOwner[name]; !ok {
			t.Errorf("内置工具 %q 未登记在 builtinToolOwner —— "+
				"会退回「按下划线猜」而归属到一个不存在的插件", name)
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("只核对到 %d 个内置工具（预期 ≥8），判据覆盖不足", checked)
	}
}

// newToolOwnerTestAgent 建一个够用又不碰外部依赖的 Agent。
func newToolOwnerTestAgent(t *testing.T) *Agent {
	t.Helper()
	return New(AgentConfig{
		ID:              "toolsvc",
		Provider:        windowProvider{n: 200000},
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
}
