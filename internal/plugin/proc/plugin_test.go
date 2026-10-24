package proc

import (
	"encoding/json"
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"strings"
	"sync"
	"testing"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 端到端验证：内核 RunStage 并发扇出 → 真实子进程插件经共享内存读改写 → 结果回读。
//
// 这是**整个迁移最关键的一环闭环验证**（§4.4 风险 3.4）：
// 机制在 shm_test.go 已被单元验证，这里验证它在真进程 + 真 RPC 下同样成立。

// fakeCoreSDK 是最简 CoreSDK 实现，记录注册行为。
type fakeCoreSDK struct {
	mu         sync.Mutex
	tools      map[string]pubsdk.ToolHandler
	toolDefs   map[string]pubsdk.ToolDef
	stages     map[pubsdk.Stage][]pubsdk.StageHandler
	outputs    map[string]pubsdk.ToolHandler
	outputDefs map[string]pubsdk.ChannelDef
	inputDefs  map[string]pubsdk.ChannelDef
	settings   map[string]interface{}
	autoStart  bool

	// injected 记录经 InjectText 注入的文本（验证跨进程共享槽路径）。
	injected []string
	// lastInjectOpts 记录最近一次带标志位注入的 opts（跨进程转发断言用）。
	lastInjectOpts pubsdk.InjectOptions
	// toolBlocks 累积 SetToolBlocks 收到的块（多模态注入通道）。
	toolBlocks []pubsdk.ContentBlock
	// 文档/知识：验证大正文经 doc_ref / content_ref 走共享内存。
	docMem *fakeDocMemory
	// 用接口而非具体类型：需要能塞入"只实现公开 KnowledgeAPI、
	// 不具备内核多模态扩展"的替身，以验证能力缺失时的报错路径。
	knowledge knowledgeAPITest

	// settingsAPI 是可注入的真实配置后端（端到端加密判据用）。
	// 为 nil 时 Settings() 返回 nil（旧行为不变）。
	settingsAPI pubsdk.SettingsAPI
	// secretsAPI 同理：变量库后端（端到端判据用）。
	secretsAPI pubsdk.SecretsAPI
}

// knowledgeAPITest 是公开 SDK 的知识库契约（不含内核扩展方法）。
type knowledgeAPITest interface {
	Search(query string, topK int) ([]*pubsdk.Knowledge, error)
	Add(name, content string) error
	List() ([]string, error)
}

func newFakeCore() *fakeCoreSDK {
	return &fakeCoreSDK{
		tools:      map[string]pubsdk.ToolHandler{},
		toolDefs:   map[string]pubsdk.ToolDef{},
		stages:     map[pubsdk.Stage][]pubsdk.StageHandler{},
		outputs:    map[string]pubsdk.ToolHandler{},
		outputDefs: map[string]pubsdk.ChannelDef{},
		inputDefs:  map[string]pubsdk.ChannelDef{},
		settings:   map[string]interface{}{},
	}
}

func (f *fakeCoreSDK) PluginName() string                  { return "fake" }
func (f *fakeCoreSDK) Settings() pubsdk.SettingsAPI        { return f.settingsAPI }
func (f *fakeCoreSDK) Secrets() pubsdk.SecretsAPI          { return f.secretsAPI }
func (f *fakeCoreSDK) Memory() pubsdk.MemoryAPI            { return nil }
func (f *fakeCoreSDK) TextMemory() pubsdk.TextMemoryAPI    { return nil }
func (f *fakeCoreSDK) DocMemory() pubsdk.DocMemoryAPI      { return f.docMem }
func (f *fakeCoreSDK) Knowledge() pubsdk.KnowledgeAPI      { return f.knowledge }
func (f *fakeCoreSDK) LLM() pubsdk.LLMAPI                  { return nil }
func (f *fakeCoreSDK) Social() pubsdk.SocialAPI            { return nil }
func (f *fakeCoreSDK) PluginMgr() pubsdk.PluginMgrAPI      { return nil }
func (f *fakeCoreSDK) RegisterPluginAPI(name string) error { return nil }
func (f *fakeCoreSDK) InjectText(s, c, t string) {
	f.mu.Lock()
	f.injected = append(f.injected, t)
	f.mu.Unlock()
}

// lastInjectOpts 记录最近一次带标志位注入的 opts（跨进程转发断言用）。
func (f *fakeCoreSDK) lastOpts() pubsdk.InjectOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastInjectOpts
}

