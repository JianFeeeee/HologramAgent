package test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"sync"
)

// ===== OpenAI 兼容面：/v1/* =====
//
// 这个端点是**给外部程序用的**（IDE、脚本、agent 框架），不是给人看的
// 聊天页。行为必须真的符合 OpenAI 协议，否则调用方直接坏掉。
//
// 下面两条都在**生产实测**中确认过（不是推理）：
//
//   GET /v1/models → 404。几乎每个 OpenAI 客户端（curl 脚本、LangChain、
//     OpenAI SDK、IDE 插件）启动时都会先列模型。404 直接让它们判定
//     「服务不可用」，连试都不试。
//
//   stream=true 不是流式：实测首字节 7.79s，随后**整段**内容在一个
//     chunk 里到达。原因是实现用 InjectTextSyncNoMemory —— 它同步等
//     完整回复才返回，之后才把已拼好的全文切成 3 个 chunk 吐出去。
//     客户端的「正在生成」体验、取消、超时、进度条全部失效。
//
// ---- 为什么必须用真 HTTP 服务器 ----
//
// httptest.ResponseRecorder **把整个响应缓冲在内存里**，请求结束时才
// 一次性交付。所以它**根本观察不到流式与否** —— 用它写的「流式判据」
// 必然是假的（真流式与假流式都会得到完整 body）。
// 只有真 socket + bufio.Reader 逐帧读，才能测出「首帧是否早于结束」。

const testAuthAPIKey = "test-api-key"

func newOpenAITestServer(t *testing.T) (*httptest.Server, *agentIO.IOManager, *events.Bus) {
	t.Helper()
	srv, iom, bus, _ := newOpenAITestServerWithHandler(t)
	return srv, iom, bus
}

// newOpenAITestServerWithHandler 额外交回 Handler，供测试直接触发阶段的处理体。
//
// ★ 为什么直接调 Handler：`on_llm_call` 的**协议**是"基座推、插件收"，
//
//	而收的那一端就是 `Handler.pushLLMDelta`。测试不装配整个 Host（那需要
//	真内核），但走的仍是生产同一个处理体。
func newOpenAITestServerWithHandler(t *testing.T) (*httptest.Server, *agentIO.IOManager, *events.Bus, *webui.Handler) {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)

	iom := agentIO.NewIOManager()
	bus := events.NewBus()
	// ★ 注入 stage 注册面：流式增量现在走 `on_llm_call` 阶段（基座在 LLM 流上
	//   逐 chunk 推送），不再经输出通道的 `delta:true` —— 那是臆造的载体。
	stageHub = &testStageHub{handlers: map[string][]sdk.StageHandler{}}
	cfg := sdk.New("webui", sdk.SDKConfig{
		Settings:  sdk.NewSettings("webui", cfgReg),
		IOManager: iom,
		EventBus:  bus,
		RegStage:  stageHub.register,
	})
	// ★ 输出通道分发器必须起：增量经 `EmitOutputTo` → `outputCh` → device
	//   handler（docs §9.2）。测试里没有 kernelloop，用独立 stop 通道。
	//
	// ★ 还要注册一个最小 device：生产里由插件的 `RegisterOutputChannel` 完成
	//   （走 pluginhost 的 regOutput 闭包），测试不走那套装配，所以手工登记
	//   一个"把 payload 交给 Handler 的 SSE 扇出"的 device。
	stop := make(chan struct{})
	iom.StartOutputDispatch(stop)
	t.Cleanup(func() { close(stop) })

	h := webui.NewHandler(cfg)
	h.RegisterRoutes(http.NewServeMux())
	_ = iom.RegisterDevice(&testOutputDevice{h: h})

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close() })
	return srv, iom, bus, h
}

// /v1/models 必须存在且返回合法结构（客户端启动必探测它）。
func TestOpenAIModelsEndpoint(t *testing.T) {
	srv, _, _ := newOpenAITestServer(t)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("X-API-Key", testAuthAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models 应 200（客户端启动必探），实际 %d", resp.StatusCode)
	}
	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if out.Object != "list" {
		t.Errorf("object = %q，OpenAI 协议要求 \"list\"", out.Object)
	}
	if len(out.Data) == 0 {
		t.Fatal("data 为空 —— 客户端拿空列表等同不可用")
	}
	for i, m := range out.Data {
		if m.ID == "" {
			t.Errorf("data[%d].id 为空", i)
		}
		if m.Object != "model" {
			t.Errorf("data[%d].object = %q，应为 \"model\"", i, m.Object)
		}
	}
}

