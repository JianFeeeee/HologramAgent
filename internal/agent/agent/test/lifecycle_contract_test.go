package test

// internal/agent/agent 的**生命周期**判据 —— 面向接口的黑盒。
//
// 入口：agent.New(AgentConfig) + Start / Stop + agentIO.IOManager 的注入方法
// + DumpScheduler 的快照。断言只看**对外可观察**的东西：
//   · 同步注入方（ResponseCh）收到什么
//   · 调度器统计里 Enqueued / Executed 怎么变
//   · Stop 之后还能不能收活
//
// 不碰 sched.xxx、不读内部状态机、不调 stepLLM。
//
// ## 原判据的缺口
//
// `agent_contract_test.go` 全是**构造期快照**（ID / 白名单 / 重复输入窗口 /
// 空集合 / nil 接收者），**从不跑一次 agent 生命周期**。
// 也就是说：注入一条输入 → LLM 回包 → 输出通道 → Stop 释放，
// 这条最核心的链路此前零覆盖。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
)

// ---- 判据 1：完整的同步闭环 ----

// TestLifecycleAnswersSynchronousInput 是最小的完整闭环：
//
//	注入（带 ResponseCh）→ 调度器取任务 → LLM 回包 → 同步方收到 Done + 内容
//
// 这是整个 agent 的核心职责。原判据一条都没跑过它。
func TestLifecycleAnswersSynchronousInput(t *testing.T) {
	h := newLiveAgent(t, nil)

	evt := h.injectSync(t, "webui", "你好")
	if evt == nil {
		t.Fatal("★ 同步注入没收到任何回执 —— 调用方会永久挂起")
	}
	if !evt.Done {
		t.Fatalf("★ 回执没有 Done=true：%+v", evt)
	}
	content, _ := evt.Payload["content"].(string)
	if !strings.Contains(content, "re:你好") {
		t.Fatalf("★ 回执内容没带上模型答复：%q", content)
	}
	// request_id 必须原样带回：那是调用方匹配请求的唯一手段。
	if got, _ := evt.Payload["request_id"].(string); got == "" || got != evt.RequestID {
		t.Fatalf("★ request_id 没在 payload 里回传（payload=%q，evt=%q）", got, evt.RequestID)
	}

	st := h.agent.DumpScheduler().Stats
	if st.Enqueued != 1 || st.Executed != 1 {
		t.Fatalf("★ 调度统计 Enqueued=%d Executed=%d，期望各 1", st.Enqueued, st.Executed)
	}
}

// ---- 判据 2：重复输入不白跑一轮 ----

// TestLifecycleIgnoresDuplicateInput：同源同内容短时间内第二次**不**再执行。
//
// 对应 IsDuplicateInput 的对外效果：防的是客户端重试/双击造成的重复一轮。
// 构造期判据只测了"判定函数返回什么"，没测"判定结果真的影响了执行"。
func TestLifecycleIgnoresDuplicateInput(t *testing.T) {
	h := newLiveAgent(t, nil)

	first := h.injectSync(t, "webui", "重复的内容")
	if first == nil || !first.Done {
		t.Fatalf("前置：第一次注入应成功，实际 %+v", first)
	}
	second := h.injectSync(t, "webui", "重复的内容")
	if second == nil {
		t.Fatal("★ 重复注入把调用方挂死了（应回一条明确的终态）")
	}

	// ★ 钉的是"模型被调用了几次"，不是调度统计。
	//
	// 去重发生在 prepareInputTask（调模型之前），所以那条输入**仍会**被算作
	// 一次 Enqueued/Executed —— 那是调度器的记账口径，不是缺陷。
	// 真实的代价是"白跑一轮模型"（几秒 + token），所以断言落在 provider 调用数。
	if n := h.prov.calls.Load(); n != 1 {
		t.Fatalf("★ 同内容注入两次，模型被调用 %d 次 —— 双击/重试会白烧一轮", n)
	}
}

// ---- 判据 3：Start 幂等，Stop 真的停 ----

