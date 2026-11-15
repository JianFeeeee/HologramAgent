package test

// internal/plugin/proc 的**扩容块生命周期**判据 —— 面向接口的黑盒。
//
// ## 被测的职责
//
// 变长结果放不进内核标定的调用帧时，插件向内核申请一块**扩容块**
// （引用上打 sharedRefFlagExpand），内核读完必须**代为归还**。
//
// 这条职责此前完全没有判据（`todo.md` §3 P2 记录：`grep expandFlag` = 0）。
// 它是真实的资金损失路径：`plugin.freeExpand` 的历史注释写着
// 「不该用 `_ =` 吞掉错误」—— 2026-10-25 线上 4MB arena 被打满的根因就是
// 归还失败被吞。没有判据 ⇒ 同一类缺陷可以无声回归。
//
// ## 为什么判据只能用形态学断言
//
// 可观测出口只有 `Arena().Stats()`（used 字节数）与调用结果本身。
// 块布局、块头字段、表项下标、扩容阈值都不在断言范围内，
// 断言只问两件事：**结果对不对**、**内存还回去了没有**。
//
// ## 为什么必须用真实模板编译插件
//
// 扩容块的申请方是**插件侧模板**（`hmapdev/templates/proc_main.go.tmpl`
// 的 `arenaPut`）。手写一个假 RPC 插件来打标志位，测的是我自己对协议的
// 复述，不是两侧真实对齐 —— 旧实现里那个 e2e 判据（`53cdc7f` 删除）正是
// 用真实模板，本判据沿用同一做法。

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// ★ 内核标定的结果预算（协议常量 toolResultBudget）是 64KB。
// 判据取 200KB，远超预算 ⇒ 必然走申请扩容块的分支，而不是帧内结果区。
const expandPayloadBytes = 200 * 1024

// pluginExpander 生成一个返回超大工具结果的插件源码。
//
// 200KB 的字符串既超出 64KB 结果预算，也远超 4096 字节，
// 无法退回任何"内联"捷径。
func pluginExpander(toolName string) string {
	return `package main

import (
	"strings"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type expander struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &expander{name: name}, nil
}

func (p *expander) Name() string   { return p.name }
func (p *expander) Stop() error    { return nil }

func (p *expander) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("` + toolName + `", sdk.ToolDef{
		Description: "返回超出内核结果预算的大结果，强制走扩容块",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (interface{}, error) {
		return strings.Repeat("x", ` + "200*1024" + `), nil
	})
	return nil
}
`
}

// pluginExpandingCleaner 生成一个 Cleaner 返回超大文本的插件。
//
// Cleaner 的往返同样用 funccall 帧（输入段 + 64KB 结果预算），
// 因此它是一条**独立**的扩容块路径（`corehandler.invokeCleanerText`），
// 与工具调用的归还点不共用代码。
func pluginExpandingCleaner(toolName string) string {
	return `package main

import (
	"strings"

	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type cleaner struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &cleaner{name: name}, nil
}

func (p *cleaner) Name() string { return p.name }
func (p *cleaner) Stop() error  { return nil }

func (p *cleaner) Start(s *sdk.PluginSDK) error {
	s.RegisterTool("` + toolName + `", sdk.ToolDef{
		Description: "带超大 Cleaner 结果的工具",
		Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		Cleaner: func(in string) string {
			return strings.Repeat("C", ` + "300*1024" + `)
		},
	}, func(args map[string]interface{}) (interface{}, error) {
		return "ok", nil
	})
	return nil
}
`
}

// ---- 判据 1：工具结果走扩容块，内核归还 ----

func TestExpansionBlockReclaimedAfterToolCall(t *testing.T) {
	host := newExpandHost(t)
	core := newRecordingCore()
	p := startExpandPlugin(t, host, core, pluginExpander("big_result"))
	defer func() { _ = p.Close() }()

	call, ok := core.tool("big_result")
	if !ok {
		t.Fatal("插件应注册 big_result")
	}

	for i := 0; i < 5; i++ {
		res, err := call(map[string]interface{}{})
		if err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
		s, _ := res.(string)
		if len(s) != expandPayloadBytes {
			t.Fatalf("第 %d 次结果 %d 字节，期望 %d（内容被截断说明读错了块）",
				i+1, len(s), expandPayloadBytes)
		}
		// ★ 核心断言：每次调用之后 arena 必须回到 0。
		//
		// 不归还的后果不是"报错"，而是**静默涨**：默认 arena 只有 4MB，
		// 每次泄漏 200KB ⇒ 第 20 次调用时所有插件一起失灵，
		// 且日志里只有归还失败那一行（连这一行都可能是 `_ =` 吞掉的）。
		if used, total := host.Arena().Stats(); used != 0 {
			t.Fatalf("第 %d 次调用后 arena used=%d/%d —— 扩容块未归还（泄漏）",
				i+1, used, total)
		}
	}
}

