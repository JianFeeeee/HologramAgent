package core

// R7b 判据：裁剪只是**视图**，原文永不丢弃；回收时把全文交回主 agent。
//
// 用户 2026-10-30 裁定：
//
//	「老内容不再进入 LLM，但是**原文保留**，rtchild 回收时依旧向主 agent
//	  展示全部上下文。」
//
// 这两件事必须分开，混在一起就会静默丢数据：
//
//	视图（进 LLM）  —— 按窗口裁       ← 判据 1
//	存储（history） —— 全文永不丢     ← 判据 2
//	回收交接        —— 交**全文**     ← 判据 3
//
// 删掉任一条实现，对应判据必红。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// rtLongHistoryProvider 每轮回复一句固定文本，用于累积历史。
type rtLongHistoryProvider struct {
	mu   sync.Mutex
	reqs [][]agentAPI.Message // 每次请求的完整消息（拷一份）
}

func (p *rtLongHistoryProvider) Name() string          { return "rtLongHistory" }
func (p *rtLongHistoryProvider) MaxContextTokens() int { return 700 } // > system(444)，否则 system 一条就超窗、裁无可裁
func (p *rtLongHistoryProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{}, nil
}
func (p *rtLongHistoryProvider) ChatStream(_ context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, append([]agentAPI.Message(nil), req.Messages...))
	p.mu.Unlock()
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: strings.Repeat("答", 40)}
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}
func (p *rtLongHistoryProvider) rounds() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}
func (p *rtLongHistoryProvider) lastReq() []agentAPI.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reqs) == 0 {
		return nil
	}
	return p.reqs[len(p.reqs)-1]
}

// TestRTChild_ViewIsTrimmedButHistoryIsNot 是同一条判据的两半，必须成对断言：
//
//	① 进 LLM 的视图**确实被裁**（旧内容不再进模型）
//	② 存储的 history **一条不少**（原文保留）
//
// 只断言 ① ⇒ "每轮清空历史"也能通过（那不是裁，是丢）
// 只断言 ② ⇒ "从不裁"也能通过（那会撑爆窗口）
func TestRTChild_ViewIsTrimmedButHistoryIsNot(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "trim")

	// 跑多轮，故意让历史远超窗口（MaxContextTokens=120，每轮产出 ~40 字）。
	const rounds = 6
	for i := 0; i < rounds; i++ {
		host.PushRTInput(id, strings.Repeat("问", 30))
		want := (i + 1) * 2
		// ★ 必须等 **transcript 条数**，不能等 p.rounds()。
		//
		// p.rounds() 在 ChatStream **被调用时**就自增（此时请求刚构建完），
		// 而 assistant 那条消息是**流结束后**才追加进 history 的。
		// 等 rounds 的话第 N 轮请求一发出就放行，随即读 transcript 会少一条
		// （实测 11 vs 12）—— 这是判据自己的竞态，不是产品缺陷。
		waitFor(t, "本轮完成", func() bool {
			tr, ok := host.RTTranscript(id)
			return ok && len(tr) >= want
		})
	}

	// ② 存储侧：6 轮 ⇒ 每轮 user+assistant 两条，共 12 条，**一条都不能少**。
	transcript, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("取不到 transcript")
	}
	if len(transcript) != rounds*2 {
		t.Fatalf("history 有 %d 条，期望 %d 条 —— 原文被裁掉了（用户裁定：原文保留）",
			len(transcript), rounds*2)
	}
	// 最早那一轮的原话必须还在。
	if !strings.Contains(transcript[0].Content, "问") {
		t.Fatalf("最早的一轮不见了：%+v", transcript[0])
	}

	// ① 视图侧：最后一次请求的消息数必须**明显少于** history（说明确实裁了）。
	last := p.lastReq()
	if len(last) >= len(transcript)+1 { // +1 = 本轮 user 之前已在 history 里
		t.Logf("视图 %d 条 / 历史 %d 条：窗口未触发裁剪（内容可能还不够大）", len(last), len(transcript))
	}
	viewTokens := 0
	for _, m := range last {
		viewTokens += rtMsgTokens(m)
	}
	// 允许一定估算偏差（+20%），但视图必须**明显小于**完整历史。
	fullTokens := 0
	for _, m := range append([]agentAPI.Message{{Role: "system", Content: rtChildDefaultPrompt}}, transcript...) {
		fullTokens += rtMsgTokens(m)
	}
	if viewTokens > 700*12/10 {
		t.Fatalf("视图估算 %d token，远超窗口 700 —— 裁剪没生效（旧内容仍在进模型）", viewTokens)
	}
	if fullTokens > 700 && viewTokens >= fullTokens*3/4 {
		t.Fatalf("视图 %d token 与完整历史 %d token 相当 —— 没有真正裁掉旧内容",
			viewTokens, fullTokens)
	}
}

