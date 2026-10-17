package proc

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 权限梯度测试（§3.8）。
//
// 守住的核心性质：外部插件拿不到内核内部机制，不是因为 C ABI 传不了
// 函数指针（那是运气），而是因为这里**显式声明并强制**了边界。

// 每个 method 都必须登记能力归属。
//
// ❗ 这是本文件最重要的测试：漏登记的 method 会按 CapCore 放行，
// 等于绕过整套权限检查。新增 method 时忘了登记，这里会当场报出来。
func TestCapability_AllMethodsClassified(t *testing.T) {
	// 与 protocol.go 的 method 常量对齐。内核→插件的 7 个调用不经 Handle，
	// 故不需要能力归属。
	kernelToPlugin := map[string]bool{
		MethodPluginInit:    true,
		MethodPluginStart:   true,
		MethodPluginStop:    true,
		MethodToolInvoke:    true,
		MethodCleanerInvoke: true,
		MethodStageInvoke:   true,
		MethodOutputInvoke:  true,
	}

	// ★ 从 protocol.go **源码**提取全部 method 常量，而不是维护手工清单。
	//
	// 为什么改：初版是手写清单，于是「新增 method 忘了登记能力」时
	// 清单里也没有它 —— 测试照样绿，守卫形同虚设（这正是初版的目的所在，
	// 却因实现方式而失效）。源码扫描让覆盖面**自动**跟上常量定义。
	src, err := os.ReadFile("protocol.go")
	if err != nil {
		t.Fatalf("读 protocol.go 失败: %v", err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Method[A-Za-z0-9_]+)\s*=\s*"([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) < 40 {
		t.Fatalf("只从 protocol.go 提取到 %d 个 method 常量 —— 正则或文件结构变了，"+
			"请修正本判据（否则它会假绿）", len(matches))
	}

	seen := 0
	for _, m := range matches {
		value := m[2]
		if kernelToPlugin[value] {
			continue // 内核→插件方向：不经 Handle，无需能力
		}
		seen++
		if _, ok := capabilityOf(value); !ok {
			t.Errorf("method %q (%s) 未登记能力归属 —— 会按 CapCore 放行，绕过权限检查",
				value, m[1])
		}
	}
	if seen == 0 {
		t.Fatal("没有校验到任何 method —— 判据失效")
	}

	// 反向：能力表里的每一项都必须是 protocol.go 里真实存在的 method。
	// 防「改了 method 名却只改一处」这类错位。
	declared := map[string]bool{}
	for _, m := range matches {
		declared[m[2]] = true
	}
	for method := range methodCapability {
		if !declared[method] {
			t.Errorf("能力表里的 %q 在 protocol.go 里不存在（常量改名后漏改？）", method)
		}
	}
}

// 未声明 capabilities 的插件不受限（存量插件向后兼容）。
//
// 若空声明当作最小权限，17 个存量插件会全部失去 IO 注入/记忆读写而静默降级。
func TestCapability_EmptyDeclarationIsUnrestricted(t *testing.T) {
	s := newCapabilitySet(nil)
	for _, m := range []string{
		MethodIOInjectText, MethodMemoryCommit, MethodPluginReloadOne,
		MethodSettingsSetCore, MethodEventsSubscribe,
	} {
		if ok, _ := s.allows(m); !ok {
			t.Errorf("未声明 capabilities 时 %q 应放行（存量插件兼容）", m)
		}
	}

	s2 := newCapabilitySet([]string{})
	if ok, _ := s2.allows(MethodMemoryCommit); !ok {
		t.Error("空数组也应视为不受限")
	}
}

// 声明了能力后，未声明的组被拒。
func TestCapability_DeclaredSetRestrictsOthers(t *testing.T) {
	// 只声明 io：能注入，但不能碰记忆/插件管理/核心配置
	s := newCapabilitySet([]string{"io"})

	allowed := []string{MethodIOInjectText, MethodIOInjectSync}
	for _, m := range allowed {
		if ok, _ := s.allows(m); !ok {
			t.Errorf("声明 io 后 %q 应放行", m)
		}
	}

	denied := map[string]Capability{
		MethodMemoryCommit:     CapMemory,
		MethodKnowledgeAdd:     CapKnowledge,
		MethodPluginReloadOne:  CapPluginMgr,
		MethodSettingsSetCore:  CapCrossPluginSettings,
		MethodEventsSubscribe:  CapEvents,
		MethodLLMSetSource:     CapLLM,
		MethodTextMemoryAppend: CapTextMemory,
		MethodDocInsert:        CapDocMemory,
		MethodSocialGetPerson:  CapSocial,
	}
	for m, wantCap := range denied {
		ok, gotCap := s.allows(m)
		if ok {
			t.Errorf("未声明 %q 时 %q 应被拒", wantCap, m)
		}
		if gotCap != wantCap {
			t.Errorf("%q 的能力归属 = %q，期望 %q", m, gotCap, wantCap)
		}
	}
}

