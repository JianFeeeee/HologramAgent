package test

// internal/plugin/proc 的**能力阶梯**判据 —— 面向接口的黑盒。
//
// ## 被测的职责
//
// 子进程插件拿到的是**收窄后的能力面**（proc.CoreSDK）。manifest 里
// 声明了哪些能力组，内核就在 RPC 边界按组放行；未声明的组必须被**明确拒绝**，
// 而且要拒绝得可操作（说清缺什么、在哪声明）。
//
// 这套机制此前 0 判据：`53cdc7f` 删掉的 `capability_test.go`（334 行 8 条，
// 含自注「本文件最重要的测试」的 `TestCapability_AllMethodsClassified`）
// 一直没补回来。而它守的是一个**安全边界**：
//
//	capabilityOf 对未登记的 method 返回 (CapCore, false)，
//	allows 见到 !registered 就**放行** —— 也就是说，
//	新增一个 method 忘了登记，它会按最宽松的基础能力通过。
//
// 没有判据 ⇒ 这个漏洞可以无声地一次次被引入。
//
// ## 为什么用「插件真的调一遍」而不是直接问能力表
//
// 直接读 `methodCapability`（私有 map）属于读实现细节，判据会随重构而
// 无意义地红；更重要的是它测不到**边界是否真的拦住了**。
// 这里让一个真插件从 SDK 访问器发起调用，看它实际收到什么 —— 那才是
// 插件作者会遇到的事实。
//
// ## 判据的输入从哪来
//
// 能力组与 SDK 访问器一一对应（公开 SDK 的契约，不是内核实现细节）：
//
//	Memory()      → memory          DocMemory()  → doc_memory
//	Knowledge()   → knowledge       TextMemory() → text_memory
//	LLM()         → llm             Social()     → social
//	PluginMgr()   → plugin_mgr      Settings()   → settings_cross（SetCore 等）
//	IO()          → io
//
// 刻意排除两组，理由在用例里逐条注明：
//
//	events        —— 模板未接 EventSubscriber（`s.Events()` 恒为 nil，
//	                 调用会 panic 在 RPC 之前，测不到边界）
//	kernel_control —— 模板未接 KernelControlAPI（同上）；且 protocol.go
//	                 登记了 kernel.restart/shutdown 但 corehandler 里
//	                 **没有任何 case**（见 todo.md §8 第 1 条），这条
//	                 渠道当前根本不通，用判据钉它只会钉出一个必然失败的断言。
//	io            —— 由**另一个判据文件**覆盖（`io_report_contract_test.go`）。
//	                 理由：IOInjector 的方法签名由公开 SDK 固定为无返回值 /
//	                 返回 string，被拒时**拿不到 error**，只能观测 stderr。
//	                 那是另一条可观测通道，用另一组判据更贴切：
//	                 本文件按「被拒是否发生」判，那边按「失败是否有声音 +
//	                 是否点名方法/通道 + 正常时是否安静」判。
//	                 ★ 两组都源于同一个实测缺陷（模板 `_, _ =` 吞掉错误）。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// capabilityProbeGroup 描述一组能力与其 SDK 访问器的探针。
//
// tool 是插件侧注册的工具名，插件在该工具里调用组的代表方法并把
// 返回的错误文本原样吐回来 —— 判据据此判断「被拒」还是「走到了后端」。
type capabilityProbeGroup struct {
	capability string // manifest 里要声明的能力名
	tool       string // 插件注册的探针工具名
	what       string // 代表方法的人话描述，出错时便于定位
}

// probedGroups 是**可探针**的非 core 能力组。
//
// 顺序即判据遍历顺序，保持稳定便于读失败输出。
var probedGroups = []capabilityProbeGroup{
	{"memory", "probe_memory", "Memory().Commit"},
	{"doc_memory", "probe_doc_memory", "DocMemory().Insert"},
	{"knowledge", "probe_knowledge", "Knowledge().Add"},
	{"text_memory", "probe_text_memory", "TextMemory().Append"},
	{"llm", "probe_llm", "LLM().SetSource"},
	{"social", "probe_social", "Social().GetPerson"},
	{"plugin_mgr", "probe_plugin_mgr", "PluginMgr().ReloadOne"},
	{"settings_cross", "probe_settings_cross", "Settings().SetCore"},
}

// ---- 判据 1：未声明该组时，每个组的代表方法都必须被明确拒绝 ----

