package core

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// captureLog 捕获 log 包在 fn 执行期间的全部输出。
//
// 注意：log 的输出目标是**全局**的，所以此 helper 不可与 t.Parallel() 同用。
// 本文件不并跑任何测试，故安全。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

// scriptedChildProvider 按脚本逐轮返回：先给定批次的工具调用，最后给文本。
//
// 与 stubProvider 的区别：stub 每轮返回同一段文本（无法表达「先调工具、再收尾」
// 的多轮序列），而本轮修复的正确性**恰恰只在多轮序列里才显现**。
type scriptedChildProvider struct {
	// toolTurns 是前 N 轮各自要返回的工具调用批次。
	toolTurns [][]agentAPI.ToolCall
	// finalText 是所有工具轮用尽后的文本答复（若为 "" 则永不返回文本，
	// 用来构造「跑满预算仍未产出答复」这一路径）。
	finalText string
	// salvageText 是收尾调用（ToolChoice=none）的答复。
	salvageText string

	calls      int
	sawSalvage bool
}

func (p *scriptedChildProvider) Name() string { return "scripted" }

func (p *scriptedChildProvider) Chat(_ context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.calls++

	// 收尾调用：子 Agent 跑满预算后补的那一次（ToolChoice=none）。
	if req.ToolChoice == "none" {
		p.sawSalvage = true
		return &agentAPI.CompletionResponse{Content: p.salvageText}, nil
	}

	// 工具轮：按脚本发工具调用。
	if p.calls <= len(p.toolTurns) {
		return &agentAPI.CompletionResponse{ToolCalls: p.toolTurns[p.calls-1]}, nil
	}
	return &agentAPI.CompletionResponse{Content: p.finalText}, nil
}

func (p *scriptedChildProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk)
	close(ch)
	return ch, nil
}

func (p *scriptedChildProvider) MaxContextTokens() int { return 8192 }

// fakeToolCall 造一个指向不存在工具的工具调用。
//
// 用不存在的工具是**有意的**：本组测试只关心轮次与收尾逻辑，不该依赖任何真实
// 工具或插件；未知工具会走到「未注册」分支并返回一段文本，足以驱动循环继续。
func fakeToolCall(i int) agentAPI.ToolCall {
	return agentAPI.ToolCall{
		ID:        fmt.Sprintf("call_%d", i),
		Name:      fmt.Sprintf("nonexistent_tool_%d", i),
		Arguments: map[string]interface{}{},
	}
}

// newSpawnAgent 造一个跑 runChildTask 所需的最小 Agent。
//
// io 必须接上：executeToolCallInner 会在设备工具授权闸上读 a.io（解引用 nil 会直接
// SIGSEGV）。用一个空 IOManager 即可——本组测试只用未知工具名，不会命中真实通道。
func newSpawnAgent(t *testing.T, p agentAPI.Provider) *Agent {
	t.Helper()
	a := New(AgentConfig{ID: "child-test", Provider: p})
	a.io = agentIO.NewIOManager()
	a.childTasks = make(map[string]*childTaskState)
	return a
}

// runChildAndGet 登记任务、跑完 runChildTask，再取回结果。
//
// 必须通过同一注册路径：runChildTask 结尾会读 a.childTasks[taskID] 回写结果，
// 未登记就直接调用会让测试自己 nil 解引用（而非被测代码出错）。
func runChildAndGet(t *testing.T, a *Agent, provider agentAPI.Provider, taskID, task string, maxTurns int) string {
	t.Helper()
	a.provider = provider
	a.childMu.Lock()
	a.childTasks[taskID] = &childTaskState{running: true}
	a.childMu.Unlock()

	a.runChildTask(taskID, task, "cli", maxTurns)

	a.childMu.Lock()
	st := a.childTasks[taskID]
	a.childMu.Unlock()
	if st == nil {
		t.Fatalf("任务 %s 未在注册表中（不应被淘汰或丢失）", taskID)
	}
	return st.result
}

