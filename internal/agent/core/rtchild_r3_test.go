package core

// R3–R6 判据：rtchild 生命周期 / 流式输出 / 打断截断 / toolcall 期不可打断。
//
// 每条判据都对应 plan.md 十一.七 的一个步骤，删掉被钉的实现必须变红。

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ── 测试替身 ──

// rtBlockingProvider 的 ChatStream 会一直流式吐出 token，直到 ctx 被取消。
//
// 用它来验证"打断即截断"：取消后流必须停在半路，且**不得重试**。
type rtBlockingProvider struct {
	mu    sync.Mutex
	calls int // ChatStream 被调用次数（验证"不重试"）
	got   int // 已发出的 chunk 数
	// blockForever=true 时永不自然结束（只能靠 cancel 停）。
	blockForever bool
}

func (p *rtBlockingProvider) Name() string          { return "rtBlocking" }
func (p *rtBlockingProvider) MaxContextTokens() int { return 8192 }
func (p *rtBlockingProvider) Chat(ctx context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "non-stream"}, nil
}

func (p *rtBlockingProvider) ChatStream(ctx context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()

	ch := make(chan agentAPI.StreamChunk)
	go func() {
		defer close(ch)
		for i := 0; ; i++ {
			select {
			case <-ctx.Done():
				return // 被取消：流就地停止（这**就是**"就地截断"）
			case <-time.After(5 * time.Millisecond):
			}
			select {
			case <-ctx.Done():
				return
			case ch <- agentAPI.StreamChunk{Content: "字"}:
				p.mu.Lock()
				p.got++
				p.mu.Unlock()
			}
			if !p.blockForever && i >= 2 {
				return
			}
		}
	}()
	return ch, nil
}

func (p *rtBlockingProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// rtRecordingDevice 记录 outputch 收到的每一段文本。
type rtRecordingDevice struct {
	mu    sync.Mutex
	texts []string
	name  string
}

func (d *rtRecordingDevice) Name() string             { return d.name }
func (d *rtRecordingDevice) Type() agentIO.DeviceType { return agentIO.DeviceOutput }
func (d *rtRecordingDevice) Description() string      { return "rt test output" }
func (d *rtRecordingDevice) Tools() []agentIO.ToolDef { return nil }
func (d *rtRecordingDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := args["text"].(string); ok {
		d.texts = append(d.texts, s)
	}
	return "ok", nil
}
func (d *rtRecordingDevice) Start() error                   { return nil }
func (d *rtRecordingDevice) Stop() error                    { return nil }
func (d *rtRecordingDevice) ChannelDef() agentIO.ChannelDef { return agentIO.ChannelDef{} }
func (d *rtRecordingDevice) OutputCapabilities() agentIO.OutputCapability {
	// ★ 声明 CapRT（"我能承载 rt 流"）。注意它仍然只收文本 ——
	// CapRT 是"能承载实时流"，不是"内核要产出音频"。
	return agentIO.CapText | agentIO.CapRT
}

func (d *rtRecordingDevice) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.texts...)
}

// newRTHost 造一个"父 agent + 注册好的 rt 输出设备"。
func newRTHost(t *testing.T, p agentAPI.Provider) (*Agent, *rtRecordingDevice) {
	t.Helper()
	pio := agentIO.NewIOManager()
	host := New(AgentConfig{ID: "rt-host", IO: pio, Provider: p})
	dev := &rtRecordingDevice{name: "voice"}
	if err := pio.RegisterDevice(dev); err != nil {
		t.Fatalf("注册 rt 输出设备: %v", err)
	}
	// rt 通道也必须能划（R1 的校验路径），这里走普通 Register + AssignRealtime。
	//
	// ★ 注册**一对** rt 通道（"voice-in" + "voice"）：rt 通道必须成对划拨
	// （用户裁定 2026-10-30），所以 SpawnRTChild 要求两个名字都在登记表里。
	// 测试统一用 rtSpawn 走"成对"路径（见下）。
	for _, n := range []string{"voice", "voice-in"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "test", Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册 rt 通道 %s: %v", n, err)
		}
	}
	return host, dev
}

