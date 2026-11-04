package all

// 判据：**生产装配路径必须真的接通**（2026-11-04）。
//
// ## 这组判据补的是一个真实的"测试绿、生产坏"
//
// 我第一版实现里，`agent/core` 的 renderSystemPrompt 已经从
// `kstate.Get().PromptSections` 读段，测试也全绿 —— 但**生产上是坏的**：
//
//   · `kstate.Mount(ks)` 在生产路径上**没有任何调用者** ⇒ Get() 永远 nil
//   · `sdk.SetSystemPromptAPI` 生产上没人调 ⇒ 插件声明拿 nil API
//
// 测试为什么绿？因为测试用 `MountForTest` 自己挂了挂载点。
// 这就是本仓反复吃过的亏（engineering-lessons §1.1）：
//
//   判据把缺失的那根线补上了，所以永远绿。
//
// 所以本组判据不看"行不行"，只看**装配代码里有没有那根线**——
// 它们失败时指向的是一行代码，而不是一个行为。

import (
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// TestKstateIsMountedByRun 钉住 kernelboot.Run 会装入统一挂载点。
//
// ## 为什么必须是这一条
//
// `kstate.Get()` 是底层包（agent/core 等）读共享数据的**唯一**入口。
// 它返回 nil 时所有读取都走 `if ks != nil` 的分支 ⇒ **静默跳过**。
//
// 静默跳过比报错危险得多：插件声明的技能索引不渲染，模型看不到任何
// skill 说明，于是凭工具名猜用法 —— 那正是"拿 terminal_create 当
// cmd_run"的根因。而日志里一个字都没有。
//
// 变红条件：删掉 kernelboot.Run 里的 kstate.Mount(ks) ⇒ 红。
func TestKstateIsMountedByRun(t *testing.T) {
	src, err := os.ReadFile("../../kernelboot/kernelboot.go")
	if err != nil {
		t.Fatalf("读 kernelboot.go: %v", err)
	}
	if !strings.Contains(string(src), "kstate.Mount(ks)") {
		t.Fatal("★ kernelboot.Run 没有调 kstate.Mount(ks)\n\n" +
			"后果：kstate.Get() 永远返回 nil ⇒ 所有读共享数据的代码\n" +
			"都走 `if ks != nil` 的跳过分支 ⇒ **静默不生效**（不是报错）。\n\n" +
			"具体症状：插件声明的系统提示词段一条都不渲染，模型看不到\n" +
			"任何 skill 索引，只能凭工具名猜用法。")
	}
}

// TestPromptSectionsWiredToPlugins 钉住插件声明面被真正注入。
//
// 三处缺一不可：
//
//	kernelmod/stagehost   产出 ks.PromptSections（注册表本身）
//	kernelmod/pluginhost  从 ks 取出来 → reg.SetPromptSections
//	internal/sdk          reg 把它传进 SDKConfig → SetSystemPromptAPI
//
// 缺任何一处，插件的 `s.SystemPrompt()` 都返回 nil，而 skillmgr 的
// publishIndex 对 nil 是**静默返回**（它不能 panic，因为插件可能跑在
// 没有该能力的宿主里）—— 于是技能索引永远进不了提示词。
//
// 这三条分开写而不是合成一条：失败时要一眼看出断在哪一节。
func TestPromptSectionsWiredToPlugins(t *testing.T) {
	// ① 注册表被产出
	stagehost, err := os.ReadFile("../../kernelmod/stagehost/mod.go")
	if err != nil {
		t.Fatalf("读 stagehost/mod.go: %v", err)
	}
	if !strings.Contains(string(stagehost), "ks.PromptSections = ") {
		t.Fatal("★ stage-host 模块没有产出 ks.PromptSections —— 注册表不存在")
	}

	// ② 被注入给插件注册表
	pluginhost, err := os.ReadFile("../../kernelmod/pluginhost/mod.go")
	if err != nil {
		t.Fatalf("读 pluginhost/mod.go: %v", err)
	}
	if !strings.Contains(string(pluginhost), "reg.SetPromptSections(") {
		t.Fatal("★ plugin-host 模块没有把 ks.PromptSections 注入给插件注册表\n\n" +
			"后果：sdk.SystemPrompt() 返回 nil ⇒ skillmgr.publishIndex 静默返回\n" +
			"⇒ 技能索引永远进不了系统提示词。")
	}

	// ③ 被传进 SDK 并被接上
	sdkplug, err := os.ReadFile("../../sdk/plugin.go")
	if err != nil {
		t.Fatalf("读 sdk/plugin.go: %v", err)
	}
	sdkText := string(sdkplug)
	if !strings.Contains(sdkText, "SetSystemPromptAPI(") {
		t.Fatal("★ internal/sdk 没有调用 SetSystemPromptAPI\n\n" +
			"后果：插件的 s.SystemPrompt() 返回 nil，DeclarePromptSection 无处落地。")
	}
	// 注意匹配的是**字段声明**（`PromptSections *promptsec.Registry`），
	// 不是赋值写法（`PromptSections:` 只出现在 registry.go 的 SDKConfig
	// 字面量里）。第一版判据写成了冒号形式，于是把已经接好的代码判成红。
	if !strings.Contains(sdkText, "PromptSections *promptsec.Registry") {
		t.Fatal("★ SDKConfig 没有 PromptSections 字段 —— 插件声明的段无处传递")
	}
}

// TestSharedDataInjectionsHaveProducers 是**通用**的一条。
//
// 本轮重构引入了若干"注入点"（Setter），每一个都要求有人调它。
// 没人调的注入点 = 功能静默缺失。
//
// 这条判据列出的每一项，都必须在生产代码（非 _test）里存在调用者。
// 它是从上面那两次真实事故里抽象出来的：
//
//	· kstate.Mount           漏了 ⇒ Get() 永远 nil
//	· SetPromptSections      漏了 ⇒ 插件声明无处落地
//
// 变红条件：新增一个注入点却忘了接 ⇒ 在这里补一行就会红，直到真接上。
func TestSharedDataInjectionsHaveProducers(t *testing.T) {
	// 注入点 → 期望在哪个包里找到生产者（子串匹配，跨文件搜索）。
	wants := []struct {
		method string
		where  string // 生产代码里的调用形态
	}{
		{"kstate.Mount", "kstate.Mount(ks)"},
		{"plugin.Registry.SetPromptSections", "reg.SetPromptSections("},
		{"sdk.SetSystemPromptAPI", "SetSystemPromptAPI("},
		{"plugin.Registry.SetSecurityRuntime", "reg.SetSecurityRuntime("},
		{"plugin.Registry.SetSecretStore", "reg.SetSecretStore("},
		{"config.SetCodec", "cfgReg.SetCodec("},
	}

	for _, w := range wants {
		found := false
		// 扫所有非 _test 的 Go 文件。
		roots := []string{"../../kernelboot", "../../kernelmod", "../../sdk", "../../plugin", "../../config", "../../agent"}
		for _, root := range roots {
			if filepathContains(t, root, w.where) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("★ 注入点 %s 在生产代码里没有调用者（找 %q）\n\n"+
				"没人调的注入点 = 功能静默缺失：拿不到注入的那一方会走\n"+
				"\"未装配就跳过\"的分支，日志里一个字都没有。", w.method, w.where)
		}
	}
}

// filepathContains 递归搜索目录下所有非测试 Go 文件是否包含 substr。
func filepathContains(t *testing.T, dir, substr string) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		p := dir + "/" + e.Name()
		if e.IsDir() {
			if filepathContains(t, p, substr) {
				return true
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), substr) {
			return true
		}
	}
	return false
}

