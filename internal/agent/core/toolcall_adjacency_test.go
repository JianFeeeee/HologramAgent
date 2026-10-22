package core

// 本文件钉死「assistant(tool_calls) 与其 tool 结果消息的**相邻性**」。
//
// ## 为何单独立这一组（2026-10-13）
//
// 生产报错（llmsproxy tier 1，持续复现）：
//
//	api error 400: An assistant message with 'tool_calls' must be followed by
//	tool messages responding to each 'tool_call_id'.
//	(insufficient tool messages following tool_calls message)
//
// 仓内已有 TestBatchToolCallIDsAllPaired，但它只断言**数量**配对，
// 注释还明确写着「不断言消息的物理排列」。而 OpenAI 协议要求的是
// **assistant(tool_calls=[c1..cn]) 之后紧接 n 条 role=tool**，
// 中间不得插入任何其他角色的消息（system/user 都不行）。
//
// 数量对而排列错，上游照样判 400 —— 这正是上面那条生产错误的形态。
//
// ## 缺陷位置（本判据要咬住的东西）
//
// stepToolAfter 每落一条 tool 消息后，会追加两类附加消息：
//
//	f.Msgs = append(f.Msgs, toolMsg)                 // tool(i)
//	if mediaMsg != nil { append(*mediaMsg) }         // ← user 消息（多模态注入）
//	if recallText != "" { appendOrReplaceRecall() }  // ← system 消息（记忆召回）
//
// 而 stepToolAfter 在**批内循环**里对每个工具各调一次。于是 N 个工具的批会变成：
//
//	assistant(tool_calls=[c0,c1])
//	tool(c0)
//	system(召回)        ← 插在中间，c1 的 tool 消息与 assistant 不再相邻
//	tool(c1)
//
// 用户明确的设计约束：**多模态注入点应在「当前轮工具调用」之后**
// —— 即整批的 tool 消息落完之后，而不是每条 tool 之后。

