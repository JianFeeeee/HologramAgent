package pluginmgr

// plugin_start/stop/enable/disable 工具的判据（2026-10-16）。
//
// 设计见 docs/zh/kernel-modularization-design.md §2.6.7.4。
//
// ## 判据重点：**工具的措辞必须让 agent 选对**
//
// 四个工具里两两一对，差别只在**是否持久**：
//
//	plugin_stop    vs plugin_disable   （停掉 vs 停掉且以后也不自启）
//	plugin_start   vs plugin_enable    （拉起 vs 拉起且以后开机也自启）
//
// agent 只能靠 Description 分辨。若描述不区分，agent 会在"临时关一下"
// 的场景误用 plugin_disable ⇒ 用户下次开机发现插件不自启（真实缺陷场景）。
//
// ## 判据必须走真实 registerTools
//
// 本仓教训：只查"源码里有这个字符串"会被 `if false &&` 之类的
// 写法骗过（E-3 那次实测）。所以这里真的调 registerTools 并捕获注册结果。

import (
	"os"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// collectToolDefs 走真实 registerTools 收集工具定义。
//
// 捕获点是 `SDKConfig.RegTool`（真实注册落点），不是别处的副本 ——
// 这样"某工具被删掉/改名"必然反映在这里。
func collectToolDefs(t *testing.T) map[string]sdk.ToolDef {
	t.Helper()
	defs := map[string]sdk.ToolDef{}
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings:  &pmSettings{},
		EventBus:  events.NewBus(),
		PluginMgr: &fakePluginMgr{},
		RegTool: func(name string, def sdk.ToolDef, h sdk.ToolHandler) error {
			defs[name] = def
			return nil
		},
	})
	p := &Plugin{name: "pluginmgr"}
	p.registerTools(sdkInst)
	if len(defs) == 0 {
		t.Fatal("registerTools 未注册任何工具 —— 捕获点失效，本文件全部判据会假绿")
	}
	return defs
}

// ★★★ 判据：**四个生命周期工具全部注册**（走真实注册路径）。
func TestLifecycleToolsAreRegistered(t *testing.T) {
	defs := collectToolDefs(t)
	for _, want := range []string{"plugin_start", "plugin_stop", "plugin_enable", "plugin_disable"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("★ 缺少工具 %s —— agent 无法%s插件", want, want)
		}
	}
	// 原有 6 个不得因新增而丢失（回归）
	for _, want := range []string{"plugin_install", "plugin_list", "plugin_status",
		"plugin_restart", "plugin_remove", "plugin_info"} {
		if _, ok := defs[want]; !ok {
			t.Errorf("★ 原有工具 %s 丢失（回归）", want)
		}
	}
}

// ★★★ 判据：**运行态与持久态工具的描述必须互相点名**。
//
// 这是防止 agent 误用的**唯一手段**（工具选择完全靠描述）。
func TestRuntimeVsPersistentToolDescriptionsDisambiguate(t *testing.T) {
	defs := collectToolDefs(t)

	// ★ 两条**独立**断言（不能用 || 合并）：
	//
	// 第一版写成 `!A && !B` ⇒ 去掉 A（"不改持久状态"）后 B（"不会影响开机自启"）
	// 仍在 ⇒ 整条通过 ⇒ **变异没咬住**。两个措辞各有作用：
	//   A = 直接陈述"不改持久"（正面定义）
	//   B = 描述用户可观察的后果（背面印证）
	// 缺任一都让 agent 更难判断，所以**两条都要在**。
	stop := defs["plugin_stop"].Description
	if !strings.Contains(stop, "不改持久状态") {
		t.Errorf("★ plugin_stop 描述未出现\"不改持久状态\"（正面定义缺失）\n%s", stop)
	}
	if !strings.Contains(stop, "不会影响开机自启") {
		t.Errorf("★ plugin_stop 描述未出现\"不会影响开机自启\"（后果说明缺失）\n%s", stop)
	}
	if !strings.Contains(stop, "plugin_disable") {
		t.Errorf("★ plugin_stop 描述未指向 plugin_disable（持久关闭）作为对比\n%s", stop)
	}

	dis := defs["plugin_disable"].Description
	if !strings.Contains(dis, "跨重启保留") && !strings.Contains(dis, "持久") {
		t.Errorf("★ plugin_disable 描述未说明它是**持久**的\n%s", dis)
	}
	if !strings.Contains(dis, "plugin_stop") {
		t.Errorf("★ plugin_disable 描述未指向 plugin_stop（临时关闭）作为对比\n%s", dis)
	}

	if st := defs["plugin_start"].Description; !strings.Contains(st, "持久") {
		t.Errorf("★ plugin_start 描述未提持久禁用的处理\n%s", st)
	}
	if en := defs["plugin_enable"].Description; !strings.Contains(en, "清除") || !strings.Contains(en, "自启") {
		t.Errorf("★ plugin_enable 描述未说明它会让之后开机自启\n%s", en)
	}
}

// ★★★ 判据：**四个工具各自转调不同的 API**（不是别名）。
//
// 若 plugin_stop 内部转调 DisablePlugin（或反过来），两者行为相同
// ⇒ 上述描述全是谎话。这里对每个工具的 handler 做"源码块内包含期望调用"检查。
//
// ★ 为何能接受源码检查（本仓曾因此吃亏）：这里检查的是**期望调用一定存在**，
// 是单向的（存在性），不会被 `if false &&` 骗过 —— 那种写法仍需写出该调用。
// 真正的行为验证在 internal/plugin 包的 runtime_state_test.go（真加载真启停）。
func TestRuntimeToolsCallDistinctAPIs(t *testing.T) {
	src := readPluginSource(t)
	cases := []struct{ tool, want string }{
		{"plugin_start", "mgr.StartPlugin("},
		{"plugin_stop", "mgr.StopPlugin("},
		{"plugin_enable", "mgr.EnablePlugin("},
		{"plugin_disable", "mgr.DisablePlugin("},
	}
	for _, c := range cases {
		idx := strings.Index(src, `s.RegisterTool("`+c.tool+`"`)
		if idx < 0 {
			t.Fatalf("未找到 %s 的注册", c.tool)
		}
		rest := src[idx:]
		// 截到下一个 RegisterTool
		if nxt := strings.Index(rest[len(c.tool)+20:], `s.RegisterTool(`); nxt >= 0 {
			rest = rest[:len(c.tool)+20+nxt]
		}
		if !strings.Contains(rest, c.want) {
			t.Errorf("★ %s 的 handler 未调用 %s —— 可能误指向另一个 API（行为与描述不符）",
				c.tool, c.want)
		}
	}
}

// readPluginSource 读本包源码（判据的静态检查用）。
func readPluginSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("plugin.go")
	if err != nil {
		t.Fatalf("读 plugin.go: %v", err)
	}
	return string(b)
}
