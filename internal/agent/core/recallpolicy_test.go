package core

import (
	"path/filepath"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 这一组测试锁死「默认召回、可显式关闭」这条语义。
//
// 与 prune 刻意相反：裁剪是破坏性的、默认关；召回是只读增量、默认开。
// 两者正交，一根 ContextPolicy 表达不了 2×2 的组合（只召回不裁剪 / 只裁不召回）。
func TestRecallDeclared_DefaultsToRecall(t *testing.T) {
	m := agentIO.NewIOManager()
	a := &Agent{io: m}

	// 没有任何声明 → 默认召回（保持既有"每条输入都召回"的行为）。
	if !a.recallDeclared(&agentIO.InputEvent{Source: "unknown", Payload: map[string]interface{}{}}) {
		t.Fatal("未声明的输入默认必须召回")
	}
	// 通道注册了但没设 RecallPolicy → 仍默认召回。
	m.RegisterInputChannel("plain", pubsdk.ChannelDef{})
	if !a.recallDeclared(&agentIO.InputEvent{Source: "plain", Payload: map[string]interface{}{}}) {
		t.Fatal("ChannelDef 未设 RecallPolicy 应默认召回")
	}
	// nil 事件不能 panic，且按默认召回。
	if !a.recallDeclared(nil) {
		t.Fatal("nil 事件应默认召回")
	}
}

func TestRecallDeclared_ChannelOptOut(t *testing.T) {
	m := agentIO.NewIOManager()
	m.RegisterInputChannel("meta", pubsdk.ChannelDef{RecallPolicy: pubsdk.RecallPolicyNone})
	m.RegisterInputChannel("talk", pubsdk.ChannelDef{RecallPolicy: pubsdk.RecallPolicyAuto})
	a := &Agent{io: m}

	if a.recallDeclared(&agentIO.InputEvent{Source: "meta", Payload: map[string]interface{}{}}) {
		t.Fatal("通道声明 none 不应召回")
	}
	if !a.recallDeclared(&agentIO.InputEvent{Source: "talk", Payload: map[string]interface{}{}}) {
		t.Fatal("通道声明 auto 应召回")
	}
}

// 注入点声明优先于通道定义：同一通道下的不同注入可以有不同意图。
func TestRecallDeclared_InjectionOverridesChannel(t *testing.T) {
	m := agentIO.NewIOManager()
	a := &Agent{io: m}
	m.RegisterInputChannel("qq", pubsdk.ChannelDef{RecallPolicy: pubsdk.RecallPolicyNone})

	evt := &agentIO.InputEvent{Source: "qq", Payload: map[string]interface{}{
		"recall_policy": pubsdk.RecallPolicyAuto,
	}}
	if !a.recallDeclared(evt) {
		t.Fatal("注入点声明 auto 应覆盖通道的 none")
	}

	m.RegisterInputChannel("plain", pubsdk.ChannelDef{RecallPolicy: pubsdk.RecallPolicyAuto})
	evt = &agentIO.InputEvent{Source: "plain", Payload: map[string]interface{}{
		"recall_policy": pubsdk.RecallPolicyNone,
	}}
	if a.recallDeclared(evt) {
		t.Fatal("注入点声明 none 应覆盖通道的 auto")
	}
}

// buildTaskMemoryContext 在声明 none 时必须返回空串（不注入记忆索引）。
func TestBuildTaskMemoryContext_RespectsPolicy(t *testing.T) {
	m := agentIO.NewIOManager()
	m.RegisterInputChannel("meta", pubsdk.ChannelDef{RecallPolicy: pubsdk.RecallPolicyNone})
	a := &Agent{io: m, indexer: newTestIndexer(t, "咖啡", "张三")}

	f := &TaskFrame{Evt: &agentIO.InputEvent{Source: "meta", Payload: map[string]interface{}{}}}
	if got := a.buildTaskMemoryContext(f, "咖啡", 0); got != "" {
		t.Fatalf("声明 none 时不应注入记忆，实际 %q", got)
	}

	f2 := &TaskFrame{Evt: &agentIO.InputEvent{Source: "plain", Payload: map[string]interface{}{}}}
	if got := a.buildTaskMemoryContext(f2, "咖啡", 0); !strings.Contains(got, "【记忆索引】") {
		t.Fatalf("默认应注入记忆索引，实际 %q", got)
	}
}

// 工具触发的召回：以（清洗后的）工具输出为 query，产出可注入的记忆文本。
func TestRecallTextFor_UsesQuery(t *testing.T) {
	a := &Agent{indexer: newTestIndexer(t, "咖啡", "张三")}
	got := a.recallTextFor("咖啡", "tool:test", nil)
	if !strings.Contains(got, "【记忆索引】") {
		t.Fatalf("应产出记忆索引文本，实际 %q", got)
	}
	// 空 query 或无 indexer 时不产出、不 panic。
	if got := a.recallTextFor("", "tool:test", nil); got != "" {
		t.Fatalf("空 query 应返回空串，实际 %q", got)
	}
	if got := (&Agent{}).recallTextFor("咖啡", "tool:test", nil); got != "" {
		t.Fatalf("无 indexer 应返回空串，实际 %q", got)
	}
}

// 召回文本以 system 消息挂在末尾；同一任务内多次触发是**替换**而非累加。
func TestAppendOrReplaceRecall(t *testing.T) {
	msgs := []agentAPI.Message{{Role: "user", Content: "hi"}}
	msgs = appendOrReplaceRecall(msgs, "第一段")
	if len(msgs) != 2 || msgs[1].Role != "system" || !strings.Contains(msgs[1].Content, "第一段") {
		t.Fatalf("首次应追加一条 system 召回消息，实际 %+v", msgs)
	}
	msgs = appendOrReplaceRecall(msgs, "第二段")
	if len(msgs) != 2 {
		t.Fatalf("再次触发应替换而非累加，实际 %d 条", len(msgs))
	}
	if !strings.Contains(msgs[1].Content, "第二段") || strings.Contains(msgs[1].Content, "第一段") {
		t.Fatalf("替换后应只含最新召回，实际 %q", msgs[1].Content)
	}
	if msgs = appendOrReplaceRecall(msgs, ""); len(msgs) != 2 {
		t.Fatalf("空召回不应改变消息，实际 %d 条", len(msgs))
	}
}

// newTestIndexer 造一个只含给定实体的图记忆 + 已同步的索引器。
func newTestIndexer(t *testing.T, subject, object string) *memory.Indexer {
	t.Helper()
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, _, err := db.Commit([]memory.Triple{{Subject: subject, Relation: "喜欢", Object: object}}, "s", 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	idx := memory.NewIndexer(db)
	if err := idx.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return idx
}