// ---- 判据 2：Cleaner 结果走扩容块，内核归还 ----

func TestExpansionBlockReclaimedAfterCleanerCall(t *testing.T) {
	host := newExpandHost(t)
	core := newRecordingCore()
	p := startExpandPlugin(t, host, core, pluginExpandingCleaner("cleaned"))
	defer func() { _ = p.Close() }()

	def, ok := core.toolDef("cleaned")
	if !ok {
		t.Fatal("插件应注册 cleaned")
	}
	// Cleaner 是函数型字段（json:"-"），必须跨进程**接回来**才可能被调用。
	// 接不回来时这里为 nil —— 那本身就是缺陷，不能跳过。
	if def.Cleaner == nil {
		t.Fatal("插件声明的 Cleaner 没有跨进程接回内核")
	}

	for i := 0; i < 3; i++ {
		out := def.Cleaner("原文")
		if len(out) != 300*1024 {
			t.Fatalf("第 %d 次 Cleaner 返回 %d 字节，期望 %d",
				i+1, len(out), 300*1024)
		}
		if !strings.HasPrefix(out, "C") {
			t.Fatalf("第 %d 次 Cleaner 返回内容不符: 前 20 字节 %q", i+1, out[:20])
		}
		// Cleaner 的扩容块归还点与工具调用**不共用代码**（
		// corehandler.invokeCleanerText 自己 defer），所以要单独钉。
		if used, total := host.Arena().Stats(); used != 0 {
			t.Fatalf("第 %d 次 Cleaner 调用后 arena used=%d/%d —— 扩容块未归还",
				i+1, used, total)
		}
	}
}

// ---- 判据 3：普通大小的结果**不应**申请扩容块 ----

func TestSmallResultDoesNotConsumeArena(t *testing.T) {
	host := newExpandHost(t)
	core := newRecordingCore()
	p := startExpandPlugin(t, host, core, pluginExpander("big_result"))
	defer func() { _ = p.Close() }()

	call, ok := core.tool("big_result")
	if !ok {
		t.Fatal("插件应注册 big_result")
	}
	if _, err := call(map[string]interface{}{}); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 扩容块是对**超出预算**的结果才申请的补偿机制，不是常态路径。
	// 每次调用都留一块的话，上面的归还判据会被"反正都还了"掩盖住真正的浪费。
	if used, _ := host.Arena().Stats(); used != 0 {
		t.Fatalf("一次普通调用后 arena used=%d —— 调用帧本身也不该留下残块", used)
	}
	// 连调 20 次不涨：帧是每次分配再释放的，不该累计。
	for i := 0; i < 20; i++ {
		if _, err := call(map[string]interface{}{}); err != nil {
			t.Fatalf("第 %d 次调用失败: %v", i+1, err)
		}
	}
	if used, total := host.Arena().Stats(); used != 0 {
		t.Fatalf("连调 20 次后 arena used=%d/%d —— 调用帧泄漏", used, total)
	}
}

// ---- 测试支撑（只用导出 API）----

