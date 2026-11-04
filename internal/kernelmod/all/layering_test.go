package all

// 架构防腐判据（2026-11-04）。
//
// ## 这组判据守的是"分层不要退化成莲藕"
//
// 本次重构（见 kernelplan.md）解决的核心问题是：**共享数据靠手工接线**。
// 它退化的形态有四条已知通道，每条都真实发生过：
//
//	core      → Agent 字段（每种 agent 构造点都要记得传）
//	plugin    → 12 个 SetXxx（合法性见下面 TestPluginSettersAreInjection）
//	sdk       → SDKConfig 传参
//	security  → 包级单例 Global() / RuntimeGlobal() / ConfigCodec()
//
// 前三条里，`security` 那条是**真缺陷**（已删除）；`core` 那条也是
// （skill 索引没进子 agent 提示词）；`plugin` 那条经实测是**合法的
// 构造期注入**（每个 Setter 只有一个调用点、调用方持有 ks、少了立刻报错）。
//
// 判据要能区分这两类，否则要么放过真缺陷，要么逼人做无意义的改造。
//
// ## 每条判据都做过变异验证
//
// 见各条注释里的"变红条件"。没有变异验证的判据等于不存在
// （engineering-lessons §1.1：结构判据 ≠ 质量判据）。

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ─────────────────── ① 环方向：底层包不得反向依赖装配器 ───────────────────

// TestCoreDoesNotImportKernelboot 钉住 core 不依赖装配引擎。
//
// 为什么重要：core 是环的一极。它一旦依赖 kernelboot，就无法读任何
// 统一挂载点（环），只能自建通道 —— 那正是本次重构要消灭的形态。
//
// 变红条件：在 internal/agent/core 任一文件里 import kernelboot ⇒ 红。
func TestCoreDoesNotImportKernelboot(t *testing.T) {
	assertNoDep(t, "github.com/JianFeeeee/HologramAgent/internal/agent/core",
		"github.com/JianFeeeee/HologramAgent/internal/kernelboot")
}

// TestPluginDoesNotImportKernelboot 钉住 plugin 不依赖装配引擎。
func TestPluginDoesNotImportKernelboot(t *testing.T) {
	assertNoDep(t, "github.com/JianFeeeee/HologramAgent/internal/plugin",
		"github.com/JianFeeeee/HologramAgent/internal/kernelboot")
}

// TestSdkDoesNotImportKernelboot 钉住 sdk 不依赖装配引擎。
func TestSdkDoesNotImportKernelboot(t *testing.T) {
	assertNoDep(t, "github.com/JianFeeeee/HologramAgent/internal/sdk",
		"github.com/JianFeeeee/HologramAgent/internal/kernelboot")
}

// TestSecurityDoesNotImportKernelboot 钉住 security 不依赖装配引擎。
func TestSecurityDoesNotImportKernelboot(t *testing.T) {
	assertNoDep(t, "github.com/JianFeeeee/HologramAgent/internal/security",
		"github.com/JianFeeeee/HologramAgent/internal/kernelboot")
}

// TestKstateDoesNotDependOnCore 是"core 能 import kstate"的另一面。
//
// 只有这一半成立（kstate 不依赖 core），"core 读统一挂载点"这条路才通。
//
// 变红条件：kstate.State 里加 *agentCore.Xxx 类型的字段 ⇒ 红。
func TestKstateDoesNotDependOnCore(t *testing.T) {
	assertNoDep(t, "github.com/JianFeeeee/HologramAgent/internal/kstate",
		"github.com/JianFeeeee/HologramAgent/internal/agent/core")
}

// ─────────────────── ② 共享数据只有一个来源 ───────────────────

// TestNoSecurityPackageLevelSingletons 已在 internal/security 内实现
// （singleton_test.go）。这里只做跨包的**存在性**检查：确认那组判据没被删。
//
// 为什么需要这条：判据文件被删是很常见的"静默废掉判据"方式
// —— 删掉之后所有测试仍然绿，只是再也不检查了。
func TestSecuritySingletonGuardStillExists(t *testing.T) {
	src, err := os.ReadFile("../../security/singleton_test.go")
	if err != nil {
		t.Fatalf("★ security 的单例判据文件不见了（%v）——\n"+
			"删除判据是最隐蔽的「修复」方式：测试全绿，但再也不检查了。", err)
	}
	if !strings.Contains(string(src), "TestNoPackageLevelSingletons") {
		t.Fatal("★ singleton_test.go 里找不到 TestNoPackageLevelSingletons")
	}
	for _, want := range []string{`"global"`, `"runtime"`, `"globalCodec"`} {
		if !strings.Contains(string(src), want) {
			t.Fatalf("★ 禁表里缺 %s —— 那个单例可以原样加回来", want)
		}
	}
}

// ─────────────────── ③ Agent 不得持有共享数据字段 ───────────────────