// TestCapabilityGroupsDenyUndelcaredAccess 是本文件的核心判据。
//
// 它同时守住两件事：
//
//	① 边界真的拦住了（放行 = 越权）
//	② 拒绝里给出的能力名**与期望一致**（登错组同样是缺陷：
//	   声明了 memory 却因为方法被登记成 doc_memory 而调不通，
//	   作者会以为是自己的代码问题）
//
// ★ 变异要点：把某个 method 从 methodCapability 里删掉（模拟"新增 method
// 忘了登记"）⇒ capabilityOf 返回 !registered ⇒ allows 放行 ⇒ 该组探针
// 不再报「被拒」⇒ 本判据当场红。这正是旧 `TestCapability_AllMethodsClassified`
// 守着的东西，而它是从**行为**侧守的，不需要读私有表。
func TestCapabilityGroupsDenyUndeclaredAccess(t *testing.T) {
	// 只声明 io：其余每一组都应被拒。
	results := runCapabilityProbes(t, []string{"io"})

	for _, g := range probedGroups {
		out, ok := results[g.tool]
		if !ok {
			t.Fatalf("%s: 插件未注册探针工具 %s（模板或 SDK 访问器变了？）", g.what, g.tool)
		}
		if !strings.Contains(out, "被拒") {
			t.Fatalf("★ %s 未被拒绝（返回 %q）—— 未声明的能力组被放行，越权",
				g.what, truncate(out, 200))
		}
		// ★ 必须是**完整能力名**，不能只做子串包含：
		// "doc_memory" 里含 "memory"，用 Contains 会把「登错组」放过
		// —— 实测变异（把 memory.commit 登成 CapDocMemory）正是这样漏掉的。
		if !strings.Contains(out, `"`+g.capability+`"`) {
			t.Fatalf("★ %s 被拒但没说是缺 %q 能力（返回 %q）—— 能力名对不上，"+
				"作者按提示声明也调不通", g.what, g.capability, truncate(out, 240))
		}
	}
}

// ---- 判据 2：声明后的组必须能走到后端 ----

// TestCapabilityDeclarationReachesBackend：「声明了」不等于「调得通」，
// 但这两件事必须分开可观测。
//
// 判据只看**错误类别**：被拒说明没走过闸门；后端不可用/成功说明走过了。
// 注意 recordingCore 各后端都返回 nil 错误（见 capableCore），所以
// 正常路径应拿到 "OK"，而被拒路径拿到 "被拒"。
func TestCapabilityDeclarationReachesBackend(t *testing.T) {
	declared := make([]string, 0, len(probedGroups))
	for _, g := range probedGroups {
		declared = append(declared, g.capability)
	}
	results := runCapabilityProbes(t, declared)

	for _, g := range probedGroups {
		out, ok := results[g.tool]
		if !ok {
			t.Fatalf("%s: 插件未注册探针工具 %s", g.what, g.tool)
		}
		if strings.Contains(out, "被拒") {
			t.Fatalf("★ 已声明 %q 却仍被拒（返回 %q）—— 声明没生效",
				g.capability, truncate(out, 240))
		}
		if out != "OK" {
			t.Fatalf("%s: 期望走到后端并返回 OK，实际 %q", g.what, truncate(out, 240))
		}
	}
}

// ---- 判据 3：省略声明 = 不受限（存量插件兼容）----

// TestEmptyCapabilityDeclarationIsUnrestricted 钉住一条硬约束：
//
// 17 个存量插件的 plugin.json 都没有 capabilities 字段。若「空声明」被
// 当成最小权限，它们会全部失去 IO 注入/记忆读写而**静默降级**。
// 这是「外部插件零改动」的硬约束，不是可调的策略。
func TestEmptyCapabilityDeclarationIsUnrestricted(t *testing.T) {
	results := runCapabilityProbes(t, nil)

	for _, g := range probedGroups {
		out, ok := results[g.tool]
		if !ok {
			t.Fatalf("%s: 插件未注册探针工具 %s", g.what, g.tool)
		}
		if strings.Contains(out, "被拒") {
			t.Fatalf("★ 未声明 capabilities 却被拒（%s 返回 %q）—— "+
				"存量插件会静默降级", g.what, truncate(out, 240))
		}
	}
}

// ---- 判据 4：拼错的能力名不得意外授权 ----