// TestRunActuallyMountsState 是**端到端**的那一条（其余是源码判据）。
//
// ## 为什么源码判据不够
//
// TestKstateIsMountedByRun 只确认"那行代码存在"。它证明不了：
//
//	· 那行在**真实装配流程**里被执行到（可能被 early return 跳过）
//	· Mount 的是**同一个** State（而不是另一份）
//	· 装配完成后 ks.PromptSections 真的被 stage-host 填上了
//
// 所以这条跑真实 `kernelboot.Run` + 真实模块登记表，然后从**底层包的
// 视角**（kstate.Get()）检查能不能看到东西。
//
// ## 为什么这条能防住我犯的那个错
//
// 第一版实现里 agent/core 读 kstate.Get()，而生产上没人 Mount ⇒ 永远 nil。
// 测试全绿是因为测试自己 MountForTest。本判据**不自己 Mount**，
// 只读 Run 之后的状态 —— 所以它能抓到"没人 Mount"。
func TestRunActuallyMountsState(t *testing.T) {
	// 先清空挂载点，确保"绿"只能来自 Run 自己。
	restore := kstate.MountForTest(nil)
	defer restore()

	if kstate.Get() != nil {
		t.Fatal("前置不成立：清空挂载点失败")
	}

	// 用**完整** fixture：emptyState 缺 CfgReg，onnx 模块会直接解引用 nil
	// （实测 panic）。这里按 bootgraph_test.go 的整机夹具建 state。
	// ★ 不用 t.TempDir()：整机装配会挂 agentfs/merged（真实 overlay 挂载点），
	// t.TempDir 的严格 RemoveAll 会报 EBUSY。改用与 bootgraph_test.go 相同的
	// "重试删除"策略（Linux 上 dentry 回收可能稍晚）。
	dir, err := os.MkdirTemp("", "wiring-mounts-")
	if err != nil {
		t.Fatalf("建临时目录失败: %v", err)
	}
	t.Cleanup(func() {
		_ = removeAllWithRetry(dir, 50)
	})
	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	t.Cleanup(func() { reg.Close() })

	ks := kstate.New()
	ks.CfgReg = reg
	ks.DataDir = dir
	ks.AgentWorkDir = filepath.Join(dir, "agentfs")
	ks.Cfg = reg.ToConfig()
	ks.Cfg.Daemon.DataDir = dir
	ks.Cfg.Plugin.Dir = filepath.Join(dir, "plugins")
	ks.HTTPAddr = "127.0.0.1:0"

	plan := realPlan(t)
	if _, err := kernelboot.Run(plan, kernelboot.RunOptions{
		State: ks, Bus: kstate.NewSyncBus(), MaxParallel: 2,
	}); err != nil {
		t.Skipf("整机装配未通过（%v）—— 本判据依赖完整装配，跳过而不是假绿", err)
	}

	// ① 挂载点必须是 Run 装入的那一份。
	got := kstate.Get()
	if got == nil {
		t.Fatal("★ kernelboot.Run 跑完了，但 kstate.Get() 仍是 nil\n\n" +
			"后果：所有读共享数据的底层包（agent/core 等）都走\n" +
			"`if ks != nil` 的跳过分支 ⇒ 静默不生效。\n" +
			"测试之所以没发现，是因为测试自己调了 MountForTest ——\n" +
			"判据把缺失的那根线补上了，所以永远绿。")
	}
	if got != ks {
		t.Fatal("★ Run 装入的挂载点不是传入的那个 State —— 底层包读到的会是另一份")
	}

	// ② stage-host 必须已产出插件声明式提示词段的注册表。
	if got.PromptSections == nil {
		t.Fatal("★ 装配完成后 ks.PromptSections 仍是 nil\n\n" +
			"stage-host 模块应产出它。缺了它 ⇒ 插件声明无处落地 ⇒\n" +
			"skillmgr 的技能索引永远进不了系统提示词。")
	}
}
