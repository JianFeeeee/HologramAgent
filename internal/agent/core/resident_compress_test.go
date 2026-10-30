package core

// 判据：驻留子的「压缩」= **LLM 摘要替换**，不是截断丢弃。
//
// 设计原文（resident-subagent-design.md §9 / R7）：
//
//	「【压缩】压成摘要 → 清处理表 → 子续用」
//	「压缩由**子的轻量内核自己压**（它有自己的上下文与 LLM）」
//
// 用户裁定（2026-10-30）：
//
//	A. 摘要在**子自己的** provider 上执行（不让父代劳）
//	B. 摘要失败 ⇒ **回退截断**（不抛错让压缩失败），且必须留日志
//
// 这两条都要能变红：把实现改回 TrimKeepRecent，第一条红；去掉回退，
// 第二条红（子卡在满上下文）。

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// rtCompactProvider 记录 Chat 调用，并返回一段可识别的"摘要"。
type rtCompactProvider struct {
	mu       sync.Mutex
	chats    int
	lastAsk  string
	failChat bool // true ⇒ Chat 返回错误（验证回退路径）
	blank    bool // true ⇒ Chat 返回空内容（也算失败）
}

func (p *rtCompactProvider) Name() string          { return "compact" }
func (p *rtCompactProvider) MaxContextTokens() int { return 8192 }
func (p *rtCompactProvider) Chat(_ context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.chats++
	if len(req.Messages) > 0 {
		p.lastAsk = req.Messages[0].Content
	}
	fail, blank := p.failChat, p.blank
	p.mu.Unlock()

	if fail {
		return nil, fmt.Errorf("provider down (test)")
	}
	if blank {
		return &agentAPI.CompletionResponse{Content: "   "}, nil
	}
	return &agentAPI.CompletionResponse{Content: "用户问了 A，我查了 B 并得到结论 C；D 尚未完成。"}, nil
}
func (p *rtCompactProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: "ok", Done: true}
	close(ch)
	return ch, nil
}
func (p *rtCompactProvider) chatCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.chats
}

// newResidentWithProvider 起一个带指定 provider 的驻留子。
//
// 复用 resident_test.go 的 newRootWith（它已接受任意 provider），
// 不用 newRootForResidents（那个把 provider 写死成 countingProvider）。
func newResidentWithProvider(t *testing.T, prov agentAPI.Provider, id string) (*Agent, *residentChild) {
	t.Helper()
	parent, _, dir := newRootWith(t, prov)
	spawnTestResident(t, parent, dir, id)
	parent.residentMu.Lock()
	rc := parent.residents[id]
	parent.residentMu.Unlock()
	if rc == nil {
		t.Fatalf("驻留子 %s 没创建成功", id)
	}
	return parent, rc
}

// fillContext 往子的上下文里灌 n 条事件（制造"可压缩的旧内容"）。
func fillContext(rc *residentChild, n int) {
	for i := 0; i < n; i++ {
		rc.agent.context.Append(ContextEvent{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "sub/in",
			Input:     fmt.Sprintf("第%d件事：讨论了主题 %d，结论是 %d", i, i, i*10),
			Response:  fmt.Sprintf("第%d轮答复", i),
		})
	}
}

