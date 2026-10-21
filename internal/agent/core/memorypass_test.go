package core

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

// memoryPass 是「裁剪」与「召回」的唯一入口：两根正交轴，但共用同一份 query。
//
// 这一组测试锁死三件事：
//  1. 两个策略都不声明时是 no-op（不裁剪、不召回）；
//  2. 同时声明时一次调用同时产出「归档数」与「召回文本」；
//  3. 单一策略只产出对应的那一个输出（正交，不互相触发）。
func TestMemoryPass_NoPolicyIsNoOp(t *testing.T) {
	a := &Agent{
		context:        newPruneableContext(15),
		maxContextSize: 4,
		indexer:        newTestIndexer(t, "咖啡", "张三"),
	}
	out := a.memoryPass("咖啡", "test", false, false, nil)
	if out.Archived != 0 || out.RecallText != "" {
		t.Fatalf("未声明任何策略时不应有任何输出，实际 %+v", out)
	}
}

func TestMemoryPass_PruneAndRecallTogether(t *testing.T) {
	a := newMemoryPassAgent(t)
	before := a.context.Len()
	out := a.memoryPass("咖啡", "tool:test", true, true, nil)
	if out.Archived == 0 {
		t.Fatal("声明 prune 应归档低相关事件")
	}
	if a.context.Len() >= before {
		t.Fatalf("裁剪后上下文应变短：%d → %d", before, a.context.Len())
	}
	if !strings.Contains(out.RecallText, "【记忆索引】") {
		t.Fatalf("声明 recall 应产出记忆索引文本，实际 %q", out.RecallText)
	}
}

func TestMemoryPass_PoliciesAreOrthogonal(t *testing.T) {
	// 只裁不召回：输出只有归档数。
	onlyPrune := &Agent{
		context:        newPruneableContext(15),
		maxContextSize: 4,
		indexer:        newTestIndexer(t, "咖啡", "张三"),
	}
	if out := onlyPrune.memoryPass("咖啡", "test", true, false, nil); out.RecallText != "" {
		t.Fatalf("只声明 prune 不应召回，实际 %q", out.RecallText)
	}
	// 只召回不裁剪：输出只有召回文本，上下文条数不变。
	onlyRecall := &Agent{
		context:        newPruneableContext(15),
		maxContextSize: 4,
		indexer:        newTestIndexer(t, "咖啡", "张三"),
	}
	before := onlyRecall.context.Len()
	out := onlyRecall.memoryPass("咖啡", "test", false, true, nil)
	if out.Archived != 0 {
		t.Fatalf("只声明 recall 不应裁剪，实际归档 %d", out.Archived)
	}
	if onlyRecall.context.Len() != before {
		t.Fatalf("只声明 recall 不应改变上下文条数：%d → %d", before, onlyRecall.context.Len())
	}
}

// 输入侧召回必须用**清洗后**的 query（通道 Cleaner 的输出），与裁剪侧一致。
// 用无关原文 + 命中清洗文本做区分，锁死「用的是 CleanInput 而不是 Input」。
func TestBuildTaskMemoryContext_UsesCleanInput(t *testing.T) {
	a := &Agent{indexer: newTestIndexer(t, "咖啡", "张三")}

	// CleanInput 命中实体、原文完全不相关 → 应召回（证明用了清洗文本）。
	f := &TaskFrame{Evt: nil, CleanInput: "咖啡"}
	if got := a.buildTaskMemoryContext(f, "zzz", 0); !strings.Contains(got, "【记忆索引】") {
		t.Fatalf("应据清洗后的 query 召回，实际 %q", got)
	}
	// 清洗为空 → 回退原文；原文无关则不召回。
	f2 := &TaskFrame{CleanInput: ""}
	if got := a.buildTaskMemoryContext(f2, "zzz", 0); got != "" {
		t.Fatalf("清洗为空且原文无关时不应召回，实际 %q", got)
	}
}

// newMemoryPassAgent 造一个同时能做裁剪与召回的 agent（含 doc 记忆落点）。
func newMemoryPassAgent(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		context:        newPruneableContext(15),
		maxContextSize: 4,
		indexer:        newTestIndexer(t, "咖啡", "张三"),
		docStore:       document.NewStore(filepath.Join(t.TempDir(), "docs"), memory.TokenizeWords),
	}
}

// newPruneableContext 造 n 条可被裁剪的上下文（最近 10 条受保护）。
func newPruneableContext(n int) *RelevanceContext {
	ctx := NewRelevanceContext("", memory.NewStaticEmbedder(""))
	for i := 0; i < n; i++ {
		ctx.Append(ContextEvent{Timestamp: time.Now(), Source: "user", Input: "事件内容"})
	}
	return ctx
}