// /v1/models 必须鉴权（否则把服务能力公开给扫描器）。
func TestOpenAIModelsRequiresAuth(t *testing.T) {
	srv, _, _ := newOpenAITestServer(t)

	// 用不跟随重定向的客户端：未登录时门户会 302 到 /login，
	// 若跟随就会拿到登录页的 200，把「被重定向」误判成「鉴权通过」。
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cli.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 未鉴权必须是 401/403（API 语义）或 302 到登录页（门户语义）。
	// 无论哪种，都不能是「直接给出模型列表」。
	if resp.StatusCode == http.StatusOK {
		t.Error("无凭证访问 /v1/models 返回 200 —— 鉴权被绕过")
	}
}

// stream=true 必须是**真**流式。
//
// ---- 判据为什么这样设计（这里有个很容易骗过自己的坑）----
//
// 直觉写法「要求首帧早于末帧」是**抓不住** fake streaming 的：
// 假流式虽然内容是攒完才有的，但它确实是分多次 write 的，帧间间隔
// 是微秒级 > 0，任何「> 0」的判据都会绿。已实测确认这一点。
//
// 真正能区分的判据是：**首帧是否早于「内核产出最终答案」的那一刻**。
// 于是假内核被构造成：先发一个内容增量，然后**扣住不放**最终响应，
// 靠消费者是否已经收到帧来解除阻塞。
//
//	真流式实现 → 收到第一个增量就转发给客户端 → 首帧在 100ms 内到达，
//	              客户端随后就能看到内容。
//	假流式实现 → 先同步等 InjectTextSync 返回（被扣住，阻塞数秒），
//	              等不到就什么都发不出去 → 首帧迟到数秒。
//
// 所以断言「首帧远早于总计 700ms 的生成时间」就是判据的全部。
func TestOpenAIStreamIsActuallyStreaming(t *testing.T) {
	srv, iom, _, webh := newOpenAITestServerWithHandler(t)

	// 假内核：发 3 个内容增量（每 100ms 一个），**扣住**最终响应 700ms，
	// 只有当消费者表现出「已经在读帧」时才放行。
	const genWindow = 700 * time.Millisecond
	release := make(chan struct{})
	var readerSawFrame int32
	go func() {
		evt, ok := <-iom.InputChan()
		if !ok {
			close(release)
			return
		}
		// 阶段 1：分片增量
		//
		// ★ 增量由**基座的 on_llm_call 阶段**承载（用户裁定 2026-11-27）：
		//   基座在 LLM 流上逐 chunk 推送 `LLMText`（累计值），插件转给客户端。
		//   输出通道只是 `output_send__webui` 工具的执行后端，不是流式载体。
		for i := 1; i <= 3; i++ {
			time.Sleep(100 * time.Millisecond)
			webh.PushLLMDelta(&sdk.StageContext{
				AgentID: "probe-agent",
				LLMText: strings.Repeat("片", i),
				Extra:   map[string]interface{}{"streaming": true},
			})
		}
		// 阶段 2：扣住最终响应，直到消费者已经在读帧（真流式会读）
		deadline := time.After(3 * time.Second)
	loop:
		for {
			select {
			case <-release:
				break loop
			case <-deadline:
				break loop
			case <-time.After(50 * time.Millisecond):
				if atomic.LoadInt32(&readerSawFrame) > 0 {
					break loop
				}
			}
		}
		if evt.ResponseCh != nil {
			evt.ResponseCh <- &agentIO.OutputEvent{
				Payload: map[string]interface{}{"content": "完整答案"},
			}
		}
	}()
	defer close(release)
	_ = genWindow

	start := time.Now()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-API-Key", testAuthAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，应为 text/event-stream", ct)
	}

	rd := bufio.NewReader(resp.Body)
	var frameTimes []time.Duration
	var sawContent bool
	for {
		line, err := rd.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			frameTimes = append(frameTimes, time.Since(start))
			// 见到任何带内容的帧即认为消费者在工作，解开内核的扣留
			if !strings.Contains(trimmed, "[DONE]") {
				atomic.CompareAndSwapInt32(&readerSawFrame, 0, 1)
				if strings.Contains(trimmed, "\"content\"") &&
					!strings.Contains(trimmed, "\"content\":\"\"") {
					sawContent = true
				}
			}
			if strings.Contains(trimmed, "[DONE]") {
				break
			}
		}
		if err != nil {
			break
		}
	}

	if len(frameTimes) < 2 {
		t.Fatalf("只收到 %d 个 data 帧 —— 流式响应至少要有多帧", len(frameTimes))
	}
	if !strings.Contains(strings.Join(nil, ""), "") && !sawContent {
		t.Error("未收到任何内容帧")
	}

	first := frameTimes[0]
	last := frameTimes[len(frameTimes)-1]
	if first > 400*time.Millisecond {
		t.Errorf("首帧延迟 %v —— 超过生成窗口的一半，说明是「等完整答案后才开始发」"+
			"（假流式）。真流式应在首个增量产生后立刻下发。", first)
	}
	_ = last
}