func (f *fakeCoreSDK) injectedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.injected...)
}
func (f *fakeCoreSDK) InjectInterruptText(s, c, t string)                       {}
func (f *fakeCoreSDK) InjectTextNoMemory(s, c, t string)                        {}
func (f *fakeCoreSDK) InjectInputSync(s, c, t string) string                    { return "" }
func (f *fakeCoreSDK) InjectInputMedia(s, c, t string, b []pubsdk.ContentBlock) {}
func (f *fakeCoreSDK) InjectInputMediaSync(s, c, t string, b []pubsdk.ContentBlock) string {
	return ""
}
func (f *fakeCoreSDK) InjectInterruptMedia(s, c, t string, b []pubsdk.ContentBlock) {}

// ---- 带 InjectOptions 的注入（1.2.0）----
//
// 转发到旧方法即可：本测试关心的是「注入了什么话」，标志位的转发在
// corehandler 与 io 层的测试里覆盖。
func (f *fakeCoreSDK) InjectTextOpts(s, c, t string, o pubsdk.InjectOptions) {
	// 记录 opts：跨进程测试要断言插件在调用点声明的标志位确实穿过了 RPC。
	f.mu.Lock()
	f.lastInjectOpts = o
	f.mu.Unlock()
	f.InjectText(s, c, t)
}
func (f *fakeCoreSDK) InjectInterruptTextOpts(s, c, t string, o pubsdk.InjectOptions) {
	f.InjectInterruptText(s, c, t)
}
func (f *fakeCoreSDK) InjectInputSyncOpts(s, c, t string, o pubsdk.InjectOptions) string {
	return f.InjectInputSync(s, c, t)
}
func (f *fakeCoreSDK) InjectInputMediaOpts(s, c, t string, b []pubsdk.ContentBlock, o pubsdk.InjectOptions) {
	f.InjectInputMedia(s, c, t, b)
}
func (f *fakeCoreSDK) InjectInputMediaSyncOpts(s, c, t string, b []pubsdk.ContentBlock, o pubsdk.InjectOptions) string {
	return f.InjectInputMediaSync(s, c, t, b)
}
func (f *fakeCoreSDK) InjectInterruptMediaOpts(s, c, t string, b []pubsdk.ContentBlock, o pubsdk.InjectOptions) {
	f.InjectInterruptMedia(s, c, t, b)
}

// SetToolBlocks 记录收到的媒体块，供测试断言共享内存通道真的把内容带到了内核侧。
func (f *fakeCoreSDK) SetToolBlocks(blocks []pubsdk.ContentBlock) {
	f.mu.Lock()
	f.toolBlocks = append(f.toolBlocks, blocks...)
	f.mu.Unlock()
}

func (f *fakeCoreSDK) toolBlockCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.toolBlocks)
}

// fakeDocMemory 只实现测试需要的部分，记录 Insert 收到的文档。
type fakeDocMemory struct {
	mu  sync.Mutex
	got *pubsdk.Doc
}

func (f *fakeDocMemory) Query(string, int) []*pubsdk.Doc { return nil }
func (f *fakeDocMemory) Insert(doc *pubsdk.Doc) error {
	f.mu.Lock()
	f.got = doc
	f.mu.Unlock()
	return nil
}
func (f *fakeDocMemory) InsertWithMedia(doc *pubsdk.Doc, _ []pubsdk.MediaAttachment) error {
	return f.Insert(doc)
}
func (f *fakeDocMemory) Remove(string)                 {}
func (f *fakeDocMemory) Stats() map[string]interface{} { return nil }

// fakeKnowledge 只实现测试需要的部分，记录 Add 收到的正文。
type fakeKnowledge struct {
	mu    sync.Mutex
	name  string
	body  string
	media []knowledge.KnowledgeMediaRef
}

// AddWithMedia 模拟内核的扩展能力（corehandler 用局部接口断言它）。
func (f *fakeKnowledge) AddWithMedia(name, content string, media []knowledge.KnowledgeMediaRef) error {
	f.mu.Lock()
	f.name, f.body, f.media = name, content, media
	f.mu.Unlock()
	return nil
}

func (f *fakeKnowledge) Search(string, int) ([]*pubsdk.Knowledge, error) { return nil, nil }
func (f *fakeKnowledge) Add(name, content string) error {
	f.mu.Lock()
	f.name, f.body = name, content
	f.mu.Unlock()
	return nil
}
func (f *fakeKnowledge) List() ([]string, error) { return nil, nil }

// arenaPutForTest 把一段字节放进 arena 并返回引用（测试用）。
func arenaPutForTest(t *testing.T, host *Host, blob []byte) SharedRef {
	t.Helper()
	arena := host.Arena()
	gen := host.Generation()
	ref, err := arena.Alloc(OwnerHost, len(blob), gen)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	area, err := arena.Read(ref, gen)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	copy(area[:len(blob)], blob)
	return ref
}