// 跑满轮预算时，报告必须**如实说明实际轮数**，不能硬编码成别的数字。
//
// 这是本次修复的核心判据。生产实证：max_turns=10 的子 Agent 跑满 10 轮，
// 报告却写「超过 5 轮」——那个假数字会把排查引向错误方向（我曾据此去查
// 「为什么是 5」，而真实原因与 5 毫无关系）。
func TestChildReachingTurnLimitReportsActualMaxTurns(t *testing.T) {
	for _, maxTurns := range []int{1, 5, 10, 14} {
		t.Run(fmt.Sprintf("max_turns=%d", maxTurns), func(t *testing.T) {
			// 每一轮都返回工具调用 ⇒ 永不产出最终文本 ⇒ 必然跑满预算。
			turns := make([][]agentAPI.ToolCall, maxTurns)
			for i := range turns {
				turns[i] = []agentAPI.ToolCall{fakeToolCall(i)}
			}
			// salvage 也返回空 ⇒ 走「诚实陈述」分支
			p := &scriptedChildProvider{toolTurns: turns, finalText: "", salvageText: ""}
			a := newSpawnAgent(t, p)

			got := runChildAndGet(t, a, p, "child_x", "test task", maxTurns)

			// 判据一：必须出现**真实的**轮数。
			if !strings.Contains(got, fmt.Sprintf("%d 轮", maxTurns)) {
				t.Fatalf("max_turns=%d 的报告应含真实轮数，实际: %q", maxTurns, got)
			}
			// 判据二：绝不能出现硬编码的「5 轮」（除非 maxTurns 恰为 5）。
			if maxTurns != 5 && strings.Contains(got, "5 轮") {
				t.Fatalf("max_turns=%d 时报告出现了陈旧的「5 轮」: %q", maxTurns, got)
			}
			// 判据三：不得再谎称「执行超时」（它不是超时，是预算耗尽）。
			if strings.Contains(got, "超时") {
				t.Fatalf("跑满预算不是超时，不应如此描述: %q", got)
			}
		})
	}
}

// 跑满预算时不得丢弃中间成果：应收尾提炼出已取得的内容。
//
// 丢弃的代价是父 Agent 只能重 spawn（生产日志里 child_5→6→7 是同一任务连试三次），
// 每次重烧一整个上下文——比多做一次收尾调用贵得多。
func TestChildReachingTurnLimitSalvagesPartialResults(t *testing.T) {
	p := &scriptedChildProvider{
		toolTurns:   [][]agentAPI.ToolCall{{fakeToolCall(0)}, {fakeToolCall(1)}},
		salvageText: "已查明：广州大学复试科目为数据结构；上海大学未取到正文。",
	}
	a := newSpawnAgent(t, p)

	got := runChildAndGet(t, a, p, "child_s", "查两所高校", 2)

	if !p.sawSalvage {
		t.Fatal("跑满预算后应发起一次收尾调用（ToolChoice=none）")
	}

	if !strings.Contains(got, "广州大学复试科目为数据结构") {
		t.Fatalf("收尾提炼的成果应被保留，实际: %q", got)
	}
	if !strings.Contains(got, "可能不完整") {
		t.Fatalf("收尾结果必须标注可能不完整（否则父 Agent 会当成完整结论）: %q", got)
	}
}

// 正常完成（模型主动给文本）时，**不得**触发收尾调用。
//
// 否则每次成功的子任务都要多花一次 LLM 调用——那是纯浪费。
func TestChildNormalCompletionDoesNotSalvage(t *testing.T) {
	p := &scriptedChildProvider{
		toolTurns:   [][]agentAPI.ToolCall{{fakeToolCall(0)}},
		finalText:   "任务完成：已写入 3 条记录。",
		salvageText: "不应被调用",
	}
	a := newSpawnAgent(t, p)

	got := runChildAndGet(t, a, p, "child_n", "写记录", 10)

	if p.sawSalvage {
		t.Fatal("正常完成时不应发起收尾调用（那是多余的 LLM 调用）")
	}

	if got != "任务完成：已写入 3 条记录。" {
		t.Fatalf("正常结果应原样返回，实际: %q", got)
	}
}