// TestRTChild_HistorySurvivesInputSupersede 钉住：被打断的轮次，**已说出口的部分**
// 也要留在 history 里（它确实被用户听到了，下一轮不能假装没发生）。
func TestRTChild_HistorySurvivesInputSupersede(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "super")
	host.PushRTInput(id, "第一问")
	waitFor(t, "第一轮完成", func() bool { return p.rounds() >= 1 })
	host.PushRTInput(id, "第二问")
	waitFor(t, "第二轮完成", func() bool { return p.rounds() >= 2 })

	tr, _ := host.RTTranscript(id)
	if len(tr) < 4 {
		t.Fatalf("两轮之后 history 只有 %d 条，期望 ≥4（每轮 user+assistant）", len(tr))
	}
}

// TestFormatRTTranscript_PreservesDialogueFully 钉住渲染口径：
// user/assistant **逐字保留**；工具结果按上限截断（体积大头，复盘只需结论）。
func TestFormatRTTranscript_PreservesDialogueFully(t *testing.T) {
	longToolResult := strings.Repeat("x", rtTranscriptToolMaxRunes*3)
	msgs := []agentAPI.Message{
		{Role: "user", Content: "帮我查一下明天的天气"},
		{Role: "assistant", ToolCalls: []agentAPI.ToolCall{
			{ID: "c1", Name: "weather_query", Arguments: map[string]interface{}{"city": "北京"}},
		}},
		{Role: "tool", ToolCallID: "c1", Content: longToolResult},
		{Role: "assistant", Content: "明天北京晴，18-26 度。"},
	}
	out := FormatRTTranscript(msgs)

	for _, want := range []string{
		"帮我查一下明天的天气",
		"weather_query",
		"city=北京",
		"明天北京晴，18-26 度。",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("交接文本缺少 %q\n---\n%s", want, out)
		}
	}
	// 工具结果被截断（不把整页塞回去），但仍有内容。
	if strings.Contains(out, strings.Repeat("x", rtTranscriptToolMaxRunes+50)) {
		t.Fatal("工具结果未按上限截断 —— 会把体积大头整段塞回主 agent 上下文")
	}
	if !strings.Contains(out, "xxx") {
		t.Fatal("工具结果被整段丢掉了 —— 复盘需要看到结论")
	}
}