func (f *fakeCoreSDK) SetAutoRestart(enabled bool) { f.autoStart = enabled }

func (f *fakeCoreSDK) RegisterTool(name string, def pubsdk.ToolDef, h pubsdk.ToolHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools[name] = h
	f.toolDefs[name] = def
	return nil
}

func (f *fakeCoreSDK) RegisterStage(stage pubsdk.Stage, h pubsdk.StageHandler, scope ...pubsdk.StageScope) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stages[stage] = append(f.stages[stage], h)
}

func (f *fakeCoreSDK) RegisterOutputChannel(name string, caps int, desc string, def pubsdk.ChannelDef, h pubsdk.ToolHandler) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outputs[name] = h
	f.outputDefs[name] = def
	return nil
}

func (f *fakeCoreSDK) RegisterInputChannel(name string, def pubsdk.ChannelDef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputDefs[name] = def
	return nil
}

func (f *fakeCoreSDK) stageHandlers(stage pubsdk.Stage) []pubsdk.StageHandler {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pubsdk.StageHandler, len(f.stages[stage]))
	copy(out, f.stages[stage])
	return out
}

// runStageLikeKernel 复刻 internal/agent/core.StageHost.RunStage 的并发扇出语义
// （stages.go:124 的 go func + wg.Wait），验证外部插件在同样的并发模型下正确工作。
func runStageLikeKernel(handlers []pubsdk.StageHandler, sc *pubsdk.StageContext) []error {
	var wg sync.WaitGroup
	errCh := make(chan error, len(handlers))
	for _, h := range handlers {
		wg.Add(1)
		go func(fn pubsdk.StageHandler) {
			defer wg.Done()
			if err := fn(sc); err != nil {
				errCh <- err
			}
		}(h)
	}
	wg.Wait()
	close(errCh)
	var errs []error
	for err := range errCh {
		errs = append(errs, err)
	}
	return errs
}

// 单插件 stage 读改写：验证共享段 + RPC + 锁的完整链路。
func TestPlugin_StageReadModifyWriteOverSharedMemory(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("sanitizer", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	handlers := core.stageHandlers(pubsdk.StageAfterToolcall)
	if len(handlers) != 1 {
		t.Fatalf("插件应注册 1 个 after_toolcall handler，实际 %d", len(handlers))
	}

	dirty := "结果：\x1b[31m脏数据\x1b[0m"
	clean := "结果：脏数据"
	sc := &pubsdk.StageContext{
		Phase:       pubsdk.StageAfterToolcall,
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "x_tool", Result: dirty}},
	}

	if errs := runStageLikeKernel(handlers, sc); len(errs) > 0 {
		t.Fatalf("stage 执行失败: %v", errs)
	}

	got, _ := sc.ToolResults[0].Result.(string)
	if got != clean {
		t.Fatalf("插件的清洗结果未回到内核 StageContext：期望 %q，实际 %q", clean, got)
	}
}

// **核心断言**：改写型插件 + 只读插件并发时，清洗结果不被覆盖。
// 复刻现网 sanitizer + weather 场景（§8.6 实测 C ABI 下 1.6~4.3% 被覆盖）。
func TestPlugin_ConcurrentWriterAndReaderNoLostUpdate(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")

	// ❗ 两个插件进程**共享同一个 Host**（同一 memfd）——这是消除 lost update 的前提。
	// 若各持一段，「内核 ctx → 段 → 插件改 → 回读 ctx」会退化成副本模型，
	// 最后回读者覆盖前者，§8.4 的 35.8~36.8% 丢失原样复现。
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	writerCore := newFakeCore()
	writer := New("sanitizer", bin, t.TempDir(), nil, host, nil)
	if err := writer.Start(writerCore); err != nil {
		t.Fatalf("writer Start: %v", err)
	}
	defer writer.Close()

	readerBin := buildTestPlugin(t, "readonlyplugin.go")
	readerCore := newFakeCore()
	reader := New("weather", readerBin, t.TempDir(), nil, host, nil)
	if err := reader.Start(readerCore); err != nil {
		t.Fatalf("reader Start: %v", err)
	}
	defer reader.Close()

	handlers := append(
		writerCore.stageHandlers(pubsdk.StageAfterToolcall),
		readerCore.stageHandlers(pubsdk.StageAfterToolcall)...,
	)
	if len(handlers) != 2 {
		t.Fatalf("应有 2 个 handler，实际 %d", len(handlers))
	}

	dirty := "天气：晴 \x1b[31m28°C\x1b[0m"
	clean := "天气：晴 28°C"
	sc := &pubsdk.StageContext{
		Phase:       pubsdk.StageAfterToolcall,
		ToolResults: []pubsdk.ToolResult{{CallID: "c1", Name: "weather_query", Result: dirty}},
	}

	if errs := runStageLikeKernel(handlers, sc); len(errs) > 0 {
		t.Fatalf("stage 执行失败: %v", errs)
	}

	got, _ := sc.ToolResults[0].Result.(string)
	if got != clean {
		t.Fatalf("只读插件覆盖了改写插件的清洗结果：期望 %q，实际 %q", clean, got)
	}
}