import (
	"strconv"
	"sync"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// visionBatchProvider 在 batchProvider 之上声明「能看图」。
//
// 为何需要它：prepareToolBlocks 只在 provider 支持该模态时才产出 native blocks
// （否则走文字回退，不产生 mediaMsg）—— 要复现多模态注入这条路径，必须让
// provider 支持 vision。
type visionBatchProvider struct{ *batchProvider }

func (p *visionBatchProvider) SupportsVision() bool { return true }
func (p *visionBatchProvider) SupportsAudio() bool  { return true }

// assertToolCallsAdjacent 断言：每条带 tool_calls 的 assistant 消息之后，
// 必须**紧接** len(tool_calls) 条 role=tool 消息，且 id 与声明一一对应。
//
// 返回违规描述（空切片 = 通过）。
func assertToolCallsAdjacent(msgs []agentAPI.Message) []string {
	var problems []string
	for i := 0; i < len(msgs); i++ {
		m := msgs[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		want := len(m.ToolCalls)
		// 逐条检查紧随其后的 want 条消息。
		for k := 1; k <= want; k++ {
			j := i + k
			if j >= len(msgs) {
				problems = append(problems, problemAt(i, k, "消息序列在配对完成前结束"))
				break
			}
			if msgs[j].Role != "tool" {
				problems = append(problems, problemAt(i, k,
					"第 "+strconv.Itoa(k)+" 条紧随消息是 role="+msgs[j].Role+"（期望 tool）"))
				break
			}
			if msgs[j].ToolCallID != m.ToolCalls[k-1].ID {
				problems = append(problems, problemAt(i, k,
					"tool_call_id 不匹配：声明 "+m.ToolCalls[k-1].ID+" 实得 "+msgs[j].ToolCallID))
			}
		}
	}
	return problems
}

func problemAt(msgIdx, k int, detail string) string {
	return "assistant@[" + strconv.Itoa(msgIdx) + "] 的配对断裂（第 " + strconv.Itoa(k) + " 条）：" + detail
}

// assertMediaNotSilentlyDropped 断言：注入的多模态块必须真的出现在消息序列里。
//
// ★ 为何与相邻性分开断言（2026-10-13 踩到）：只测相邻性时，把
//
//	flushBatchAttachments 整个删掉反而"更相邻"（附加消息压根没进来）——
//	判据全绿，而媒体被**静默丢弃**。两个性质必须同时断言：
//	  ① 媒体不得丢（丢了是功能失效）
//	  ② 媒体必须在整批 tool 消息**之后**（位置错了是协议违规）
func assertMediaNotSilentlyDropped(t *testing.T, msgs []agentAPI.Message) {
	t.Helper()
	for _, m := range msgs {
		if m.Role == "user" && len(m.Blocks) > 0 {
			return
		}
	}
	t.Errorf("工具注入了多模态块，但消息序列里没有任何带 Blocks 的 user 消息"+
		"（媒体被静默丢弃）\n%s", renderMsgRoles(msgs))
}

// TestBatchMediaInjectionKeepsToolPairingAdjacent 钉死用户明确的设计约束：
//
//	「多模态注入点应该在当前轮工具调用后」
//
// 即：整批的 tool 消息先落完，才允许挂多模态 user 消息；
// 而不是每落一条 tool 就挂一次 —— 那样第二条 tool 消息会被挤开，
// assistant 的 tool_calls 与它的结果不再相邻，上游判
// 「insufficient tool messages following tool_calls message」（生产实测）。
func TestBatchMediaInjectionKeepsToolPairingAdjacent(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &visionBatchProvider{&batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}}

	// 自己装配 agent：**不能**用 newBatchAgent —— 它已经把 tool_alpha/tool_beta
	// 注册在 batchdev 上，再注册第二个设备会被工具解析忽略（本判据第一版
	// 就因此"前置条件不成立"）。这里注册唯一的设备，并在其 toolFn 里注入块。
	a := New(AgentConfig{
		ID:              "mediaagent",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	var mu sync.Mutex
	dev := &mockOutputDevice{
		name: "mediadev",
		caps: agentIO.CapText,
		tools: []agentIO.ToolDef{
			{Name: "tool_alpha", Description: "a"},
			{Name: "tool_beta", Description: "b"},
		},
		toolFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			// 每个工具都注入一个多模态块（模拟截图/看图类工具）。
			a.io.SetToolBlocks([]interface{}{
				pubsdk.ContentBlock{
					Type:     "image_url",
					ImageURL: &pubsdk.ImageURL{URL: "data:image/png;base64,AAAA"},
				},
			})
			return "ran:" + tool, nil
		},
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册测试设备失败: %v", err)
	}

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}

	assertMediaNotSilentlyDropped(t, f.Msgs)

	if probs := assertToolCallsAdjacent(f.Msgs); len(probs) > 0 {
		for _, p := range probs {
			t.Errorf("配对相邻性违反：%s", p)
		}
		t.Errorf("完整消息序列：\n%s", renderMsgRoles(f.Msgs))
	}
}

// TestBatchParallelPathAlsoKeepsAdjacency 覆盖**并行**路径的 flush 点。
//
// 为何必须单独一条：并行与串行是 stepToolBatch / stepToolBegin 两个不同的
// 收尾点，各自调用 flushBatchAttachments。只测一条路径会让另一条的 flush
// 被删掉也不报红（本判据第一版即如此：测试工具未声明 ParallelSafe，
// 整批降级走串行，M3 变异删掉并行 flush 后仍全绿）。
func TestBatchParallelPathAlsoKeepsAdjacency(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "p1", Name: "par_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "p2", Name: "par_beta", Arguments: map[string]interface{}{}}
	sp := &visionBatchProvider{&batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}}
	a := New(AgentConfig{
		ID:              "paragent",
		Provider:        sp,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
	})
	var mu sync.Mutex
	dev := &mockOutputDevice{
		name: "pardev",
		caps: agentIO.CapText,
		tools: []agentIO.ToolDef{
			// ★ ParallelSafe:true 是全批走并行路径的必要条件（三条件之一）。
			{Name: "par_alpha", Description: "a", ParallelSafe: true},
			{Name: "par_beta", Description: "b", ParallelSafe: true},
		},
		toolFn: func(tool string, args map[string]interface{}) (interface{}, error) {
			mu.Lock()
			defer mu.Unlock()
			a.io.SetToolBlocks([]interface{}{
				pubsdk.ContentBlock{
					Type:     "image_url",
					ImageURL: &pubsdk.ImageURL{URL: "data:image/png;base64,AAAA"},
				},
			})
			return "ran:" + tool, nil
		},
	}
	if err := a.io.RegisterDevice(dev); err != nil {
		t.Fatalf("注册测试设备失败: %v", err)
	}

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}
	// 前置断言：批内两枚工具都声明了 ParallelSafe，且 >1 个 ⇒ 必须走并行路径。
	// 若这条不成立，说明并行判定变了，本判据就不再覆盖并行 flush —— 那是
	// 「测了等于没测」，必须响亮失败而不是静默通过。
	if !f.batchRunnable(a) {
		t.Fatalf("本批未走并行路径（两枚工具均已声明 ParallelSafe），"+
			"本判据失去覆盖目标\n%s", renderMsgRoles(f.Msgs))
	}
	assertMediaNotSilentlyDropped(t, f.Msgs)
	if probs := assertToolCallsAdjacent(f.Msgs); len(probs) > 0 {
		for _, p := range probs {
			t.Errorf("并行路径配对相邻性违反：%s", p)
		}
		t.Errorf("完整消息序列：\n%s", renderMsgRoles(f.Msgs))
	}
}

// TestBatchToolMessagesAdjacentToAssistant 是最小回归：两个工具的批，
// assistant 之后必须紧跟两条 tool 消息。
func TestBatchToolMessagesAdjacentToAssistant(t *testing.T) {
	tcA := agentAPI.ToolCall{ID: "c1", Name: "tool_alpha", Arguments: map[string]interface{}{}}
	tcB := agentAPI.ToolCall{ID: "c2", Name: "tool_beta", Arguments: map[string]interface{}{}}
	sp := &batchProvider{responses: []*agentAPI.CompletionResponse{
		{ToolCalls: []agentAPI.ToolCall{tcA, tcB}},
		{Content: "final"},
	}}
	a, _ := newBatchAgent(t, sp)

	f := a.newTaskFrame("go", a.stageCtxFromInput("go", "", ""))
	if out := a.runTaskSteps(f); out != outcomeDone {
		t.Fatalf("runTaskSteps = %v, err=%v", out, f.Err)
	}

	if probs := assertToolCallsAdjacent(f.Msgs); len(probs) > 0 {
		for _, p := range probs {
			t.Errorf("配对相邻性违反：%s", p)
		}
		t.Errorf("完整消息序列：\n%s", renderMsgRoles(f.Msgs))
	}
}

// renderMsgRoles 把消息序列渲染成一行，便于失败时肉眼定位。
func renderMsgRoles(msgs []agentAPI.Message) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "\n"
		}
		out += "  [" + strconv.Itoa(i) + "] " + m.Role
		if len(m.ToolCalls) > 0 {
			out += " tool_calls=["
			for k, tc := range m.ToolCalls {
				if k > 0 {
					out += ","
				}
				out += tc.ID
			}
			out += "]"
		}
		if m.ToolCallID != "" {
			out += " tool_call_id=" + m.ToolCallID
		}
		if m.Role == "system" {
			out += " (system)"
		}
	}
	return out
}