func newExpandHost(t *testing.T) *proc.Host {
	t.Helper()
	h, err := proc.NewHost()
	if err != nil {
		t.Skipf("共享内存不可用：%v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func startExpandPlugin(t *testing.T, host *proc.Host, core *recordingCore, src string) *proc.Plugin {
	t.Helper()
	bin := buildTemplatePlugin(t, src)
	// onCrash 传 nil：崩溃判定不属于本判据的职责。
	p := proc.New("expand-probe", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("插件启动失败（握手/init 阶段）: %v", err)
	}
	return p
}

// recordingCore 是 proc.CoreSDK 的黑盒实现：只记录插件反向注册了什么。
//
// proc.CoreSDK 是本模块的**公开 SPI**（子进程插件可见的能力面），
// 因此在本包外实现它不越界；其余能力返回 nil，与"内核没接这条能力"一致。
type recordingCore struct {
	tools    map[string]pubsdk.ToolHandler
	toolDefs map[string]pubsdk.ToolDef
}

func newRecordingCore() *recordingCore {
	return &recordingCore{
		tools:    map[string]pubsdk.ToolHandler{},
		toolDefs: map[string]pubsdk.ToolDef{},
	}
}

func (c *recordingCore) tool(name string) (pubsdk.ToolHandler, bool) {
	h, ok := c.tools[name]
	return h, ok
}

func (c *recordingCore) toolDef(name string) (pubsdk.ToolDef, bool) {
	d, ok := c.toolDefs[name]
	return d, ok
}

func (c *recordingCore) PluginName() string                  { return "expand-probe" }
func (c *recordingCore) Settings() pubsdk.SettingsAPI        { return nil }
func (c *recordingCore) Secrets() pubsdk.SecretsAPI          { return nil }
func (c *recordingCore) Security() pubsdk.SecurityAPI        { return nil }
func (c *recordingCore) Memory() pubsdk.MemoryAPI            { return nil }
func (c *recordingCore) TextMemory() pubsdk.TextMemoryAPI    { return nil }
func (c *recordingCore) DocMemory() pubsdk.DocMemoryAPI      { return nil }
func (c *recordingCore) Knowledge() pubsdk.KnowledgeAPI      { return nil }
func (c *recordingCore) LLM() pubsdk.LLMAPI                  { return nil }
func (c *recordingCore) Social() pubsdk.SocialAPI            { return nil }
func (c *recordingCore) PluginMgr() pubsdk.PluginMgrAPI      { return nil }
func (c *recordingCore) RegisterPluginAPI(name string) error { return nil }
func (c *recordingCore) RegisterStage(s pubsdk.Stage, h pubsdk.StageHandler, sc ...pubsdk.StageScope) {
}
func (c *recordingCore) RegisterInputChannel(name string, def pubsdk.ChannelDef) error {
	return nil
}
func (c *recordingCore) RegisterOutputChannel(name string, caps int, desc string,
	def pubsdk.ChannelDef, handler pubsdk.ToolHandler) error {
	return nil
}

func (c *recordingCore) RegisterTool(name string, def pubsdk.ToolDef, handler pubsdk.ToolHandler) error {
	c.tools[name] = handler
	c.toolDefs[name] = def
	return nil
}

func (c *recordingCore) InjectText(source, channel, text string)             {}
func (c *recordingCore) InjectInterruptText(source, channel, text string)    {}
func (c *recordingCore) InjectTextNoMemory(source, channel, text string)     {}
func (c *recordingCore) InjectInputSync(source, channel, text string) string { return "" }
func (c *recordingCore) InjectInputMedia(source, channel, text string, b []pubsdk.ContentBlock) {
}
func (c *recordingCore) InjectInputMediaSync(source, channel, text string,
	b []pubsdk.ContentBlock) string {
	return ""
}
func (c *recordingCore) InjectInterruptMedia(source, channel, text string, b []pubsdk.ContentBlock) {
}
func (c *recordingCore) InjectTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
}
func (c *recordingCore) InjectInterruptTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
}
func (c *recordingCore) InjectInputSyncOpts(source, channel, text string,
	opts pubsdk.InjectOptions) string {
	return ""
}
func (c *recordingCore) InjectInputMediaOpts(source, channel, text string,
	b []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
}
func (c *recordingCore) InjectInputMediaSyncOpts(source, channel, text string,
	b []pubsdk.ContentBlock, opts pubsdk.InjectOptions) string {
	return ""
}
func (c *recordingCore) InjectInterruptMediaOpts(source, channel, text string,
	b []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
}
func (c *recordingCore) SetToolBlocks(blocks []pubsdk.ContentBlock) {}
func (c *recordingCore) SetAutoRestart(enabled bool)                {}