// 插件注册的工具可被内核调用，并把结果带回。
func TestPlugin_RegisteredToolInvokable(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.tools["demo_upper"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 demo_upper 工具")
	}

	res, err := h(map[string]interface{}{"text": "abc"})
	if err != nil {
		t.Fatalf("调用工具: %v", err)
	}
	if res != "ABC" {
		t.Fatalf("工具结果应为 ABC，实际 %v", res)
	}

	core.mu.Lock()
	def, ok := core.toolDefs["demo_upper"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("内核未保存 demo_upper 的 ToolDef")
	}
	if def.Cleaner == nil {
		t.Fatal("跨进程注册后 Cleaner 不应丢失")
	}
	if got := def.Cleaner("raw-output"); got != "tool-cleaned:raw-output" {
		t.Fatalf("跨进程工具 Cleaner 结果错误：got %q, want %q", got, "tool-cleaned:raw-output")
	}

	core.mu.Lock()
	inputDef, inputOK := core.inputDefs["demo_in"]
	outputDef, outputOK := core.outputDefs["demo_ch"]
	core.mu.Unlock()
	if !inputOK || inputDef.Cleaner == nil {
		t.Fatal("跨进程注册后输入通道 Cleaner 不应丢失")
	}
	if got := inputDef.Cleaner("raw-input"); got != "input-cleaned:raw-input" {
		t.Fatalf("跨进程输入 Cleaner 结果错误：got %q", got)
	}
	if !outputOK || outputDef.Cleaner == nil {
		t.Fatal("跨进程注册后输出通道 Cleaner 不应丢失")
	}
	if got := outputDef.Cleaner("raw-output"); got != "output-cleaned:raw-output" {
		t.Fatalf("跨进程输出 Cleaner 结果错误：got %q", got)
	}
}

// §13.6：输出通道 payload 走共享内存调用帧（与 tool.invoke 同一模型），
// 大 payload 不再爆 stdin/stdout 管道。
func TestPlugin_OutputPayloadViaArena(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.outputs["demo_ch"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 demo_ch 输出通道")
	}

	// 9000 字节，明显超过任何内联预算
	payload := strings.Repeat("输出", 3000)
	res, err := h(map[string]interface{}{"payload": payload, "type": "text"})
	if err != nil {
		t.Fatalf("发送应成功: %v", err)
	}
	m, _ := res.(map[string]interface{})
	// 插件回报它实际收到的长度：只有完整 payload 经帧送达才等于发送长度。
	var gotLen int
	switch v := m["payload_len"].(type) {
	case float64:
		gotLen = int(v)
	case int:
		gotLen = v
	}
	if gotLen != len(payload) {
		t.Fatalf("插件收到的 payload 长度 = %d，期望 %d（帧未把完整 payload 带到插件侧）", gotLen, len(payload))
	}
	if used, total := host.Arena().Stats(); used != 0 {
		t.Fatalf("调用结束后 arena 应归零，实际 used=%d/%d", used, total)
	}
}

// 输出通道**同步等真实结果**：失败必须上报（§9.4 根治）。
func TestPlugin_OutputChannelReportsRealFailure(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.outputs["demo_ch"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 demo_ch 输出通道")
	}

	// 成功路径
	res, err := h(map[string]interface{}{"payload": "hi", "type": "text"})
	if err != nil {
		t.Fatalf("发送应成功: %v", err)
	}
	m, _ := res.(map[string]interface{})
	if m["status"] != "sent" {
		t.Errorf("成功应返回 status=sent，实际 %v", m)
	}

	// 失败路径：插件返回错误 → 调用方必须收到 error（而非假成功）
	_, err = h(map[string]interface{}{"payload": "fail", "type": "text"})
	if err == nil {
		t.Fatal("发送失败时必须上报 error（C ABI 路径此处永远假成功）")
	}
	if !strings.Contains(err.Error(), "缺少 user_id") {
		t.Errorf("应透传插件的失败原因，实际: %v", err)
	}
}