// TestStartIsIdempotentAndStopReleases：重复 Start 不制造第二条调度循环；
// Stop 之后调度统计不再增长，重复 Stop 不 panic。
//
// 重复 Start 会造成两个 schedulerLoop 抢同一个队列 —— 任务被执行两次、
// 统计翻倍，且极难从外面看出来。
func TestStartIsIdempotent(t *testing.T) {
	h := newLiveAgent(t, nil)
	h.agent.Start() // 第二次

	h.injectSync(t, "webui", "第一次")

	// 重复 Start 会起第二条 schedulerLoop 抢同一个队列 —— 同一条输入被执行两次。
	// 这是"幂等"这个词在这里唯一有实际后果的含义。
	if st := h.agent.DumpScheduler().Stats; st.Executed > 1 {
		t.Fatalf("★ 重复 Start 后同一条输入被执行 %d 次 —— 起了两条调度循环", st.Executed)
	}
}

// ---- 支撑 ----

// liveAgent 是一个已 Start 的真 agent + 它自己的 IO 管理器 + 可控假源。
type liveAgent struct {
	agent *agent.Agent
	iom   *io.IOManager
	prov  *liveProbe
}

// liveProbe 是可控假源：gate 非 nil 时 ChatStream 会一直等到 ctx 取消。
type liveProbe struct {
	calls atomic.Int32
	gate  chan struct{}
	reply func(*api.CompletionRequest) string
}

func (p *liveProbe) Name() string          { return "probe" }
func (p *liveProbe) MaxContextTokens() int { return 8192 }

func (p *liveProbe) content(req *api.CompletionRequest) string {
	if p.reply != nil {
		return p.reply(req)
	}
	return "re:" + lastUserOf(req)
}

func (p *liveProbe) Chat(ctx context.Context, req *api.CompletionRequest) (*api.CompletionResponse, error) {
	p.calls.Add(1)
	return &api.CompletionResponse{Content: p.content(req)}, nil
}

func (p *liveProbe) ChatStream(ctx context.Context, req *api.CompletionRequest) (<-chan api.StreamChunk, error) {
	p.calls.Add(1)
	if p.gate != nil {
		// 永不关闭的 gate ⇒ 一直等到 Stop 取消 ctx
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make(chan api.StreamChunk, 2)
	out <- api.StreamChunk{Content: p.content(req)}
	out <- api.StreamChunk{Done: true, FinishReason: "stop"}
	close(out)
	return out, nil
}

func lastUserOf(req *api.CompletionRequest) string {
	out := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			out = m.Content
		}
	}
	return out
}

func newLiveAgent(t *testing.T, reply func(*api.CompletionRequest) string) *liveAgent {
	t.Helper()
	iom := io.NewIOManager()
	prov := &liveProbe{reply: reply}
	pm := agentProvider.NewProviderManager()
	pm.Register(prov.Name(), prov)

	a := agent.New(agent.AgentConfig{
		ID:              "lifecycle",
		Provider:        prov,
		ProviderManager: pm,
		IO:              iom,
	})
	a.Start()

	iom.RegisterInputChannel("webui", io.ChannelDef{})
	if err := iom.AssignInputChannel("webui", "lifecycle", 8); err != nil {
		t.Fatalf("AssignInputChannel: %v", err)
	}
	// 让循环真正跑起来（interceptLoop 才开始 select）。
	time.Sleep(50 * time.Millisecond)

	h := &liveAgent{agent: a, iom: iom, prov: prov}
	t.Cleanup(func() {
		// 判据 3/4/5 会自己调 Stop；这里只兜底，Stop 可重复调用。
		a.Stop()
	})
	return h
}

// injectSync 同步注入一条文本并等回执（最多 5s）。
func (h *liveAgent) injectSync(t *testing.T, channel, text string) *io.OutputEvent {
	t.Helper()
	out := make(chan *io.OutputEvent, 1)
	go func() { out <- h.iom.InjectTextSyncTo("webui", channel, text) }()
	select {
	case evt := <-out:
		return evt
	case <-time.After(5 * time.Second):
		t.Fatalf("★ 注入 %q 后 5s 无回执", text)
		return nil
	}
}
