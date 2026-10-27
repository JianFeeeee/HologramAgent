package core

// 插件的 `NoMemory` 声明必须落到**事件**上（2026-10-27）。
//
// 声明链：InjectOptions.NoMemory / ChannelDef.NoMemory
//        → payload["no_memory"] → stageCtx.NoMemory → ContextEvent.NoMemory
//
// 此前最后一步从来没发生：`ContextEvent.NoMemory` 只有在 coldcontext_query
// 回填时才被置 true，于是 QQ 那句取件样板
//（"来自「X」的私聊消息(message_id=…) 先用 qq_get_message 取正文…"）
// 每轮都进计算层（向量化 / 关键词 / 蒸馏）。
//
// 判据（按字段既有契约，不是新增语义）：
//   ① NoMemory 事件的 textForVector 为空（不进计算层）；
//   ② 但事件**照常归档**（Input/Response 原样保留，能从 L1 落回 L2）；
//   ③ 非 NoMemory 事件不受影响。

import (
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newNoMemFlagContext() *RelevanceContext {
	return NewRelevanceContext("", memory.NewStaticEmbedder(""))
}

func TestNoMemoryEventSkipsComputationButKeepsArchiveFields(t *testing.T) {
	rc := newNoMemFlagContext()
	ev := ContextEvent{
		Timestamp: time.Now(),
		Source:    "agent",
		NoMemory:  true,
		Input:     "来自「—/」的私聊消息(message_id=1)。先用qq_get_message取正文",
		Response:  "收到。",
	}
	rc.Append(ev)

	got := rc.snapshotEvents()
	if len(got) != 1 {
		t.Fatalf("应写入一条事件，实际 %d", len(got))
	}
	// ① 计算层：不进向量
	if txt := textForVector(got[0], nil, nil); txt != "" {
		t.Fatalf("NoMemory 事件不得进入计算层，实际 textForVector=%q", txt)
	}
	// ② 归档字段保留（否则 L2 回放闭环断掉）
	if got[0].Input == "" || got[0].Response == "" {
		t.Fatalf("归档字段必须保留，实际 Input=%q Response=%q", got[0].Input, got[0].Response)
	}
}

func TestNormalEventStillEntersComputation(t *testing.T) {
	rc := newNoMemFlagContext()
	rc.Append(ContextEvent{Timestamp: time.Now(), Source: "webui", Input: "帮我看下日志"})
	got := rc.snapshotEvents()
	if txt := textForVector(got[0], nil, nil); txt == "" {
		t.Fatal("普通事件必须进入计算层")
	}
}