// 插件在 plugin.start 期间反向调用内核（settings/autoRestart 等）。
func TestPlugin_ReverseCallsDuringStart(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	if !core.autoStart {
		t.Error("插件调用 lifecycle.autoRestart 后内核状态应更新")
	}
}

// 权限梯度显式化（§3.8）：CoreSDK 不提供内核内部机制，
// 插件请求这些能力时必须被拒绝而非静默忽略。
func TestCoreHandler_RejectsUnknownAndUnimplementedMethods(t *testing.T) {
	h := &coreHandler{sdk: newFakeCore(), name: "x", locks: &lockRegistry{}}

	// 未知 method
	if _, err := h.Handle("supervisor.restart", nil); err == nil {
		t.Error("内核内部机制不应可达（应报未知 method）")
	}

	// 事件订阅：今日 C ABI 是空实现（静默成功），这里必须明确报未实现
	if _, err := h.Handle(MethodEventsSubscribe, json.RawMessage(`{}`)); err == nil {
		t.Error("事件订阅未落地时应明确报错，而非静默成功后收不到事件")
	}
}

// §13.13：媒体块经共享内存（blocks_ref）送达内核。
//
// 之前 io.setToolBlocks 是桩实现（直接返回“待共享段二进制通道落地”），
// 后果是**子进程插件调 SetToolBlocks 必然失败**，只有内置插件能用。
func TestCoreHandler_SetToolBlocksViaArena(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	h := &coreHandler{sdk: core, name: "x", host: host, locks: &lockRegistry{}}

	// 一张“本地生成的图”：base64 data URL，远大于内联阈值。
	big := "data:image/png;base64," + strings.Repeat("A", 8000)
	blocks := []pubsdk.ContentBlock{{
		Type:     "image_url",
		ImageURL: &pubsdk.ImageURL{URL: big},
	}}
	blob, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	arena := host.Arena()
	gen := host.Generation()
	ref, err := arena.Alloc(OwnerHost, len(blob), gen)
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	defer func() { _ = arena.Free(OwnerHost, ref) }()
	area, err := arena.Read(ref, gen)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	copy(area[:len(blob)], blob)

	params, _ := json.Marshal(map[string]interface{}{"blocks_ref": ref})
	if _, err := h.Handle(MethodIOSetToolBlocks, params); err != nil {
		t.Fatalf("setToolBlocks 应成功: %v", err)
	}
	if n := core.toolBlockCount(); n != 1 {
		t.Fatalf("内核应收到 1 个媒体块，实际 %d", n)
	}

	core.mu.Lock()
	got := core.toolBlocks[0]
	core.mu.Unlock()
	if got.ImageURL == nil || got.ImageURL.URL != big {
		t.Fatal("经共享内存送达的媒体块内容与发送的不一致")
	}
}

// 内联路径仍可用（直连 RPC 调用方 / arena 不可用时）。
func TestCoreHandler_SetToolBlocksInline(t *testing.T) {
	core := newFakeCore()
	h := &coreHandler{sdk: core, name: "x", locks: &lockRegistry{}}

	params := json.RawMessage(`{"blocks":[{"type":"text","text":"hi"}]}`)
	if _, err := h.Handle(MethodIOSetToolBlocks, params); err != nil {
		t.Fatalf("内联 blocks 应成功: %v", err)
	}
	if n := core.toolBlockCount(); n != 1 {
		t.Fatalf("内核应收到 1 个块，实际 %d", n)
	}
}

// blocks 为空必须报错，而不是静默成功——静默成功会让插件以为图已注入。
func TestCoreHandler_SetToolBlocksEmptyRejected(t *testing.T) {
	core := newFakeCore()
	h := &coreHandler{sdk: core, name: "x", locks: &lockRegistry{}}
	if _, err := h.Handle(MethodIOSetToolBlocks, json.RawMessage(`{}`)); err == nil {
		t.Error("blocks 为空应明确报错")
	}
}

// §13.13：知识正文经共享内存（content_ref）送达内核。
//
// 正文可达数十 KB；内联时整份要在 RPC 报文里再编码再拷贝一遍，且内容本体
// 不在共享段里，插件回调无法就地改写。
func TestCoreHandler_KnowledgeAddViaArena(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	kn := &fakeKnowledge{}
	core.knowledge = kn
	h := &coreHandler{sdk: core, name: "x", host: host, locks: &lockRegistry{}}

	content := strings.Repeat("知识正文", 3000) // 12000 字节
	blob, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ref := arenaPutForTest(t, host, blob)
	defer func() { _ = host.Arena().Free(OwnerHost, ref) }()

	params, _ := json.Marshal(map[string]interface{}{"name": "n", "content_ref": ref})
	if _, err := h.Handle(MethodKnowledgeAdd, params); err != nil {
		t.Fatalf("knowledge.add 应成功: %v", err)
	}

	kn.mu.Lock()
	got, gotName := kn.body, kn.name
	kn.mu.Unlock()
	if got != content {
		t.Fatalf("经共享内存送达的正文不一致（got len=%d want len=%d）", len(got), len(content))
	}
	if gotName != "n" {
		t.Fatalf("name 传错: %q", gotName)
	}
}

