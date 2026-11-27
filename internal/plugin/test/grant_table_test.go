package test

// grant_table_test.go —— 授权三档的**表运算**判据。
//
// 只用公开函数（`plugin.Grant` 的方法），不看内部结构、不读源码文本。

import (
	"sort"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

// all 是"全量表"的替身（真实运行时来自 plghost 的插件清单 / 工具表）。
var allPlugins = []string{"cli", "webui", "skillmgr", "files", "seq"}

func sortIn(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func eq(t *testing.T, label string, got, want []string) {
	t.Helper()
	g, w := sortIn(got), sortIn(want)
	if len(g) != len(w) {
		t.Fatalf("%s: 得到 %v，期望 %v", label, g, w)
	}
	for i := range g {
		if g[i] != w[i] {
			t.Fatalf("%s: 得到 %v，期望 %v", label, g, w)
		}
	}
}

// 三档全空 ⇒ 不裁剪（存量 fail-open 契约）。
func TestEmptyGrantKeepsEverything(t *testing.T) {
	var g plugin.Grant
	if !g.IsZero() {
		t.Fatal("零值 Grant 应报告 IsZero")
	}
	eq(t, "PluginTable", g.PluginTable(allPlugins), allPlugins)
	eq(t, "ToolTable", g.ToolTable(allPlugins), allPlugins)
	for _, n := range allPlugins {
		if !g.PluginVisible(n) {
			t.Fatalf("空授权下 %q 应可见", n)
		}
	}
}

// 只声明 allow ⇒ 全量 ∩ allow（"只有这些"）。
func TestAllowOnlyNarrowsToAllowSet(t *testing.T) {
	g := plugin.Grant{AllowPlugins: []string{"cli", "webui"}}
	eq(t, "PluginTable", g.PluginTable(allPlugins), []string{"cli", "webui"})
	if g.PluginVisible("skillmgr") {
		t.Fatal("allow 之外的插件不该可见")
	}
}

// ★ add 的核心场景：allow 非空时 add 仍能补进表里。
//
// 改造前的实现（封闭白名单）会在这里失败：allow 非空 ⇒ 只认 allow ⇒ add 被拒。
func TestAddExtendsBeyondAllow(t *testing.T) {
	g := plugin.Grant{
		AllowPlugins: []string{"cli"},
		AddPlugins:   []string{"files"},
	}
	eq(t, "PluginTable", g.PluginTable(allPlugins), []string{"cli", "files"})
	if !g.PluginVisible("cli") || !g.PluginVisible("files") {
		t.Fatal("allow 与 add 应共存")
	}
	if g.PluginVisible("webui") {
		t.Fatal("未声明的插件不该可见")
	}
}

// ★ deny 是删除操作：它既能撤销 allow 里的，也能撤销 add 里的。
func TestDenyRemovesFromBothAllowAndAdd(t *testing.T) {
	g := plugin.Grant{
		AllowPlugins: []string{"cli", "webui"},
		AddPlugins:   []string{"files"},
		DenyPlugins:  []string{"webui", "files"},
	}
	eq(t, "PluginTable", g.PluginTable(allPlugins), []string{"cli"})
	if g.PluginVisible("webui") {
		t.Fatal("deny 应能撤销 allow 里的项")
	}
	if g.PluginVisible("files") {
		t.Fatal("deny 应能撤销 add 里的项")
	}
}

// 只声明 deny ⇒ 全量减去 deny（不引入 allow 的封闭语义）。
func TestDenyOnlyRemovesFromFullTable(t *testing.T) {
	g := plugin.Grant{DenyPlugins: []string{"skillmgr"}}
	eq(t, "PluginTable", g.PluginTable(allPlugins), []string{"cli", "webui", "files", "seq"})
}

// 通配形式（尾部 *）在 allow / deny 两侧都生效。
func TestWildcardPatternsInAllThreeTiers(t *testing.T) {
	tools := []string{"cmd_run", "cmd_kill", "read_file", "write_file"}
	g := plugin.Grant{AllowTools: []string{"cmd_*"}}
	eq(t, "allow 通配", g.ToolTable(tools), []string{"cmd_run", "cmd_kill"})

	g = plugin.Grant{DenyTools: []string{"cmd_*"}}
	eq(t, "deny 通配", g.ToolTable(tools), []string{"read_file", "write_file"})

	g = plugin.Grant{AllowTools: []string{"read_*"}, AddTools: []string{"cmd_run"}}
	eq(t, "add 具体名", g.ToolTable(tools), []string{"read_file", "cmd_run"})
}

// add 里的通配要展开成全量里的命中项（而不是原样保留 "*"）。
func TestAddWildcardExpandsAgainstFullTable(t *testing.T) {
	g := plugin.Grant{AllowPlugins: []string{"cli"}, AddPlugins: []string{"s*"}}
	eq(t, "add 通配", g.PluginTable(allPlugins), []string{"cli", "seq", "skillmgr"})
}

// 插件组与工具组互不影响（两组独立运算）。
func TestPluginAndToolGrantsAreIndependent(t *testing.T) {
	g := plugin.Grant{
		DenyPlugins: []string{"skillmgr"},
		AllowTools:  []string{"read_file"},
		DenyTools:   []string{},
		AddTools:    []string{"cmd_run"},
	}
	eq(t, "插件表", g.PluginTable(allPlugins), []string{"cli", "webui", "files", "seq"})
	eq(t, "工具表", g.ToolTable([]string{"read_file", "write_file", "cmd_run"}), []string{"read_file", "cmd_run"})
}

// 单点判定（visibleIn）必须与整表运算（tableOf）逐字一致。
//
// ★ 这条是防漂移的关键：两处若不一致，会出现"渲染看得见、执行被拒"
// 或反过来的静默错配。
func TestSingleQueryMatchesTableComputation(t *testing.T) {
	cases := []plugin.Grant{
		{},
		{AllowPlugins: []string{"cli", "webui"}},
		{AllowPlugins: []string{"cli"}, AddPlugins: []string{"files"}},
		{AllowPlugins: []string{"cli", "webui"}, AddPlugins: []string{"files"}, DenyPlugins: []string{"webui"}},
		{DenyPlugins: []string{"cli", "seq"}},
		{AddPlugins: []string{"s*"}},
	}
	for i, g := range cases {
		table := g.PluginTable(allPlugins)
		inTable := map[string]bool{}
		for _, n := range table {
			inTable[n] = true
		}
		for _, n := range allPlugins {
			if got, want := g.PluginVisible(n), inTable[n]; got != want {
				t.Fatalf("case %d 名字 %q：PluginVisible=%v 但整表里=%v（两处判据漂移）", i, n, got, want)
			}
		}
	}
}

// 空白项与空串不得污染表（配置里常见 "a, b,, c"）。
func TestBlankEntriesAreIgnored(t *testing.T) {
	g := plugin.Grant{AllowPlugins: []string{" cli ", "", "  "}}
	eq(t, "去空白", g.PluginTable(allPlugins), []string{"cli"})
}