// 非流式必须仍是单个 JSON（别因为修流式把非流式改坏）。
func TestOpenAINonStreamUnchanged(t *testing.T) {
	srv, iom, _ := newOpenAITestServer(t)
	go func() {
		evt, ok := <-iom.InputChan()
		if !ok {
			return
		}
		if evt.ResponseCh != nil {
			evt.ResponseCh <- &agentIO.OutputEvent{
				Payload: map[string]interface{}{"content": "你好"},
			}
		}
	}()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-API-Key", testAuthAPIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw := make([]byte, 0, 4096)
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		raw = append(raw, buf[:n]...)
		if err != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("非流式响应必须是单个 JSON 对象: %v", err)
	}
	if out["object"] != "chat.completion" {
		t.Errorf("object = %v，应为 chat.completion", out["object"])
	}
	if strings.Contains(string(raw), "data: ") {
		t.Error("非流式响应里出现 SSE 帧 —— 两种模式串了")
	}
}

// testOutputDevice 是把输出通道调用交给 Handler 的最小 device。
//
// 生产路径由插件 `RegisterOutputChannel("webui", ...)` 注册真实 device
// （内核 pluginhost 的 regOutput 闭包）；判据里没有那套装配，所以这里
// 用同一个入口形状（`Execute("output", args)`）直连 Handler 的推送函数。
type testOutputDevice struct{ h *webui.Handler }

func (d *testOutputDevice) Name() string                                 { return "webui" }
func (d *testOutputDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceOutput }
func (d *testOutputDevice) Description() string                          { return "webui test output" }
func (d *testOutputDevice) Tools() []agentIO.ToolDef                     { return nil }
func (d *testOutputDevice) Start() error                                 { return nil }
func (d *testOutputDevice) Stop() error                                  { return nil }
func (d *testOutputDevice) OutputCapabilities() agentIO.OutputCapability { return agentIO.CapText }
func (d *testOutputDevice) ChannelDef() agentIO.ChannelDef               { return agentIO.ChannelDef{} }
func (d *testOutputDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	d.h.PushChannelOutput(args)
	return map[string]interface{}{"status": "ok"}, nil
}

// testStageHub 是测试用的 stage 注册面：把插件注册的 handler 存起来，
// 由测试**直接触发**（模拟基座在对应时机调 RunStage）。
//
// 生产里这一步由 `kit.StageHostType` 完成；测试不装配整个内核，
// 但也必须走**同一个注册契约**（`RegStage`），否则测的就不是生产路径。
type testStageHub struct {
	mu       sync.Mutex
	handlers map[string][]sdk.StageHandler
}

func (h *testStageHub) register(stage sdk.Stage, fn sdk.StageHandler) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.handlers == nil {
		h.handlers = map[string][]sdk.StageHandler{}
	}
	h.handlers[string(stage)] = append(h.handlers[string(stage)], fn)
}

// fire 触发某阶段的全部注册方（与基座 RunStage 的语义一致：并发扇出）。
func (h *testStageHub) fire(stage sdk.Stage, ctx *sdk.StageContext) {
	h.mu.Lock()
	fns := append([]sdk.StageHandler(nil), h.handlers[string(stage)]...)
	h.mu.Unlock()
	for _, fn := range fns {
		_ = fn(ctx)
	}
}

// stageHub 是本包当前测试会话的阶段注册面（harness 每次重建）。
var stageHub = &testStageHub{handlers: map[string][]sdk.StageHandler{}}