// TestUnknownCapabilityNameGrantsNothing：能力名是手写的 manifest 字段，
// 拼错是常态。这里刻意混了三类：
//
//	"memroy" / "doc-memory" / "plugin-mgr"  —— 字符被换位/分隔符写错
//	"memor" / "plugin"                       —— **真名的不完整前缀**
//
// ★ 第二类不是凑数：一个"前缀模糊匹配"的实现（`HasPrefix(known, name)`）
// 会对它们全部放行，而前一类一个都不会命中 —— 只用前一类的话，
// 那种实现能悄悄上线而不被本判据发现（实测：变异版本正是如此漏过）。
//
// 两种失效都很糟：
//
//	拼错却按通配放行 ⇒ 一个以为只声明了 memory 的插件拿到了全部权限
//	拼错就静默降级   ⇒ 作者以为声明好了，功能却一直不通
//
// 本判据只钉第一种（越权）；第二种无法从外部区分，属于设计选择。
func TestUnknownCapabilityNameGrantsNothing(t *testing.T) {
	results := runCapabilityProbes(t, []string{
		"memroy", "plugin-mgr", "doc-memory", // 换位 / 分隔符错
		"memor", "plugin", "doc", // 真名的不完整前缀
	})

	for _, g := range probedGroups {
		out, ok := results[g.tool]
		if !ok {
			t.Fatalf("%s: 插件未注册探针工具 %s", g.what, g.tool)
		}
		if !strings.Contains(out, "被拒") {
			t.Fatalf("★ 只声明了拼错的名字，%s 却被放行（%q）—— 拼错等于授权",
				g.what, truncate(out, 200))
		}
	}
}

// ---- 判据 5：拒绝错误必须可操作 ----

// TestCapabilityDenialIsActionable：被拒时必须让作者能自助修复。
//
// 这条针对的是一类真实故障（C ABI 时代 case 23/24 返回成功但永远收不到
// 事件）：插件作者无从得知「这个能力没给我」。
func TestCapabilityDenialIsActionable(t *testing.T) {
	results := runCapabilityProbes(t, []string{"io"})

	out, ok := results["probe_memory"]
	if !ok {
		t.Fatal("插件未注册 probe_memory")
	}
	if !strings.Contains(out, "被拒") {
		t.Fatalf("前置：memory 应被拒，实际 %q", truncate(out, 200))
	}

	// 三要素：缺什么能力、在哪声明、可选值有哪些。
	for _, want := range []string{
		"memory",       // 缺的能力名
		"capabilities", // 声明字段名
		"plugin.json",  // 声明所在文件
		"io",           // 可选能力列表里至少要出现一个真实能力名
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("★ 拒绝错误缺少 %q（作者无法自助修复）：%q",
				want, truncate(out, 300))
		}
	}
}

// ---- 判据 6：公开清单自洽 ----

// TestPublicCapabilityListsAreConsistent：
//
//	KnownCapabilities 不含 core（core 无需声明）、已排序、非空
//	WithheldCapabilities 每项都有理由（否则读代码的人无从判断边界为何在此）
//	判据用例里声明的每一组，都必须在 KnownCapabilities 里出现
//	   —— 否则作者照着拒绝提示也找不到该写什么名字
func TestPublicCapabilityListsAreConsistent(t *testing.T) {
	known := proc.KnownCapabilities()
	if len(known) == 0 {
		t.Fatal("KnownCapabilities 为空")
	}
	if !sortStringsAscending(known) {
		t.Fatalf("KnownCapabilities 未按字典序排列（拒绝错误里会显得杂乱）: %v", known)
	}
	for _, k := range known {
		if k == "core" {
			t.Fatal("KnownCapabilities 不应含 core（core 无需声明，始终可用）")
		}
	}

	knownSet := map[string]bool{}
	for _, k := range known {
		knownSet[k] = true
	}
	for _, g := range probedGroups {
		if !knownSet[g.capability] {
			t.Fatalf("能力 %q 在本判据里被声明，却不在 KnownCapabilities 里 "+
				"—— 作者照着拒绝提示写不出这个名字", g.capability)
		}
	}

	withheld := proc.WithheldCapabilities()
	if len(withheld) == 0 {
		t.Fatal("WithheldCapabilities 为空（那是「刻意不提供」的文档锚点）")
	}
	for name, reason := range withheld {
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("withheld 项 %q 缺少不提供的理由", name)
		}
	}
	// §3.8 明确列为「不提供」的三项。
	for _, name := range []string{"SelftestAPI", "SupervisorAPI", "TrackerAPI"} {
		if _, ok := withheld[name]; !ok {
			t.Fatalf("%s 应在 withheld 清单中（§3.8 明确不提供）", name)
		}
	}
}

// ---- 测试支撑 ----