// core 能力始终可用，无需声明。
//
// 没有它插件无法注册工具、读写自己的配置、参与 stage 锁仲裁——
// 即完全无法工作。
func TestCapability_CoreAlwaysAllowed(t *testing.T) {
	s := newCapabilitySet([]string{"io"}) // 只声明 io

	for _, m := range []string{
		MethodHandshake,
		MethodToolRegister, MethodStageRegister, MethodOutputRegister,
		MethodInputRegister, MethodAPIRegister,
		MethodStageLock, MethodStageUnlock,
		MethodSettingsGet, MethodSettingsSet, MethodSettingsList,
		MethodSettingsDefs, MethodSettingsRegisterDef, MethodSettingsDataDir,
		MethodLifecycleAutoRestart,
		MethodIOSetToolBlocks,
	} {
		if ok, _ := s.allows(m); !ok {
			t.Errorf("core 能力 %q 应始终放行", m)
		}
	}
}

// 自身配置读写属 core，跨插件/核心配置需显式声明。
//
// 这个区分是有意的：读写自己的配置是插件正常工作所需；
// 读写别人的配置能改别人行为，读写核心配置能改内核行为。
func TestCapability_SettingsScopeSeparation(t *testing.T) {
	s := newCapabilitySet([]string{}) // 不受限，先确认归属正确

	own := []string{MethodSettingsGet, MethodSettingsSet, MethodSettingsList}
	for _, m := range own {
		if cap, _ := capabilityOf(m); cap != CapCore {
			t.Errorf("%q 应属 core（自身配置），实际 %q", m, cap)
		}
	}

	cross := []string{
		MethodSettingsGetCore, MethodSettingsSetCore, MethodSettingsListCore,
		MethodSettingsGetPlugin, MethodSettingsSetPlugin, MethodSettingsListPlugin,
		MethodSettingsDump, MethodSettingsPlugins,
	}
	for _, m := range cross {
		if cap, _ := capabilityOf(m); cap != CapCrossPluginSettings {
			t.Errorf("%q 应属 settings_cross，实际 %q", m, cap)
		}
	}
	_ = s
}

// 被拒时错误消息必须可操作：说清缺什么、怎么补。
//
// 针对的是 C ABI 时代的一类真实故障：case 23/24 返回成功但永远收不到事件，
// 插件作者无从得知。
func TestCapability_DeniedErrorIsActionable(t *testing.T) {
	err := errCapabilityDenied("demo", MethodMemoryCommit, CapMemory)
	msg := err.Error()

	for _, want := range []string{
		"demo",             // 哪个插件
		MethodMemoryCommit, // 哪个调用
		string(CapMemory),  // 缺什么能力
		"capabilities",     // 在哪声明
		"plugin.json",      // 声明在哪个文件
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息应含 %q，实际: %s", want, msg)
		}
	}

	// 还应列出可用能力名，避免作者猜
	if !strings.Contains(msg, string(CapEvents)) {
		t.Errorf("错误消息应列出可选能力（如 %q），实际: %s", CapEvents, msg)
	}
}

// coreHandler 在 Handle 入口强制权限，被拒的调用不进 switch。
func TestCapability_HandleEnforcesAtRPCBoundary(t *testing.T) {
	core := newFakeCore()
	h := &coreHandler{
		sdk:  core,
		name: "restricted",
		caps: newCapabilitySet([]string{"io"}), // 不含 memory
	}

	_, err := h.Handle(MethodMemoryCommit, json.RawMessage(`{"triples":[]}`))
	if err == nil {
		t.Fatal("未声明 memory 能力时 memory.commit 应被拒")
	}
	if !strings.Contains(err.Error(), "被拒") {
		t.Errorf("应是权限拒绝错误，实际: %v", err)
	}

	// 已声明的能力照常走到 switch（这里 memory 为 nil，会返回 errUnavailable，
	// 但错误类型不同——证明请求进了 switch 而非被权限拦下）
	h2 := &coreHandler{
		sdk:  core,
		name: "allowed",
		caps: newCapabilitySet([]string{"memory"}),
	}
	_, err2 := h2.Handle(MethodMemoryCommit, json.RawMessage(`{"triples":[]}`))
	if err2 != nil && strings.Contains(err2.Error(), "被拒") {
		t.Errorf("声明了 memory 后不应被权限拒绝，实际: %v", err2)
	}
}