// TestAgentHasNoPromptSectionsField 钉住 Agent 不再持插件声明的提示词段。
//
// 这是本次故障的直接来源：skillIndex 曾是 Agent 的字段，导致 rtchild /
// 驻留子 / 工作式子各自构造时都要记得传，漏传就是"那个 agent 看不到任何
// skill"，而模型只能凭工具名猜用法。
//
// 变红条件：给 Agent 加回 promptSections 或 skillIndex 字段 ⇒ 红。
func TestAgentHasNoPromptSectionsField(t *testing.T) {
	src, err := os.ReadFile("../../agent/core/agent.go")
	if err != nil {
		t.Fatalf("读 agent.go: %v", err)
	}
	text := string(src)

	// 只看结构体字段声明行（允许注释里提到这些名字）。
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(trimmed, "promptSections ") && strings.Contains(trimmed, "*") {
			t.Fatalf("★ Agent 又持有了提示词段字段: %q\n\n"+
				"它必须挂在 kstate.State.PromptSections 上，任何 agent\n"+
				"（含未来新类型）读同一份 —— 零接线。", trimmed)
		}
		if strings.Contains(trimmed, "skillIndex ") {
			t.Fatalf("★ Agent 又持有了 skillIndex 字段: %q\n"+
				"详情见上述 PromptSections 的说明。", trimmed)
		}
	}
}

// ─────────────────── ④ plugin 的 Setter 是合法注入（防误改） ───────────────────

// TestPluginSettersAreInjection 钉住"这些 Setter 不是要收敛的东西"。
//
// 背景：kernelplan v1 曾主张把 12 个 SetXxx 改成"读 kstate"。
// 实测做不到 —— kstate 持有 *plugin.Registry，plugin 若 import kstate
// 就是 import cycle（已用探针实证）。
//
// 更重要的是：它们**本来就不是缺陷**。每个 Setter 只有一个调用点，
// 调用方持有 ks，显式传参；少了任何一个插件拿 nil 立刻报错，
// 不存在"静默降级"（那是包级单例的病症）。
//
// 本条判据防的是**过度收敛**：后来者看到一堆 Setter 就想合并成全局注册表，
// 那会把"显式注入"退化成"隐式全局"。
//
// 变红条件：plugin.Registry 出现读 kstate 的代码 ⇒ 红（说明有人硬改）。
func TestPluginSettersAreInjection(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps",
		"github.com/JianFeeeee/HologramAgent/internal/plugin").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	deps := strings.Fields(string(out))
	for _, d := range deps {
		if d == "github.com/JianFeeeee/HologramAgent/internal/kstate" {
			t.Fatal("★ internal/plugin 依赖了 kstate —— 应该是 import cycle。\n\n" +
				"若这真的编译过了，说明 kstate 不再持有 *plugin.Registry，\n" +
				"那要先重新评估：共享数据的分发方式是否还成立。\n" +
				"（kstate 持有 plugin.Registry 是为了让模块间共享插件注册表；\n" +
				" 改了这个前提，整套注入路径都要重新设计。）")
		}
	}
}

// ─────────────────── ⑤ 插件声明式提示词段的唯一来源 ───────────────────

// TestPromptSectionsOnlyFromKstate 钉住内核只从挂载点读插件段。
//
// 变红条件：agent/core 里出现 a.promptSections（字段读取）⇒ 红。
func TestPromptSectionsOnlyFromKstate(t *testing.T) {
	src, err := os.ReadFile("../../agent/core/promptsections.go")
	if err != nil {
		t.Fatalf("读 promptsections.go: %v", err)
	}
	text := string(src)
	if !strings.Contains(text, "kstate.Get()") {
		t.Fatal("★ renderSystemPrompt 不再从 kstate 读插件段 ——\n" +
			"那意味着它退回了 Agent 字段（子 agent 拿不到）。")
	}
	if strings.Contains(text, "a.promptSections") {
		t.Fatal("★ 仍在读 a.promptSections 字段 —— 应改为 kstate.Get().PromptSections")
	}
}

// ─────────────────── 工具 ───────────────────

// assertNoDep 断言 pkg 的**传递依赖**里没有 forbidden。
//
// 用 `go list -deps` 而不是源码扫描：传递依赖能抓到间接环
// （源码扫描只能看到直接 import）。
func assertNoDep(t *testing.T, pkg, forbidden string) {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s 失败: %v\n%s", pkg, err, stderr)
	}
	for _, d := range strings.Fields(string(out)) {
		if d == forbidden {
			t.Fatalf("★ %s 依赖了 %s\n\n"+
				"这条依赖会形成环，让被依赖方无法反向读统一挂载点 ——\n"+
				"那正是「共享数据靠手工接线」的来源。\n\n"+
				"正确做法：共享数据挂在 kstate.State 上，需要它的包 import kstate。\n"+
				"（若被依赖方**就是**挂载点持有的类型，则它不能反过来读挂载点 ——\n"+
				"  那种情况用构造期显式注入，见 TestPluginSettersAreInjection。）",
				pkg, forbidden)
		}
	}
}