// runCapabilityProbes 编译并运行一个**声明了指定能力**的探针插件，
// 逐个调用探针工具，返回「工具名 → 插件吐回的字符串」。
//
// 插件侧把 SDK 返回的错误原样回传（`return "ERR:" + err.Error(), nil`），
// 因此判据能区分「被权限拒绝」与「后端不可用」——
// 这两件事在插件作者眼里是完全不同的故障。
func runCapabilityProbes(t *testing.T, declared []string) map[string]string {
	t.Helper()
	host := newExpandHost(t)
	core := newCapableCore()
	bin := buildTemplatePlugin(t, capabilityProbePluginSource())

	p := proc.New("cap-probe", bin, t.TempDir(), nil, host, nil, declared...)
	if err := p.Start(core); err != nil {
		t.Fatalf("插件启动失败（声明 %v）: %v", declared, err)
	}
	t.Cleanup(func() { _ = p.Close() })

	results := map[string]string{}
	for _, g := range probedGroups {
		h, ok := core.tool(g.tool)
		if !ok {
			continue // 由各判据自行报「未注册」
		}
		res, err := h(map[string]interface{}{})
		if err != nil {
			t.Fatalf("%s: 工具调用自身失败: %v", g.what, err)
		}
		s, _ := res.(string)
		results[g.tool] = s
	}
	return results
}

// capabilityProbePluginSource 生成覆盖全部可探针能力组的插件。
//
// 每个工具做**最少一次**代表调用，只关心它是否被拦在门口。
func capabilityProbePluginSource() string {
	return `package main

import (
	sdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

type probe struct{ name string }

func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
	return &probe{name: name}, nil
}

func (p *probe) Name() string { return p.name }
func (p *probe) Stop() error  { return nil }

func (p *probe) Start(s *sdk.PluginSDK) error {
	reg := func(name, desc string, fn func() error) {
		s.RegisterTool(name, sdk.ToolDef{
			Description: desc,
			Parameters:  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
		}, func(args map[string]interface{}) (interface{}, error) {
			if err := fn(); err != nil {
				return "ERR:" + err.Error(), nil
			}
			return "OK", nil
		})
	}

	reg("probe_memory", "Memory().Commit", func() error {
		return s.Memory().Commit([]sdk.Triple{{Subject: "甲", Relation: "是", Object: "人"}})
	})
	reg("probe_doc_memory", "DocMemory().Insert", func() error {
		return s.DocMemory().Insert(&sdk.Doc{Title: "探针", Content: "探针正文"})
	})
	reg("probe_knowledge", "Knowledge().Add", func() error {
		return s.Knowledge().Add("探针条目", "探针正文")
	})
	reg("probe_text_memory", "TextMemory().Append", func() error {
		return s.TextMemory().Append(sdk.TextEvent{Role: "user", Content: "探针事件"})
	})
	reg("probe_llm", "LLM().SetSource", func() error {
		return s.LLM().SetSource("probe")
	})
	reg("probe_social", "Social().GetPerson", func() error {
		_, err := s.Social().GetPerson("探针")
		return err
	})
	reg("probe_plugin_mgr", "PluginMgr().ReloadOne", func() error {
		return s.PluginMgr().ReloadOne("probe")
	})
	reg("probe_settings_cross", "Settings().SetCore", func() error {
		return s.Settings().SetCore("core.probe.key", "probe")
	})
	return nil
}
`
}

// capableCore 是**后端全部可用**的 recordingCore：它让「被权限拒绝」
// 与「后端不可用」能区分开。
//
// 若后端为 nil，被拒绝之外的一切都会返回「能力在当前内核实例中不可用」，
// 判据就无法证明调用**真的走到了闸门之后**——那是另一个失效模式
// （声明生效了但方式不对，例如走到错误的 case 分支）。
type capableCore struct {
	recordingCore
}

func newCapableCore() *capableCore {
	return &capableCore{recordingCore: *newRecordingCore()}
}

type probeMemory struct{}

func (probeMemory) Recall(q []string, d int) ([]pubsdk.Entity, []pubsdk.Relation, error) {
	return nil, nil, nil
}
func (probeMemory) Commit(t []pubsdk.Triple) error                      { return nil }
func (probeMemory) Introspect() (map[string]interface{}, error)         { return nil, nil }
func (probeMemory) MergeEntities(source, target string) (int, error)    { return 0, nil }
func (probeMemory) Purge(c map[string]string, mode string) (int, error) { return 0, nil }