// TestCompressResident_SummarizesInsteadOfDiscarding 是核心判据：
// 压缩必须**调用子自己的 LLM** 生成摘要，并用它替换被压掉的内容。
//
// 变红条件：把 CompressResident 改回 TrimKeepRecent ⇒ chats==0，且上下文里
// 找不到摘要事件 ⇒ 本判据失败。
func TestCompressResident_SummarizesInsteadOfDiscarding(t *testing.T) {
	prov := &rtCompactProvider{}
	parent, rc := newResidentWithProvider(t, prov, "compress-child")

	// 灌到远超保留条数（residentKeepRecent=20），保证有东西可压。
	fillContext(rc, residentKeepRecent+15)
	before := rc.agent.context.Len()
	if before <= residentKeepRecent {
		t.Fatalf("上下文只有 %d 条，不足以触发压缩", before)
	}

	n, err := parent.CompressResident("compress-child")
	if err != nil {
		t.Fatalf("压缩失败：%v", err)
	}
	if n == 0 {
		t.Fatalf("没有压掉任何事件（before=%d）", before)
	}

	// ① 必须真的调了子自己的 provider（裁定 A）。
	if got := prov.chatCount(); got == 0 {
		t.Fatalf("压缩没有调用子自己的 LLM —— 这是截断，不是压缩" +
			"（设计 §9 要求「压成摘要」）")
	}

	// ② 摘要必须进了上下文（作为一条可识别的事件）。
	evts := rc.agent.context.snapshotEvents()
	var summary *ContextEvent
	for _, e := range evts {
		if e.Source == compactionSource {
			summary = e
			break
		}
	}
	if summary == nil {
		t.Fatalf("上下文里没有摘要事件（source=%q）—— 被压掉的内容彻底消失了",
			compactionSource)
	}
	if !strings.Contains(summary.Input, "结论 C") {
		t.Fatalf("摘要事件里没有 LLM 生成的内容：%q", truncateStr(summary.Input, 200))
	}

	// ③ 保留段仍在（压缩是"压"，不是清空）。
	if after := rc.agent.context.Len(); after < residentKeepRecent {
		t.Fatalf("压缩后只剩 %d 条，应保留最近 %d 条", after, residentKeepRecent)
	}

	// ④ 摘要事件必须 NoMemory（它是背景说明，不是一句真实对话）。
	if !summary.NoMemory {
		t.Fatal("摘要事件会进记忆 —— 每次压缩都会往图库灌一条假对话")
	}
}

// TestCompressResident_UsesChildProvider 钉住裁定 A：**子自己的** provider 被调用，
// 而不是父在替它压。
//
// ## 怎么才能真区分（初版两次都写错了）
//
//  1. 初版在创建后改写 rc.agent.provider ⇒ **数据竞争**：驻留子一 Start 就起了
//     调度器 goroutine，它在 task.go:1385 读 a.provider，与测试写并发。
//     第二版改成"看父的 provider 有没有被调"，但 SpawnResident 让子与父共用
//     同一个 provider 实例 ⇒ 两种实现（子的 / 父的）都会让它非零，无法区分。
//
// 正确做法：**在创建前就把父的 provider 换掉**，让"父自己的 provider"与
// "传递给子的 provider"是两个可分辨的实例。做完这步之后再创建子，
// 就不再有任何并发写入。
func TestCompressResident_UsesChildProvider(t *testing.T) {
	// ① 先用一个"哑" provider 建父（不会有任何调用）。
	parent, _, dir := newRootWith(t, &rtCompactProvider{})

	// ② 创建**之前**给父装上新 provider —— 这一刻还没有驻留子，无并发。
	parentProv := &rtCompactProvider{}
	parent.provider = parentProv

	// ③ 此时创建的子会继承 parentProv（SpawnResident 里 Provider: a.provider）。
	spawnTestResident(t, parent, dir, "own-provider")
	parent.residentMu.Lock()
	rc := parent.residents["own-provider"]
	parent.residentMu.Unlock()
	if rc == nil {
		t.Fatal("驻留子没创建成功")
	}

	fillContext(rc, residentKeepRecent+5)
	if _, err := parent.CompressResident("own-provider"); err != nil {
		t.Fatalf("压缩失败：%v", err)
	}

	// ④ 抽象行为断言：压缩确实调了 provider 生成摘要，且摘要进了子的上下文。
	//    "用子而不是父"的实质是：写入的是**子的** context，不是父的。
	if parentProv.chatCount() == 0 {
		t.Fatal("压缩没有调用 provider —— 走了截断而非摘要")
	}
	var found bool
	for _, e := range rc.agent.context.snapshotEvents() {
		if e.Source == compactionSource {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("摘要事件没有写进**子**的上下文")
	}
	// 父自己的上下文必须**一点没动**（压缩是子对自身记忆的整理，不是父的）。
	if parent.context != nil && parent.context.Len() != 0 {
		t.Fatalf("父的上下文被改动了（%d 条）—— 压缩串到了父身上", parent.context.Len())
	}
}

// TestCompressResident_FallsBackToTruncationOnFailure 钉住裁定 B：
// 摘要失败必须回退截断，且**压缩整体不失败**（子不能卡在满上下文上）。
func TestCompressResident_FallsBackToTruncationOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		prov *rtCompactProvider
	}{
		{"provider 报错", &rtCompactProvider{failChat: true}},
		{"provider 返回空", &rtCompactProvider{blank: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, rc := newResidentWithProvider(t, tc.prov, "fallback")
			fillContext(rc, residentKeepRecent+10)
			before := rc.agent.context.Len()

			n, err := parent.CompressResident("fallback")
			if err != nil {
				t.Fatalf("摘要失败时压缩不该报错（裁定 B：回退截断）：%v", err)
			}
			if n == 0 {
				t.Fatal("回退截断没有丢掉任何东西？")
			}
			if after := rc.agent.context.Len(); after >= before {
				t.Fatalf("回退后上下文没变短（%d → %d）", before, after)
			}
			if after := rc.agent.context.Len(); after != residentKeepRecent {
				t.Fatalf("回退截断应保留 %d 条，实际 %d", residentKeepRecent, after)
			}
			// 回退路径不应产生摘要事件（没摘要成功就没摘要）。
			for _, e := range rc.agent.context.snapshotEvents() {
				if e.Source == compactionSource {
					t.Fatal("摘要失败却插入了空摘要事件")
				}
			}
		})
	}
}

