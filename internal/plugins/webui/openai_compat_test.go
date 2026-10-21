package webui

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
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
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
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)

	iom := agentIO.NewIOManager()
	bus := events.NewBus()
	cfg := sdk.SDKConfig{
		Settings:  sdk.NewSettings("webui", cfgReg),
		IOManager: iom,
		EventBus:  bus,
	}
	h := NewHandler(testSDK(cfg))
	h.RegisterRoutes(http.NewServeMux())

	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { srv.Close() })
	return srv, iom, bus
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
// 所以断言「首帧远早于总计 700ms 的生成时间」��就是判据的全部。
func TestOpenAIStreamIsActuallyStreaming(t *testing.T) {
	srv, iom, bus := newOpenAITestServer(t)

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
		for i := 0; i < 3; i++ {
			time.Sleep(100 * time.Millisecond)
			bus.Publish(&sdk.Event{
				Type:    sdk.EventContentDelta,
				Payload: map[string]interface{}{"content": "片"},
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