// 内联回退仍可用（直连 RPC 调用方 / arena 不可用）。
func TestCoreHandler_KnowledgeAddInline(t *testing.T) {
	core := newFakeCore()
	kn := &fakeKnowledge{}
	core.knowledge = kn
	h := &coreHandler{sdk: core, name: "x", locks: &lockRegistry{}}

	params := json.RawMessage(`{"name":"n","content":"短正文"}`)
	if _, err := h.Handle(MethodKnowledgeAdd, params); err != nil {
		t.Fatalf("内联 knowledge.add 应成功: %v", err)
	}
	kn.mu.Lock()
	got := kn.body
	kn.mu.Unlock()
	if got != "短正文" {
		t.Fatalf("内联正文不一致: %q", got)
	}
}

// §13.13：文档正文经共享内存（doc_ref）送达内核。
func TestCoreHandler_DocInsertViaArena(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	dm := &fakeDocMemory{}
	core.docMem = dm
	h := &coreHandler{sdk: core, name: "x", host: host, locks: &lockRegistry{}}

	doc := &pubsdk.Doc{Title: "标题", Content: strings.Repeat("正文", 5000)}
	blob, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	ref := arenaPutForTest(t, host, blob)
	defer func() { _ = host.Arena().Free(OwnerHost, ref) }()

	params, _ := json.Marshal(map[string]interface{}{"doc_ref": ref})
	if _, err := h.Handle(MethodDocInsert, params); err != nil {
		t.Fatalf("doc.insert 应成功: %v", err)
	}

	dm.mu.Lock()
	got := dm.got
	dm.mu.Unlock()
	if got == nil || got.Content != doc.Content {
		t.Fatal("经共享内存送达的文档正文不一致")
	}
}

// stage 锁在无进行中 stage 时申请应被拒绝（防止插件在 stage 外乱加锁）。
func TestCoreHandler_StageLockOutsideStageRejected(t *testing.T) {
	h := &coreHandler{sdk: newFakeCore(), name: "x", locks: &lockRegistry{}}
	if _, err := h.Handle(MethodStageLock, nil); err == nil {
		t.Error("stage 外加锁应被拒绝")
	}
	if !strings.Contains(fmt.Sprint(mustErr(h.Handle(MethodStageUnlock, nil))), "无进行中的 stage") {
		t.Error("stage 外解锁的错误信息应说明原因")
	}
}

func mustErr(_ interface{}, err error) error { return err }

// **跨进程 lost update 终极验证**：5 个独立插件进程并发读-改-写同一个
// FinalText，全部标记必须保留。
//
// 这是实验 8（5 进程 × 300 轮零丢失）在真实 RPC + 真实 RunStage 并发扇出
// 下的复刻。对照今日 C ABI 副本模型实测 35.8~36.8% 丢失（§8.4）。
func TestPlugin_FiveProcessesConcurrentAppendNoLostUpdate(t *testing.T) {
	bin := buildTestPlugin(t, "appendplugin.go")

	// 关键：全部插件共享同一个 Host（同一 memfd）
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	tags := []string{"A", "B", "C", "D", "E"}
	var handlers []pubsdk.StageHandler
	for _, tag := range tags {
		core := newFakeCore()
		p := New("append-"+tag, bin, t.TempDir(), nil, host, nil)
		p.env = []string{"PLUGIN_TAG=" + tag}
		if err := p.Start(core); err != nil {
			t.Fatalf("插件 %s Start: %v", tag, err)
		}
		defer p.Close()
		handlers = append(handlers, core.stageHandlers(pubsdk.StageAfterToolcall)...)
	}
	if len(handlers) != len(tags) {
		t.Fatalf("应有 %d 个 handler，实际 %d", len(tags), len(handlers))
	}

	sc := &pubsdk.StageContext{
		Phase:     pubsdk.StageAfterToolcall,
		FinalText: "",
	}

	if errs := runStageLikeKernel(handlers, sc); len(errs) > 0 {
		t.Fatalf("并发 stage 执行失败: %v", errs)
	}

	// 断言：各标记出现次数之和 == 最终长度 == 插件数 ⇒ 无丢失、无撕裂
	total := 0
	counts := map[string]int{}
	for _, tag := range tags {
		c := strings.Count(sc.FinalText, tag)
		counts[tag] = c
		total += c
	}
	if total != len(sc.FinalText) {
		t.Fatalf("出现撕裂：各标记计数之和 %d != 最终长度 %d（final=%q counts=%v）",
			total, len(sc.FinalText), sc.FinalText, counts)
	}
	if total != len(tags) {
		t.Fatalf("出现 lost update：期望 %d 个插件的写入全部保留，实际 %d（final=%q counts=%v）",
			len(tags), total, sc.FinalText, counts)
	}
	for tag, c := range counts {
		if c != 1 {
			t.Errorf("插件 %s 的写入丢失：期望 1 次，实际 %d 次", tag, c)
		}
	}
}