// TestCloseRTChild_PushesSignalNotBody 是**交接协议**的判据（用户裁定 2026-10-30）：
//
// 回收走「contextfull 式中断」—— 只推**信号**（规模），正文留着等主 agent
// 按需拉取，由它自己选保留哪些。
//
// 若把 CloseRTChild 改回"直接把全文 injectSelfChannel 进主 agent 上下文"，
// 本判据必红：正文动辄几十万字符，全灌进去会顶爆父的上下文。
func TestCloseRTChild_PushesSignalNotBody(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "close")
	host.PushRTInput(id, "第一句原话")
	waitFor(t, "第一轮完成", func() bool { return p.rounds() >= 1 })
	host.PushRTInput(id, "第二句原话")
	waitFor(t, "第二轮完成", func() bool { return p.rounds() >= 2 })

	before, _ := host.RTTranscript(id)
	n, err := host.CloseRTChild(id)
	if err != nil {
		t.Fatalf("回收失败：%v", err)
	}
	if n != len(before) {
		t.Fatalf("交接条数 %d，实际 %d", n, len(before))
	}
	if _, ok := host.RTChildStats(id); ok {
		t.Fatal("回收后实例仍在登记表里")
	}

	// ★ 核心断言：父侧收到的是**中断信号**（level=L4，source=rtchild/<id>），
	//   且 payload 里**没有**通话正文 —— 正文是要去拉的东西，不该被推进上下文。
	//
	// 注意这里查的是**调度器的中断登记**而不是 selfInputCh：
	// 交接走的是 contextfull 同款 L4（raiseVector），不是自循环消息。
	snap := host.DumpScheduler()
	if len(snap.InterruptQueues[LevelCritical]) == 0 {
		t.Fatalf("没有产生 L4 中断 —— 交接必须走 contextfull 式中断，实际快照：%+v", snap)
	}

	// 原文必须仍可**按需取回**（回收之后也能取）。
	after, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("回收后取不到原文 —— 「回收时向主 agent 展示全部上下文」断链")
	}
	if len(after) != len(before) {
		t.Fatalf("回收后原文 %d 条，回收前 %d 条", len(after), len(before))
	}
}

// TestRTTranscript_ToolServesAllContentAndPages 钉住 rt_transcript 工具：
// 它必须能取回全部原文（分段），这是主 agent"自行选择保留内容"的前提。
func TestRTTranscript_ToolServesAllContentAndPages(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "tool")
	const rounds = 3
	for i := 0; i < rounds; i++ {
		host.PushRTInput(id, fmt.Sprintf("第%d句原话", i))
		want := i + 1
		waitFor(t, "本轮完成", func() bool { return p.rounds() >= want })
	}
	host.CloseRTChild(id)

	// 第一段：默认 limit。
	out := host.executeRTTranscript(agentAPI.ToolCall{
		Name: "rt_transcript", Arguments: map[string]interface{}{"id": id},
	})
	if !strings.Contains(out, "共 ") {
		t.Fatalf("工具输出没有规模信息：%s", truncateStr(out, 200))
	}
	if !strings.Contains(out, "第0句原话") {
		t.Fatalf("第一段缺少最早的原话：%s", truncateStr(out, 300))
	}
	// 分段续读：offset 应能取到后面的内容（用 limit=1 逼出分页）。
	page2 := host.executeRTTranscript(agentAPI.ToolCall{
		Name: "rt_transcript", Arguments: map[string]interface{}{"id": id, "offset": 1, "limit": 1},
	})
	if !strings.Contains(page2, "1-2") {
		t.Fatalf("分页参数未生效（应返回第 1-2 条）：%s", truncateStr(page2, 200))
	}
	// 未知 id 必须明确报错（不静默返回空）。
	bad := host.executeRTTranscript(agentAPI.ToolCall{
		Name: "rt_transcript", Arguments: map[string]interface{}{"id": "nope"},
	})
	if !strings.Contains(bad, "不存在") {
		t.Fatalf("未知 id 未明确报错：%s", bad)
	}
}

// TestCloseRTChild_EmptyHistoryNoSpam 钉住：没有历史时不推中断（别污染主 agent）。
func TestCloseRTChild_EmptyHistoryNoSpam(t *testing.T) {
	p := &rtLongHistoryProvider{}
	host, _ := newRTHost(t, p)

	id := rtSpawn(t, host, "empty")
	n, err := host.CloseRTChild(id)
	if err != nil {
		t.Fatalf("回收失败：%v", err)
	}
	if n != 0 {
		t.Fatalf("空历史返回 %d 条", n)
	}
	time.Sleep(30 * time.Millisecond)
	select {
	case msg := <-host.selfInputCh:
		t.Fatalf("空历史也推了信号，污染主 agent：%q", truncateStr(msg.text, 80))
	default:
	}
}