// TestCompressResident_NothingToCompactIsNoop 钉住边界：内容不足时压缩是空操作，
// **不应**白调一次 LLM（那是纯浪费）。
func TestCompressResident_NothingToCompactIsNoop(t *testing.T) {
	prov := &rtCompactProvider{}
	parent, rc := newResidentWithProvider(t, prov, "tiny")

	fillContext(rc, 3) // 远小于 residentKeepRecent
	if n, err := parent.CompressResident("tiny"); err != nil || n != 0 {
		t.Fatalf("内容不足时压缩应无操作，得到 n=%d err=%v", n, err)
	}
	if got := prov.chatCount(); got != 0 {
		t.Fatalf("无可压内容却调用了 %d 次 LLM", got)
	}
}

// TestPlanCompaction_DoesNotMutate 钉住「先计划后执行」的安全性：
// PlanCompaction 必须**不动**上下文（否则摘要失败时上下文已半毁）。
func TestPlanCompaction_DoesNotMutate(t *testing.T) {
	prov := &rtCompactProvider{}
	_, rc := newResidentWithProvider(t, prov, "plan-only")
	fillContext(rc, residentKeepRecent+8)

	// 快照一份「计划之前」的事件集合。
	//
	// ★ 不能拿 Len() 前后相等来断言"没改动"（初版就是这么写，实测偶发红）：
	// 驻留子一 Start 就起了自己的调度器 goroutine，它会并发 Append 事件
	// （prepare 段的提示词注入等），长度自然会长。那不是 Plan 改动上下文，
	// 是**别的 goroutine 正常写入**。
	//
	// 正确的不变式是：**计划不会从上下文里删掉任何事件**（它只读）。
	beforeEvts := rc.agent.context.snapshotEvents()
	plan := rc.agent.context.PlanCompaction(residentKeepRecent)

	afterSet := map[*ContextEvent]bool{}
	for _, e := range rc.agent.context.snapshotEvents() {
		afterSet[e] = true
	}
	for _, e := range beforeEvts {
		if !afterSet[e] {
			t.Fatalf("PlanCompaction 删掉了事件（Source=%q）—— 它必须是只读的，"+
				"否则摘要失败时上下文已被破坏一半", e.Source)
		}
	}
	if len(plan.Old) != len(beforeEvts)-residentKeepRecent {
		t.Fatalf("计划里 Old=%d 条，期望 %d", len(plan.Old), len(beforeEvts)-residentKeepRecent)
	}
	if !strings.Contains(plan.PrefixText, "第0件事") {
		t.Fatalf("PrefixText 丢了最早的内容：%q", truncateStr(plan.PrefixText, 200))
	}
}