// 跨进程共享槽池：插件通过 arena.alloc 申请、写入、随业务 RPC 回传、arena.free 归还。
//
// 验证内核独占管理的所有权模型在真进程 + 真 RPC 下成立：
//   - 内核能把插件申请的槽内容正确读回来（偏移/长度无误）
//   - 插件归还后槽确实回到池里（无泄漏）
func TestPlugin_ArenaAllocFreeAcrossProcess(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.tools["demo_inject"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 demo_inject 工具")
	}

	// 用明显超过内联阈值的 payload，确保真的走共享槽而非内联。
	payload := strings.Repeat("共享内存", 500) // 约 6KB
	if _, err := h(map[string]interface{}{"text": payload}); err != nil {
		t.Fatalf("调用 demo_inject: %v", err)
	}

	got := core.injectedTexts()
	if len(got) != 1 {
		t.Fatalf("应注入 1 条文本，实际 %d 条", len(got))
	}
	if got[0] != payload {
		t.Fatalf("经共享槽读到的内容不一致：len(got)=%d len(want)=%d", len(got[0]), len(payload))
	}

	// 注入标志位必须穿过 RPC 到达内核：插件在调用点声明「不进记忆 / 据此裁剪 /
	// 用哪个 cleaner」，内核得拿到才能照做。只测 SDK 侧记录不到这一点——
	// 字段在 JSON 与参数结构之间丢掉的失败模式是静默的。
	opts := core.lastOpts()
	if !opts.NoMemory {
		t.Errorf("no_memory 未穿过 RPC: %+v", opts)
	}
	if opts.ContextPolicy != "prune" {
		t.Errorf("context_policy 未穿过 RPC: %+v", opts)
	}
	if opts.CleanerName != "demo_cleaner" {
		t.Errorf("cleaner_name 未穿过 RPC: %+v", opts)
	}

	// 插件已归还槽：池必须回到全空，否则说明 arena.free 没生效。
	if used, total := host.Arena().Stats(); used != 0 {
		t.Fatalf("插件归还后槽池应全空，实际 used=%d/%d", used, total)
	}
}

// 工具调用的参数/结果走共享槽（§13.3）。
//
// 控制面仍是 RPC（请求 ID 关联、ctx 取消、崩溃唤醒都由它承载），
// 只有 payload 走共享内存：
//   - 参数超过阈值时内核写入槽，把 ArgsRef 发给插件
//   - 结果放得下时插件写入内核预分配的响应槽，回 ResultRef
//   - 超限/池满时退回内联 JSON，不能影响功能
//
// demo_big 会在结果里报出它到底从哪里读到参数，因此本测试验证的是
// “真的走了共享内存”，而不只是“返回值对”。
func TestPlugin_ToolInvokeArgsResultViaArena(t *testing.T) {
	bin := buildTestPlugin(t, "stageplugin.go")
	core := newFakeCore()

	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	p := New("demo", bin, t.TempDir(), nil, host, nil)
	if err := p.Start(core); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	core.mu.Lock()
	h, ok := core.tools["demo_big"]
	core.mu.Unlock()
	if !ok {
		t.Fatal("插件应注册 demo_big 工具")
	}

	t.Run("大 payload 走共享槽", func(t *testing.T) {
		// 4 字节 × 3 × 1000 = 12000 字节，明显超过内联阈值且能放进槽
		big := strings.Repeat("共享内存", 1000)
		res, err := h(map[string]interface{}{"text": big})
		if err != nil {
			t.Fatalf("调用 demo_big: %v", err)
		}
		s, _ := res.(string)
		if !strings.HasPrefix(s, "shared:") {
			t.Fatalf("大参数应经共享槽传递，实际结果前缀不对（len=%d, head=%.40q）", len(s), s)
		}
		if want := "shared:" + strings.ToUpper(big); s != want {
			t.Fatalf("经共享槽往返的内容不一致：got len=%d want len=%d", len(s), len(want))
		}
		if used, total := host.Arena().Stats(); used != 0 {
			t.Fatalf("调用结束后槽池应全空，实际 used=%d/%d", used, total)
		}
	})

	t.Run("小 payload 同样走调用帧", func(t *testing.T) {
		// 设计上不再有“小 payload 走内联”的按大小分支：内核总是标定调用帧。
		res, err := h(map[string]interface{}{"text": "abc"})
		if err != nil {
			t.Fatalf("调用 demo_big: %v", err)
		}
		if res != "shared:ABC" {
			t.Fatalf("小参数也应走内核标定的调用帧，实际 %v", res)
		}
		if used, total := host.Arena().Stats(); used != 0 {
			t.Fatalf("调用结束后应全部归还，实际 used=%d/%d", used, total)
		}
	})
}