// rtSpawn 是测试里创建 rtchild 的**唯一入口**：自动补上成对的 Input。
//
// 为什么不改每个调用点：成对是 SpawnRTChild 的硬要求，但测试关心的是
// 各自的语义（生命周期/流式/上下文…），一个个补 `Input: "voice-in"`
// 只是噪声，且容易漏（漏了就是"创建失败 + 后续 nil 解引用"）。
func rtSpawn(t *testing.T, host *Agent, id string) string {
	t.Helper()
	got, err := host.SpawnRTChild(RTChildOptions{
		ID: id, Input: "voice-in", Output: "voice",
	})
	if err != nil {
		t.Fatalf("创建 rtchild %s: %v", id, err)
	}
	return got
}

// ── R3：生命周期 ──

// TestRTChild_StopNoOrphans 钉住：Stop 之后实例出登记表、且不再产生输出。
func TestRTChild_StopNoOrphans(t *testing.T) {
	p := &rtBlockingProvider{blockForever: true}
	host, dev := newRTHost(t, p)

	id := rtSpawn(t, host, "c1")
	if got := host.RTChildren(); len(got) != 1 || got[0] != "c1" {
		t.Fatalf("登记表 = %v，期望 [c1]", got)
	}

	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止: %v", err)
	}
	if got := host.RTChildren(); len(got) != 0 {
		t.Fatalf("停止后登记表仍有 %v —— 留了孤儿", got)
	}

	// Stop 之后不得再有输出（流已被截断）。
	before := len(dev.snapshot())
	time.Sleep(60 * time.Millisecond)
	after := len(dev.snapshot())
	if after != before {
		t.Fatalf("停止后仍在输出（%d → %d）—— 当前轮未被截断", before, after)
	}
}

// TestRTChild_StopAllNoOrphans 钉住父退出时全部销毁（对应 StopRTChildren）。
func TestRTChild_StopAllNoOrphans(t *testing.T) {
	p := &rtBlockingProvider{blockForever: true}
	host, _ := newRTHost(t, p)

	for i, id := range []string{"a", "b", "c"} {
		rtSpawn(t, host, id)
		_ = i
	}
	if got := len(host.RTChildren()); got != 3 {
		t.Fatalf("登记了 %d 个，期望 3", got)
	}
	if n := host.StopRTChildren(); n != 3 {
		t.Fatalf("StopRTChildren 返回 %d，期望 3", n)
	}
	if got := host.RTChildren(); len(got) != 0 {
		t.Fatalf("停止后仍有 %v", got)
	}
}

