package core

import (
	"strings"
	"testing"
)

// 阶段 2.5：提示词改为「默认并行」。
//
// ⚠️ 本阶段有一条**硬性顺序约束**：必须在并行执行（阶段 2）落地**之后**。
// 反序（先改提示词说"并发"、内核仍串行）会让提示词**对模型说谎** ——
// 模型据"并发执行"推断安全性，写出真正依赖顺序的调用。宁可晚改，不可错改。
//
// 判据的负向部分（串行阶段不得出现"默认并行"字样）已在阶段 2 完成后
// 才补写，故此处只断言**正向**内容：四要点齐全，且与 batchRunnable 的
// 真实判据**一致**——提示词若与实现不符，比不说更坏。

// ① 四要点齐全：默认并行 / 不可依赖顺序 / 同通道保序 / 写类工具不并发。
func TestPromptDeclaresParallelContract(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	p := a.buildSystemPrompt("", "hi")

	required := []struct {
		key   string
		words []string
	}{
		{"默认并行", []string{"并行"}},
		{"不可依赖顺序", []string{"顺序"}},
		{"同通道保序", []string{"保序"}},
		{"并发安全声明", []string{"并发安全", "ParallelSafe", "声明"}},
	}
	for _, r := range required {
		found := false
		for _, w := range r.words {
			if strings.Contains(p, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("提示词缺少要点「%s」（期望含 %v 之一）", r.key, r.words)
		}
	}
}

// ② ★ 提示词必须与实现**一致**，不能只说一半。
//
// batchRunnable 的真实规则是「全批都 ParallelSafe 才并发，且同通道
// 多次发送不并发」。若提示词只说"会并行"而不说例外，模型会在
// 「同通道连发」时误以为顺序无关 —— 而实现恰恰保证了保序（安全但
// 模型不知情）；反过来若说"永远串行"则与实现矛盾。
//
// 本判据钉死：提示词里必须同时出现"例外/不并发"的限定语。
func TestPromptStatesExceptionsNotJustParallelism(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	p := a.buildSystemPrompt("", "hi")
	for _, w := range []string{"例外", "不会并发", "不并发"} {
		if strings.Contains(p, w) {
			return
		}
	}
	t.Error("提示词只讲并行、不讲例外 —— 与 batchRunnable 的实际规则不符，模型会误判")
}

// ③ output_send__ 同通道保序这一条必须**显式**告诉模型。
//
// 理由：保序是内核替模型兜住的行为，模型不知道就可能依赖"反正并发"
// 来发多条消息，从而写出让保序失去意义的东西（如把"重试"和"确认"
// 并发发出）。显式说明能让模型据此主动选择分轮。
func TestPromptExplainsSameChannelOrdering(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	p := a.buildSystemPrompt("", "hi")
	if !strings.Contains(p, "output_send__") {
		t.Fatal("提示词未提及 output_send__，无法说明同通道保序")
	}
	if !strings.Contains(p, "保序") {
		t.Error("提示词未说明同通道多次发送会保序")
	}
}

// ④ spawn_child 的旧表述必须改掉。
//
// 原文是「应并行 spawn 多个子 Agent，不要自己串行逐个执行」——它在并行化
// 之前是**落空**的（模型照做，内核仍串行）。改造后应改为机制性表述。
func TestPromptSpawnChildNoLongerOverpromises(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	desc := toolDefDescription(a, "spawn_child")
	if desc == "" {
		t.Fatal("buildToolDefs 里没有 spawn_child")
	}
	// 若保留「并行 spawn」的建议，必须同时说明它现在真的并发
	// （否则又是一句落空的建议）。这里只要求不出现旧的绝对化措辞。
	if strings.Contains(desc, "不要自己串行逐个执行") {
		t.Error("spawn_child 描述仍含旧的「不要自己串行逐个执行」——该建议在并行化前是落空的")
	}
}

// ⑤ 提示词改动不得破坏既有要点（回归防护）。
func TestPromptKeepsExistingContract(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	p := a.buildSystemPrompt("", "hi")
	for _, want := range []string{
		"【输出规则】",
		"output_list_channels",
		"【可用工具能力】",
		// 记忆维护块：标题从【记忆清理指令】改为【记忆维护】——具体工具选择
		// 下沉到 memory_* 的工具描述，提示词只保留「必须真做、不许只描述计划」。
		"【记忆维护】",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词丢失既有要点 %q", want)
		}
	}
}

// toolDefDescription 从 buildToolDefs 的产物里取某工具的 description。
// 直接查真实产物，而不是另建一套注册表——判据必须对着**代码真实输出**。
func toolDefDescription(a *Agent, name string) string {
	for _, t := range a.buildToolDefs() {
		fn, ok := t.(map[string]interface{})["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := fn["name"].(string); n == name {
			d, _ := fn["description"].(string)
			return d
		}
	}
	return ""
}
