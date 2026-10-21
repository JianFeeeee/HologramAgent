package core

// 前提验证（C7）：内核的 `@{别名}` 回填是否让插件的 `enc_shell` 变得**多余**。
//
// ## 为什么要先验这个再删代码
//
// C7 要「大幅删减插件的加密 shell 工具族」。删的依据是一个断言：
// 用户可以用**任何**工具（如内置的 cmd_run）配合 `@{别名}`，
// 由内核对所有工具参数统一回填 —— 而不必依赖插件专有的 enc_shell。
//
// 这个断言不成立的话，删掉 enc_shell 就是**削功能**而非去冗余。
//
// 已验证过 `TestFirewall_ToolArgsFilledBeforePlugin`（插件工具收到明文）。
// 这里补的是**决定性**的那一条：用**内置工具名**（cmd_run）也走同一条回填路径。

import (
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ★★ 判据：`cmd_run`（内置终端工具，非插件）的参数同样被内核回填。
//
// 这条成立 ⇒ 用户不必用插件专有的 enc_shell：
//
//	enc_shell(command="git push https://x:{token}@host/r.git fav")
//	  ↓ 等价于
//	cmd_run(command="git push https://x:@{sec_xxx}@host/r.git fav")
//
// 后者完全走内核机制，插件无需自带加密库与别名表。
func TestKernelFillCoversBuiltinToolsToo(t *testing.T) {
	store := newScrubStore(t)
	a, _, _ := newRootForResidents(t)

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "测试")
	if err != nil {
		t.Fatal(err)
	}

	// 注册一个**模拟 cmd_run 的内置工具**（不真的执行 shell），
	// 验证它通过 StageHost.ExecuteTool 拿到的是明文。
	var seen string
	if err := a.stageHost.RegisterTool("cmd_run", sdk.ToolDef{
		Name: "cmd_run", Description: "模拟内置终端工具",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		seen, _ = args["command"].(string)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.stageHost.ExecuteTool("cmd_run", map[string]interface{}{
		"command": "git push https://x:@" + "{" + alias + "}@host/r.git feat/x",
	}); err != nil {
		t.Fatal(err)
	}

	if seen == "" {
		t.Fatal("工具未被调用")
	}
	if !strings.Contains(seen, tok) {
		t.Fatalf("★ 内核回填未覆盖内置工具名 —— 那 enc_shell 就有存在理由（不能删）\n实际: %q", seen)
	}
	t.Logf("✅ 内置工具名的参数也被回填 ⇒ enc_shell 的填充功能是冗余的: %q", seen)
}

// ★★ 判据：内核回填**发生在插件 handler 之前** ⇒ 插件自己的别名表对
// 内核别名是**死代码**（永远不会命中）。
//
// 这个顺序很关键：
//
//	StageHost.ExecuteTool → security.FillMap(args) → handler(args)
//
// 所以当内核安全模块启用时，插件的 enc_shell 收到的 command 里
// `@{sec_xxx}` **已经变成明文**了；插件自己的别名正则再扫也扫不到。
//
// ⇒ 结论：插件的「填充 @{别名}」这部分是**冗余**的（内核先做了）。
//
//	但它的 argv 保护（脚本走 stdin）与输出脱敏**仍然独有**，不可删。
func TestKernelFillHappensBeforePluginHandler(t *testing.T) {
	store := newScrubStore(t)
	a, _, _ := newRootForResidents(t)

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "测试")
	if err != nil {
		t.Fatal(err)
	}

	var handlerSaw string
	if err := a.stageHost.RegisterTool("probe_encshell", sdk.ToolDef{
		Name: "probe_encshell", Description: "模拟 enc_shell",
		Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		handlerSaw, _ = args["command"].(string)
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := a.stageHost.ExecuteTool("probe_encshell", map[string]interface{}{
		"command": "curl -H 'Authorization: token @{" + alias + "}'",
	}); err != nil {
		t.Fatal(err)
	}

	// handler 里看到的**已经没有 @{别名}** 了（已被内核换掉）
	if strings.Contains(handlerSaw, "@{"+alias+"}") {
		t.Fatalf("内核回填应在 handler 之前完成；实际 handler 仍看到别名: %q", handlerSaw)
	}
	if !strings.Contains(handlerSaw, tok) {
		t.Fatalf("handler 应看到明文: %q", handlerSaw)
	}
	t.Logf("✅ 内核先回填，插件看不到别名 ⇒ 插件自带别名表对内核别名是死代码")
}

// ★★ 判据：内核与插件的 `@{...}` 引用**语法不冲突**（带连字符的名字归插件）。
//
// 这是个**偶然但真实**的兼容，必须钉住，否则将来有人"统一语法"就炸：
//
//	内核 refPattern:  @\{([A-Za-z0-9_]{1,64})\}    ← 不含 - 和 .
//	插件 credRefPattern: @\{([A-Za-z0-9_.-]{1,64})\}  ← 含 - 和 .
//
// 于是：
//
//	@{sec_3718c87a11e9478b}  内核吃（无连字符）
//	@{gitea-local}           内核**不吃**（有连字符）⇒ 留给插件解析
//
// ★ 若把内核的字符集放宽到含 `-`，插件自己的凭据（gitea-local）会被内核
//
//	先吃掉 —— 那些名字不在内核变量库里，内核会原样保留（认识不到），
//	看起来"没坏"，但只要恰好有一个同名内核变量，就会静默换错值。
func TestKernelAndPluginAliasSyntaxDoNotCollide(t *testing.T) {
	store := newScrubStore(t)
	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "内核变量")
	if err != nil {
		t.Fatal(err)
	}

	// 内核别名（无连字符）→ 被回填
	if got, n, _ := store.Fill("x:@" + "{" + alias + "}"); n != 1 || !strings.Contains(got, tok) {
		t.Fatalf("内核别名应被回填: n=%d got=%q", n, got)
	}
	// 插件风格的名字（含连字符）→ **不被内核碰**
	pluginStyle := "git push https://x:@" + "{gitea-local}@host/r.git"
	if got, n, _ := store.Fill(pluginStyle); n != 0 || !strings.Contains(got, "{gitea-local}") {
		t.Fatalf("含连字符的引用内核不应碰（那是插件的凭据）: n=%d got=%q", n, got)
	}
	t.Logf("✅ 内核只吃 [A-Za-z0-9_]，@{gitea-local} 留给插件")
}