// §13.14：knowledge.addWithMedia 走共享内存，且媒体清单能穿透到内核扩展能力。
//
// 媒体参数可能很长（一篇知识挂几十张图），与正文同形走 content_ref/media_ref。
func TestCoreHandler_KnowledgeAddWithMedia(t *testing.T) {
	host, err := NewHost()
	if err != nil {
		t.Fatalf("NewHost: %v", err)
	}
	defer host.Close()

	core := newFakeCore()
	kn := &fakeKnowledge{}
	core.knowledge = kn
	h := &coreHandler{sdk: core, name: "x", host: host, locks: &lockRegistry{}}

	content := strings.Repeat("带图知识", 2000)
	blob, _ := json.Marshal(content)
	cRef := arenaPutForTest(t, host, blob)
	defer func() { _ = host.Arena().Free(OwnerHost, cRef) }()

	media := []knowledge.KnowledgeMediaRef{{Digest: "d1", MIME: "image/png", Kind: "image"}}
	mBlob, _ := json.Marshal(media)
	mRef := arenaPutForTest(t, host, mBlob)
	defer func() { _ = host.Arena().Free(OwnerHost, mRef) }()

	params, _ := json.Marshal(map[string]interface{}{
		"name": "n", "content_ref": cRef, "media_ref": mRef,
	})
	res, err := h.Handle(MethodKnowledgeAddMedia, params)
	if err != nil {
		t.Fatalf("knowledge.addWithMedia 应成功: %v", err)
	}
	kn.mu.Lock()
	got, gotName := kn.body, kn.name
	gotMedia := kn.media
	kn.mu.Unlock()
	if gotName != "n" {
		t.Fatalf("name 传错: %q", gotName)
	}
	if got != content {
		t.Fatalf("经共享内存送达的正文不一致（got len=%d want len=%d）", len(got), len(content))
	}
	if len(gotMedia) != 1 || gotMedia[0].Digest != "d1" {
		t.Fatalf("媒体清单未穿透到内核: %+v", gotMedia)
	}
	// 回传媒体清单（插件据此后续引用同一份媒体）
	m, _ := res.(map[string]interface{})
	if m == nil || m["media"] == nil {
		t.Errorf("应回传 media 清单，实为 %#v", res)
	}
}

// 内核不支持多模态扩展时必须**明确报错**，不能静默退化成"媒体已写入"。
func TestCoreHandler_KnowledgeAddMediaCapabilityMissing(t *testing.T) {
	core := newFakeCore()
	// 用一个只实现公开 KnowledgeAPI 的假实现（无 AddWithMedia）
	core.knowledge = &pubOnlyKnowledge{}
	h := &coreHandler{sdk: core, name: "x", locks: &lockRegistry{}}

	params := json.RawMessage(`{"name":"n","content":"正文","media":[{"digest":"d1","mime":"image/png"}]}`)
	if _, err := h.Handle(MethodKnowledgeAddMedia, params); err == nil {
		t.Error("内核缺少多模态能力时应明确报错，而不是静默丢弃媒体")
	}
}

// pubOnlyKnowledge 只实现公开 SDK 的 KnowledgeAPI（**刻意不含**内核扩展的
// AddWithMedia）。不能内嵌 fakeKnowledge——那会把 AddWithMedia 一起带进来，
// 断言就会成功，用例测不到「能力缺失」这条路径。
type pubOnlyKnowledge struct{}

func (f *pubOnlyKnowledge) Search(string, int) ([]*pubsdk.Knowledge, error) { return nil, nil }
func (f *pubOnlyKnowledge) Add(name, content string) error                  { return nil }
func (f *pubOnlyKnowledge) List() ([]string, error)                         { return nil, nil }