// TestRTChild_InterruptDoesNotAffectOthers 是**用户裁定 R1 的核心判据**：
// 打断/停止 rtA 时，rtB 与主 agent 的 LLM 调用必须继续。
//
// 实现上靠"每个 rtchild 自己的 ctx"。若有人把 rtStream 里的
// `context.WithCancel(a.ctx)` 改回直接用 `a.ctx`，本判据必红。
func TestRTChild_InterruptDoesNotAffectOthers(t *testing.T) {
	p := &rtBlockingProvider{blockForever: true}
	host, _ := newRTHost(t, p)

	idA := rtSpawn(t, host, "a")
	idB := rtSpawn(t, host, "b")

	// 让两个实例都跑起来（各自 block 在流上）。
	if _, err := host.PushRTInput(idA, "你好 A"); err != nil {
		t.Fatalf("投递 A: %v", err)
	}
	if _, err := host.PushRTInput(idB, "你好 B"); err != nil {
		t.Fatalf("投递 B: %v", err)
	}
	waitFor(t, "两个实例都进入流式", func() bool { return p.callCount() >= 2 })

	// 停 A。
	if err := host.StopRTChild(idA); err != nil {
		t.Fatalf("停止 A: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	// B 必须仍然活着、仍在登记表里。
	if _, ok := host.RTChildStats(idB); !ok {
		t.Fatal("停 A 之后 B 不见了 —— 打断波及了其他实例（R1 被违反）")
	}
	// 父 agent 的 ctx 必须仍然可用（没被连带取消）。
	select {
	case <-host.ctx.Done():
		t.Fatal("停 rtchild 把父 agent 的 ctx 也取消了 —— 严重影响主 agent")
	default:
	}
}

// ── R4：流式输出 ──

// TestRTChild_OutputIsStreamed 钉住契约 2：输出**逐片**外发，不攒整段。
//
// 若把 rtStream 改成"先收完整段再 emit"，本判据会看到 1 次 emit 而非多次。
func TestRTChild_OutputIsStreamed(t *testing.T) {
	p := &rtBlockingProvider{} // 非 blockForever：吐 3 片后自然结束
	host, dev := newRTHost(t, p)

	id := rtSpawn(t, host, "s")
	if _, err := host.PushRTInput(id, "说点什么"); err != nil {
		t.Fatalf("投递: %v", err)
	}

	waitFor(t, "收到多片输出", func() bool { return len(dev.snapshot()) >= 3 })

	texts := dev.snapshot()
	for _, s := range texts {
		if len([]rune(s)) > 8 {
			t.Fatalf("单片输出过长（%q）——这看起来是攒整段后一次吐出，不是流式", s)
		}
	}
	if n := len(texts); n < 3 {
		t.Fatalf("只收到 %d 片，期望 ≥3（流式逐片外发）", n)
	}
}

// ── R5：打断即截断，且不重试 ──

// TestRTChild_InterruptTruncatesOutput 钉住契约 2 的后半：被打断时就地截断。
func TestRTChild_InterruptTruncatesOutput(t *testing.T) {
	p := &rtBlockingProvider{blockForever: true}
	host, dev := newRTHost(t, p)

	id := rtSpawn(t, host, "t")
	host.PushRTInput(id, "一直说")
	waitFor(t, "开始输出", func() bool { return len(dev.snapshot()) >= 2 })

	if err := host.StopRTChild(id); err != nil {
		t.Fatalf("停止: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	frozen := len(dev.snapshot())
	time.Sleep(80 * time.Millisecond)
	if got := len(dev.snapshot()); got != frozen {
		t.Fatalf("截断后仍在输出（%d → %d）", frozen, got)
	}
}

// TestRTChild_NewInputWinsWithoutRetry 钉住契约 1 + "不重试"：
// 新输入到来时，旧轮次被作废，**不会**为旧输入补跑一次。
//
// 若在打断后重试（break 改 continue 之类），callCount 会明显超过 2。
func TestRTChild_NewInputWinsWithoutRetry(t *testing.T) {
	p := &rtBlockingProvider{blockForever: true}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "n")
	host.PushRTInput(id, "第一句")
	waitFor(t, "第一轮开始", func() bool { return p.callCount() >= 1 })

	// 新输入：应作废旧轮并用新输入重开。
	if _, err := host.PushRTInput(id, "第二句"); err != nil {
		t.Fatalf("投递第二句: %v", err)
	}
	waitFor(t, "第二轮开始", func() bool { return p.callCount() >= 2 })

	// ★ 关键：不得为"第一句"补跑。给足够时间让任何错误的重试发生。
	time.Sleep(120 * time.Millisecond)
	if got := p.callCount(); got > 3 {
		t.Fatalf("ChatStream 被调用 %d 次 —— 存在「为旧输入重试」（契约 1/3 禁止补偿）", got)
	}

	stats, _ := host.RTChildStats(id)
	// lastSeq 必须是**最新**输入的那次（第二句）。
	if stats.LastSeq != 2 {
		t.Fatalf("LastSeq = %d，期望 2（第二轮必须基于最新输入）", stats.LastSeq)
	}
}

// ── R6：toolcall 期间不可打断 ──

// rtToolProvider 第一轮返回一个工具调用，第二轮返回纯文本。
type rtToolProvider struct {
	mu     sync.Mutex
	calls  int
	toolCh chan struct{} // 工具开始时关闭（测试用它对齐时序）
}

func (p *rtToolProvider) Name() string          { return "rtTool" }
func (p *rtToolProvider) MaxContextTokens() int { return 8192 }
func (p *rtToolProvider) Chat(ctx context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{}, nil
}
func (p *rtToolProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()

	ch := make(chan agentAPI.StreamChunk, 4)
	if n == 1 {
		ch <- agentAPI.StreamChunk{ToolCalls: []agentAPI.ToolCall{{
			ID: "c1", Name: "rt_slow_tool", Arguments: map[string]interface{}{},
		}}}
		ch <- agentAPI.StreamChunk{Done: true}
	} else {
		ch <- agentAPI.StreamChunk{Content: "好", Done: true}
	}
	close(ch)
	return ch, nil
}

// TestRTChild_NotInterruptibleDuringToolExec 钉住 R5 的边界：
// 工具执行期间新输入**不得**打断它（副作用不可回滚）。
//
// ❗ 这一条**单独测不出真实缺陷**（实测过）：设备工具经 `a.io.ExecuteTool`
// 执行，它内部传的是 `context.Background()`（toolcall.go:377），工具根本
// 收不到 roundCtx。于是"工具没被打断"在下面两种情况下都成立：
//
//	· 守卫生效（我们主动不取消）
//	· 守卫失效但取消也没效果（工具不消费 ctx）
//
// 所以真正钉住守卫的是下一个判据（suppressedCancels）。这里保留本判据，
// 因为它测的是**用户可见的行为**（工具跑完），与下面的实现细节互补。
func TestRTChild_NotInterruptibleDuringToolExec(t *testing.T) {
	p := &rtToolProvider{}
	host, _ := newRTHost(t, p)

	var executed atomic.Int64
	done := make(chan struct{}, 1)
	if err := host.io.RegisterDevice(&rtToolDevice{
		onExec: func() {
			executed.Add(1)
			time.Sleep(80 * time.Millisecond)
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}); err != nil {
		t.Fatalf("注册工具设备: %v", err)
	}

	id := rtSpawn(t, host, "tool")
	host.PushRTInput(id, "调用工具")

	waitFor(t, "工具开始执行", func() bool { return executed.Load() == 1 })
	if _, err := host.PushRTInput(id, "新的插话"); err != nil {
		t.Fatalf("插话: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("工具执行期间被新输入打断 —— 违反 R5「除 toolcall 期间都可打断」的边界")
	}
}

// TestRTChild_CriticalSectionSuppressesCancel 是 R6 守卫的**直接判据**：
// 工具执行期收到新输入时，内核必须**记录一次"故意未取消"**。
//
// 这是唯一能区分"守卫真的执行了"与"守卫失效但取消没效果"的判据。
// 变异验证：把 `if critical > 0 { suppressedCancels++ }` 改成直接 cancel，
// 本判据必红（因为 suppressedCancels 恒为 0）。
func TestRTChild_CriticalSectionSuppressesCancel(t *testing.T) {
	p := &rtToolProvider{}
	host, _ := newRTHost(t, p)

	var executed atomic.Int64
	done := make(chan struct{}, 1)
	if err := host.io.RegisterDevice(&rtToolDevice{
		onExec: func() {
			executed.Add(1)
			time.Sleep(100 * time.Millisecond) // 留出插话窗口
			select {
			case done <- struct{}{}:
			default:
			}
		},
	}); err != nil {
		t.Fatalf("注册工具设备: %v", err)
	}

	id := rtSpawn(t, host, "guard")
	host.PushRTInput(id, "调用工具")
	waitFor(t, "工具开始执行", func() bool { return executed.Load() == 1 })

	// 工具执行期间推新输入：此时 critical > 0 ⇒ 必须抑制取消。
	if _, err := host.PushRTInput(id, "工具期插话"); err != nil {
		t.Fatalf("插话: %v", err)
	}
	waitFor(t, "抑制计数 +1", func() bool {
		st, ok := host.RTChildStats(id)
		return ok && st.SuppressedCancels >= 1
	})

	<-done
}

// rtToolDevice 提供一个会阻塞的 rt_slow_tool。
type rtToolDevice struct {
	name   string
	onExec func()
}

func (d *rtToolDevice) Name() string                                 { return "rt-tool-dev" }
func (d *rtToolDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceIO }
func (d *rtToolDevice) Description() string                          { return "rt tool test device" }
func (d *rtToolDevice) Start() error                                 { return nil }
func (d *rtToolDevice) Stop() error                                  { return nil }
func (d *rtToolDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *rtToolDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
func (d *rtToolDevice) Tools() []agentIO.ToolDef {
	return []agentIO.ToolDef{{
		Name:        "rt_slow_tool",
		Description: "slow tool for rt test",
		Parameters:  map[string]interface{}{"type": "object"},
	}}
}
func (d *rtToolDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	if d.onExec != nil {
		d.onExec()
	}
	return "tool done", nil
}

// TestRTChild_PromptRequiresPreCallNotice 钉住用户裁定 R4：
// 默认提示词必须要求"调用工具前先告知用户"。
func TestRTChild_PromptRequiresPreCallNotice(t *testing.T) {
	if !strings.Contains(rtChildDefaultPrompt, "调用任何工具之前") {
		t.Fatal("rtchild 默认提示词缺少「调用工具前先告知」约束（用户裁定 R4）")
	}
	if !strings.Contains(rtChildDefaultPrompt, "实时通话") {
		t.Fatal("rtchild 默认提示词缺少【实时通话】段")
	}
}