// buildTemplatePlugin 用 **hmapdev 的真实 proc 运行时模板**编译一个插件二进制。
//
// ## 模板从哪来（两份来源，缺一不可）
//
//	hmapdev 工具链整个 `tools/` 目录属于 SDK 仓（`.gitignore:33`），
//	本仓**不追踪** —— 所以主仓 CI 的干净检出里没有它：
//
//	    工作机：third_party/hologramagentsdk/tools/hmapdev/templates/*.tmpl   （完整 SDK 仓）
//	    CI    ：**不存在**
//
//	第一版只读 SDK 路径 ⇒ CI 上三条判据全部 SKIP、恒绿、什么也没测。
//	实测（干净 worktree 检出）：`--- SKIP: TestExpansionBlockReclaimedAfterToolCall`。
//	一条会自认为"通过"的判据比没有更糟：它让人相信扩容块路径已被守住。
//
//	因此把 proc 运行时那三份模板**拷进本仓**作为 CI 用的夹具：
//
//	    internal/plugin/proc/testdata/hmapdev/proc_main.go.tmpl        （74KB）
//	    internal/plugin/proc/testdata/hmapdev/proc_shm_unix.go.tmpl
//	    internal/plugin/proc/testdata/hmapdev/proc_shm_windows.go.tmpl
//
// ## 两份来源为什么还要**比对**
//
//	夹具是副本，会腐坏：SDK 改了模板而这里没刷 ⇒ CI 测的是**上一代协议**，
//	而工作机上照样绿。所以两边同时存在时逐字节比对，不一致就**响**，
//	并在错误里直接给出刷新命令。只在本仓有夹具时（CI）才跳过比对。
//
//	模板只 import `github.com/JianFeeeee/hologramagentsdk/sdk`（已 vendor 的
//	22 个文件里就有）与 stdlib，所以夹具自带即可编译，不依赖 SDK 仓其余部分。
func buildTemplatePlugin(t *testing.T, businessCode string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("环境无 go 工具链，跳过（本判据需要真实模板编译）: %v", err)
	}

	// 平台模板与 proc 运行时的平台相关部分一一对应，缺一个都编不出来。
	runtimeTemplates := []string{"proc_main.go.tmpl"}
	if runtime.GOOS == "windows" {
		runtimeTemplates = append(runtimeTemplates, "proc_shm_windows.go.tmpl")
	} else {
		runtimeTemplates = append(runtimeTemplates, "proc_shm_unix.go.tmpl")
	}

	vendoredDir := filepath.Join("..", "testdata", "hmapdev")
	sdkDir := filepath.Join("..", "..", "..", "..",
		"third_party", "hologramagentsdk", "tools", "hmapdev", "templates")

	dir := t.TempDir()
	pickTemplate := func(name string) {
		t.Helper()
		// 优先 SDK 原版：工作机上它是最新鲜的，能让本判据跟着模板演进。
		sdkPath := filepath.Join(sdkDir, name)
		live, err := os.ReadFile(sdkPath)
		if err == nil {
			copied, cerr := os.ReadFile(filepath.Join(vendoredDir, name))
			if cerr == nil && !bytes.Equal(live, copied) {
				t.Fatalf("★ 本仓的模板夹具已过期：%s\n"+
					"  SDK 原版（%d 字节）与本仓副本（%d 字节）不一致。\n"+
					"  刷新命令：\n"+
					"    cp %s internal/plugin/proc/testdata/hmapdev/\n"+
					"  不刷新则 CI 测的是上一代协议，而工作机照样绿。",
					name, len(live), len(copied), sdkPath)
			}
			writeOrFail(t, filepath.Join(dir, templateOutputName(name)), string(live))
			return
		}
		// CI：SDK 仓不在，用本仓夹具。
		copied, cerr := os.ReadFile(filepath.Join(vendoredDir, name))
		if cerr != nil {
			t.Fatalf("模板 %s 既不在 SDK 仓也不在本仓夹具里（两处都缺，判据无法运行）: %v",
				name, cerr)
		}
		writeOrFail(t, filepath.Join(dir, templateOutputName(name)), string(copied))
	}
	for _, tmpl := range runtimeTemplates {
		pickTemplate(tmpl)
	}

	writeOrFail(t, filepath.Join(dir, "plugin.go"), businessCode)

	sdkPath, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "third_party", "hologramagentsdk"))
	if err != nil {
		t.Fatalf("解析 SDK 路径: %v", err)
	}
	writeOrFail(t, filepath.Join(dir, "go.mod"),
		"module expandprobe\n\ngo 1.27.1\n\n"+
			"require github.com/JianFeeeee/hologramagentsdk v0.9.2\n\n"+
			"replace github.com/JianFeeeee/hologramagentsdk => "+sdkPath+"\n")

	bin := filepath.Join(dir, "plugin.bin")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	// CGO_ENABLED=0：模板零 cgo 是迁移的核心收益，这里同时充当回归保护。
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("用真实模板编译插件失败: %v\n%s", err, out)
	}
	return bin
}

// templateOutputName 把模板文件名映射成生成文件名（与 hmapdev 一致）。
func templateOutputName(tmpl string) string {
	switch tmpl {
	case "proc_main.go.tmpl":
		return "z_proc_gen.go"
	case "proc_shm_unix.go.tmpl":
		return "z_proc_shm_unix.go"
	case "proc_shm_windows.go.tmpl":
		return "z_proc_shm_windows.go"
	}
	return "z_" + tmpl
}

func writeOrFail(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}