type probeDocMemory struct{}

func (probeDocMemory) Query(text string, topK int) []*pubsdk.Doc { return nil }
func (probeDocMemory) Insert(doc *pubsdk.Doc) error              { return nil }
func (probeDocMemory) InsertWithMedia(doc *pubsdk.Doc, a []pubsdk.MediaAttachment) error {
	return nil
}
func (probeDocMemory) Remove(id string)              {}
func (probeDocMemory) Stats() map[string]interface{} { return nil }

type probeKnowledge struct{}

func (probeKnowledge) Search(q string, topK int) ([]*pubsdk.Knowledge, error) { return nil, nil }
func (probeKnowledge) Add(name, content string) error                         { return nil }
func (probeKnowledge) List() ([]string, error)                                { return nil, nil }

type probeTextMemory struct{}

func (probeTextMemory) Append(evt pubsdk.TextEvent) error { return nil }

type probeLLM struct{}

func (probeLLM) ListSources() []string       { return nil }
func (probeLLM) SetSource(name string) error { return nil }
func (probeLLM) CurrentSource() string       { return "" }

type probeSocial struct{}

func (probeSocial) GetPerson(name string) (*pubsdk.PersonProfile, error)      { return nil, nil }
func (probeSocial) GetTrait(name, trait string) (string, bool)                { return "", false }
func (probeSocial) GetRelations(name string) ([]pubsdk.SocialRelation, error) { return nil, nil }
func (probeSocial) GetNetwork(name string, depth int) ([]*pubsdk.PersonProfile, error) {
	return nil, nil
}
func (probeSocial) ListPersons() ([]string, error) { return nil, nil }

type probePluginMgr struct{}

func (probePluginMgr) ReloadOne(name string) error             { return nil }
func (probePluginMgr) ListLoadedPlugins() []string             { return nil }
func (probePluginMgr) IsPluginDisabled(name string) bool       { return false }
func (probePluginMgr) StopPlugin(name string) error            { return nil }
func (probePluginMgr) StartPlugin(name string) error           { return nil }
func (probePluginMgr) IsPluginRuntimeStopped(name string) bool { return false }

type probeSettings struct{}

func (probeSettings) Get(key string) (interface{}, error)               { return nil, nil }
func (probeSettings) Set(key string, value interface{}) error           { return nil }
func (probeSettings) SetSecret(key string, value interface{}) error     { return nil }
func (probeSettings) GetSecret(key string) (string, error)              { return "", nil }
func (probeSettings) List(prefix string) ([]string, error)              { return nil, nil }
func (probeSettings) GetCore(key string) (interface{}, error)           { return nil, nil }
func (probeSettings) SetCore(key string, value interface{}) error       { return nil }
func (probeSettings) ListCore(prefix string) ([]string, error)          { return nil, nil }
func (probeSettings) DataDir() string                                   { return "" }
func (probeSettings) GetPlugin(plugin, key string) (interface{}, error) { return nil, nil }
func (probeSettings) SetPlugin(plugin, key string, value interface{}) error {
	return nil
}
func (probeSettings) ListPlugin(plugin, prefix string) ([]string, error) { return nil, nil }
func (probeSettings) RegisterDef(def pubsdk.ConfigDef)                   {}
func (probeSettings) Defs(prefix string) []*pubsdk.ConfigDef             { return nil }
func (probeSettings) Dump() map[string]interface{}                       { return nil }
func (probeSettings) Plugins() []string                                  { return nil }

// 覆写上游为 nil 的访问器：只有后端真存在，「走到闸门之后」才可观测。
func (c *capableCore) Memory() pubsdk.MemoryAPI         { return probeMemory{} }
func (c *capableCore) DocMemory() pubsdk.DocMemoryAPI   { return probeDocMemory{} }
func (c *capableCore) Knowledge() pubsdk.KnowledgeAPI   { return probeKnowledge{} }
func (c *capableCore) TextMemory() pubsdk.TextMemoryAPI { return probeTextMemory{} }
func (c *capableCore) LLM() pubsdk.LLMAPI               { return probeLLM{} }
func (c *capableCore) Social() pubsdk.SocialAPI         { return probeSocial{} }
func (c *capableCore) PluginMgr() pubsdk.PluginMgrAPI   { return probePluginMgr{} }
func (c *capableCore) Settings() pubsdk.SettingsAPI     { return probeSettings{} }

// sortStringsAscending 检查字典序（拒绝错误里列能力名，乱序会显得随意）。
func sortStringsAscending(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
