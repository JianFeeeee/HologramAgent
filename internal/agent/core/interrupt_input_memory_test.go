package core

// 中断**消息本身**不得进入记忆（用户口径，2026-10-27，颗粒度=单条消息）。
//
// 中断的 CleanInput 是触发文本（"来自「X」的私聊消息(message_id=…)先用
// qq_get_message 取正文…"），它是指令不是用户的话。存进 L1 的后果实测：
// 时间线里每轮都是同一句样板，真正的对话内容只剩工具回执碎片 ——
// 用户回一句 "233719"，模型却问"那串数字什么意思"（指代消解失效）。
//
// 但**本轮的工具回执与回复必须照常入库**：qq_get_message 的正文各自声明
// NoMemory=false，那才是要记的内容。判据就是这两条一起成立。

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// newInterruptTestContext 建一个不落盘的 L1（判据只看内存态）。
func newInterruptTestContext() *RelevanceContext {
	return NewRelevanceContext("", memory.NewStaticEmbedder(""))
}

func TestInterruptTriggerTextNotStoredButToolResultsAre(t *testing.T) {
	rc := newInterruptTestContext()
	turn := ContextEvent{
		Timestamp:   time.Now(),
		Source:      "agent",
		Input:       "", // finishInputTask 对中断轮置空
		Response:    "已经回你了。",
		ToolResults: []ToolResultItem{{Name: "qq_get_message", Output: `{"content":"233719"}`}},
	}
	rc.Append(turn)

	got := rc.snapshotEvents()
	if len(got) != 1 {
		t.Fatalf("应写入一条事件，实际 %d", len(got))
	}
	if got[0].Input != "" {
		t.Fatalf("中断触发文本不得入库，实际 Input=%q", got[0].Input)
	}
	if len(got[0].ToolResults) != 1 {
		t.Fatalf("工具回执必须保留（内容侧），实际 %+v", got[0].ToolResults)
	}
	if got[0].Response == "" {
		t.Fatal("agent 回复应保留")
	}
}

// 只清"取件指令样板"，内核中断的正文必须保留（测试 D1=B：中断上下文要
// 加载回被抢占任务之上）。
func TestInjectionBoilerplateOnly(t *testing.T) {
	boiler := "来自「—/」的私聊消息(message_id=814968683, user_id=2198972886)。先用qq_get_message(message_id=814968683)取正文；若取不到（消息已过期），改用qq_get_history(user_id=2198972886)按会话拉取上下文。用output_send__qq回复对方"
	if !isInjectionBoilerplate(boiler) {
		t.Fatal("插件注入的取件样板应被识别")
	}
	if isInjectionBoilerplate("紧急打断：立刻停下手上的活") {
		t.Fatal("内核中断的正文不是样板，不得清空")
	}
	if isInjectionBoilerplate("帮我看下日志") {
		t.Fatal("普通输入不得被判成样板")
	}
}

// 非中断轮必须原样保留用户输入（不能被同一条规则误伤）。
func TestNormalTurnKeepsInput(t *testing.T) {
	rc := newInterruptTestContext()
	rc.Append(ContextEvent{Timestamp: time.Now(), Source: "webui", Input: "帮我看下日志"})
	got := rc.snapshotEvents()
	if len(got) != 1 || got[0].Input != "帮我看下日志" {
		t.Fatalf("普通轮输入不能被清空，实际 %+v", got)
	}
}