// 轮上限为 1：一轮就耗尽，收尾仍必须工作（边界）。
func TestChildMaxTurnsOne(t *testing.T) {
	p := &scriptedChildProvider{
		toolTurns:   [][]agentAPI.ToolCall{{fakeToolCall(0)}},
		salvageText: "部分结果",
	}
	a := newSpawnAgent(t, p)

	got := runChildAndGet(t, a, p, "child_1t", "任务", 1)

	if !strings.Contains(got, "1 轮") {
		t.Fatalf("max_turns=1 应如实报告 1 轮，实际: %q", got)
	}
}

// 子 Agent 的工具调用必须落日志——否则失败时无法判断倒在哪一步。
//
// 此前子侧全程静默，父内核日志里只有 started/done 两行，
// 无法区分「搜索无结果」「页面解析失败」还是「被轮上限截断」。
func TestChildToolCallsAreLogged(t *testing.T) {
	p := &scriptedChildProvider{
		toolTurns: [][]agentAPI.ToolCall{{fakeToolCall(0)}},
		finalText: "done",
	}
	a := newSpawnAgent(t, p)

	logs := captureLog(t, func() {
		runChildAndGet(t, a, p, "child_log", "任务", 5)
	})

	if !strings.Contains(logs, "child_log") {
		t.Fatalf("子 Agent 日志应带自己的 taskID，实际日志: %q", logs)
	}
	if !strings.Contains(logs, "turn 1/5") {
		t.Fatalf("子 Agent 的工具调用应带轮次与预算，实际日志: %q", logs)
	}
	if !strings.Contains(logs, "nonexistent_tool_0") {
		t.Fatalf("子 Agent 日志应含工具名，实际日志: %q", logs)
	}
}

// 非驻留子的提示词必须包含「执行纪律」与「工具执行顺序」两节。
//
// 为什么需要这条判据：这两节缺失时子不会报错、也不会崩——它只是**静静地少干活**，
// 把「我打算做什么」当成完成任务交回父（生产日志：child_1/3/4/5 都交回将来时计划）。
// 这类退化没有其它可观测信号，只能靠判据钉住。
func TestChildSystemPromptEnforcesExecutionDiscipline(t *testing.T) {
	rendered := fmt.Sprintf(childSystemPromptTemplate, "测试任务")

	// 判据一：必须要求「实际调用工具」，且明确否定「只回复将要做」。
	if !strings.Contains(rendered, "必须实际调用工具") {
		t.Fatal("子提示词缺少「必须实际调用工具」的硬性要求（子会退化成只交回计划）")
	}
	if !strings.Contains(rendered, "宣告计划不等于完成任务") {
		t.Fatal("子提示词必须显式否定「宣告计划即完成」这一行为")
	}
	// 判据二：必须说明同轮并行语义与「依赖结果时分轮」。
	if !strings.Contains(rendered, "默认并行执行") {
		t.Fatal("子提示词缺少「同轮默认并行」的说明")
	}
	if !strings.Contains(rendered, "分两轮") {
		t.Fatal("子提示词必须告诉子：依赖前一步结果时要分两轮")
	}
	// 判据三：任务描述必须真的被插进去（模板参数位不能错位）。
	if !strings.Contains(rendered, "任务: 测试任务") {
		t.Fatal("任务描述未被正确插入模板")
	}
	// 判据四：不得把父专属的输出指引搬进来——子被禁用 output_send__*，
	// 告知它「异步通道必须 output_send」是错误且白耗预算的指引。
	if strings.Contains(rendered, "output_send__") {
		t.Fatal("子提示词不应包含输出通道指引（子已被禁用 output_send__*，结果回父而非用户）")
	}
}

// 子提示词模板必须正好一个 %s 占位（多了/少了都会让 Sprintf 出错或插错位置）。
func TestChildSystemPromptTemplateHasSingleVerbatimSlot(t *testing.T) {
	if n := strings.Count(childSystemPromptTemplate, "%s"); n != 1 {
		t.Fatalf("模板应有且仅有一个 %%s 占位，实际 %d 个", n)
	}
	if !strings.HasSuffix(childSystemPromptTemplate, "任务: %s") {
		t.Fatal("任务描述应位于模板末尾（否则会被后续章节挤到中间，读起来像背景而非指令）")
	}
}