// 刻意不提供的内核内部机制必须有明确记录。
//
// 这些没有对应 method 常量——不是忘了加，是决定不加。
// 列表存在本身就是「这是策略而非疏漏」的证据。
func TestCapability_WithheldListIsDocumented(t *testing.T) {
	withheld := WithheldCapabilities()

	// §3.8 明确列为「不提供」的
	for _, name := range []string{"SelftestAPI", "SupervisorAPI", "TrackerAPI"} {
		reason, ok := withheld[name]
		if !ok {
			t.Errorf("%s 应在 withheld 清单中（§3.8 明确不提供）", name)
			continue
		}
		if reason == "" {
			t.Errorf("%s 缺少不提供的理由", name)
		}
	}

	// 每一项都必须有理由，否则读代码的人无从判断边界为何在此
	for name, reason := range withheld {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("withheld 项 %q 缺少理由", name)
		}
	}

	// 这些能力不应被任何 method 暴露。
	//
	// 匹配用的是去掉 API 后缀的词根 + 词边界，而非直接子串：
	// 直接子串匹配会把 tool.register / io.setToolBlocks 误判为泄露 ToolAPI，
	// 而那两个是合法开放的（注册自己的工具、设置自己工具的返回块）。
	// 真正要拦的是形如 "tool.unregister" / "tracker.diff" 这类新增的越权 method。
	forbiddenPrefixes := map[string]string{
		"selftest.":      "SelftestAPI",
		"supervisor.":    "SupervisorAPI",
		"tracker.":       "TrackerAPI",
		"status.":        "StatusAPI",
		"adapter.":       "AdapterAPI",
		"config.":        "ConfigAPI",
		"indexer.":       "IndexerAPI",
		"outputchan.":    "OutputChanRaw",
		"events.publish": "EventPublish",
	}
	for m := range methodCapability {
		lower := strings.ToLower(m)
		for prefix, capName := range forbiddenPrefixes {
			if strings.HasPrefix(lower, prefix) {
				t.Errorf("method %q 暴露了刻意不提供的能力 %q", m, capName)
			}
		}
		// 工具表直接操作：注册自己的工具合法，注销别人的不合法
		if strings.Contains(lower, "unregister") {
			t.Errorf("method %q 暴露了工具注销能力（ToolAPI，刻意不提供）", m)
		}
	}
}

// KnownCapabilities 不含 core（无需声明），且与 methodCapability 一致。
func TestCapability_KnownListExcludesCore(t *testing.T) {
	known := KnownCapabilities()
	for _, k := range known {
		if k == string(CapCore) {
			t.Error("KnownCapabilities 不应含 core（无需声明）")
		}
	}

	// 每个非 core 能力都应可声明
	declared := map[string]bool{}
	for _, k := range known {
		declared[k] = true
	}
	for _, cap := range methodCapability {
		if cap == CapCore {
			continue
		}
		if !declared[string(cap)] {
			t.Errorf("能力 %q 在 methodCapability 中使用但不在 KnownCapabilities 里", cap)
		}
	}
}

// 未知 method 走 Handle 的兜底分支，不因权限检查提前返回误导性错误。
func TestCapability_UnknownMethodFallsThrough(t *testing.T) {
	h := &coreHandler{
		sdk:  newFakeCore(),
		name: "demo",
		caps: newCapabilitySet([]string{"io"}),
	}
	_, err := h.Handle("nonexistent.method", nil)
	if err == nil {
		t.Fatal("未知 method 应报错")
	}
	// 应是「未知 method」而非「权限被拒」——否则作者会以为是漏声明能力
	if strings.Contains(err.Error(), "被拒") {
		t.Errorf("未知 method 不应报权限错误，实际: %v", err)
	}
}
