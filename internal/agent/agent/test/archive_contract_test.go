package test

// 冷文档归档的**判定语义**契约（2026-11-15 重写后）。
//
// ## 钉的是什么
//
// 生产实测的失效形态（`homed_2026-11-15_13-08-13.log`）：
//
//	702 个文档 → 0 个块进 L3（持 11994 块，图库命中 0）
//	50 个文档每小时重试一次，连跑 3 天共 2950 次
//
// 四层原因叠加：
//  1. 704/721 文档 source=context_archived，`文档--主题-->` 与 `文档--来源-->`
//     两个模板三元组都被跳过 ⇒ 只剩 NLP 一条路；
//  2. NLP 在「终端日志 + ADB 截图回执」上切出「、」「（」「，」当主语；
//  3. 判定用 MemoryBlockCount 差值 —— 块 ID 内容派生，幂等命中时**不涨**，
//     于是「写成功」被判成「没写」（259 条里 77 条属此类）；
//  4. 判定排在块迁移之前 ⇒ 三元组全被拒 = 文档自带的块也一起扣在 L2。
//
// 本文件钉住重写后的语义边界，全部走**导出接口**：
// Agent.Start → 归档循环自动跑 → 观察 ColdDoc / GraphDB 状态。
//
// ⚠️ 这些判据不依赖真实 LLM：归档路径只碰 nlp.Extractor（无 ONNX 时走 fallback）
// 与图库，不需要 provider 回包。

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// archiveHarness 构造一个「只有归档相关装配」的最小 agent。
//
// 为什么这样搭：归档只看 graph memory + coldcontext store + embedder，
// 不需要 provider/插件/IO 这些。少装配一样，判据就少一样可能坏的东西。
func archiveHarness(t *testing.T, factory func(cfg agent.AgentConfig) (*agent.Agent, error)) (*agent.Agent, *coldcontext.Store, *memory.GraphDB) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	store := coldcontext.NewStore(filepath.Join(dir, "documents"), func(s string) []string {
		return strings.Fields(s)
	})
	if err := store.Start(); err != nil {
		t.Fatalf("store.Start: %v", err)
	}
	t.Cleanup(store.Stop)
	store.SetDenseSpace(nil) // 无稠密空间 ⇒ 走 TF-IDF 回退，判据不依赖 ONNX

	iom := io.NewIOManager()

	a, instErr := factory(agent.AgentConfig{
		Security: security.NewDisabledRuntime().AsGate(),
		ID:       types.AgentID("archive-contract"),
		Memory:   g,
		DocStore: store,
		IO:       iom,
		DataDir:  dir,
		// 归档间隔给得足够短，让判据在秒级观察到一轮；
		// DistillInterval 同理（ArchiveInterval<=0 时它会继承 DistillInterval）。
		DistillInterval: 50 * time.Millisecond,
	})
	if instErr != nil {
		t.Fatalf("Instantiate(main): %v", instErr)
	}
	return a, store, g
}

// TestArchiveMovesBlocksWhenNoTriplesProduced 是本轮修复的核心判据。
//
// 语义：**三元组抽不出来时，文档自带的块仍必须迁进 L3，文档被删除。**
//
// 变异验证（实测）：把「块迁移」重新放回「三元组写入之后」并加上
// `blocksAfter == blocksBefore ⇒ continue` 前置门 ⇒ 块永远不进库、文档永不删
// ⇒ 本判据红（这正是在生产里发生了三天的形态）。
func TestArchiveMovesBlocksWhenNoTriplesProduced(t *testing.T) {
	a, store, g := archiveHarness(t, func(cfg agent.AgentConfig) (*agent.Agent, error) {
		return agent.Instantiate(agent.TypeNameMain, cfg)
	})

	// 一段「终端日志 / 工具回执」类正文：NLP 抽不出合法三元组。
	// 用它构造文档，正是生产里那 702 篇的形态。
	doc, err := store.ContextToColdContext("context_archived", []coldcontext.ContextEntry{{
		Source:  "agent",
		Content: "adb devices\n2XT0219315005305 device\n" + strings.Repeat("screenshot {scale:60}\n", 20),
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}
	// 进入冷候选窗口（FindColdContexts 要求 ≤2 次访问且 72h 未读）。
	doc.LastAccess = time.Now().Add(-96 * time.Hour)
	doc.AccessCount = 0

	before, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}

	// ★ 归档是**类型声明的定时任务**，由**内核事件循环**刻度
	// （2026-11-17 裁定：agentloop 归 agent，定时器归 eventloop）。
	// ⇒ 判据必须显式推动那个任务体：它不再由 Start 隐含启动。
	a.RegisterMaintenanceTasks()
	a.Start()
	defer a.Stop()

	// 推动一次归档任务体（等价于事件循环的一次 tick）。
	for _, t := range a.PeriodicForLoop() {
		if t.Name == "archive" && t.Run != nil {
			t.Run(a)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if store.Get(doc.ID) == nil {
			break // 被删除 = 归档完成
		}
		time.Sleep(20 * time.Millisecond)
	}
	if store.Get(doc.ID) != nil {
		t.Fatalf("★ 文档仍留在 L2（块也没进 L3）—— 这正是死循环的形态")
	}

	after, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("★ 块数没有增长（before=%d after=%d）：文档被删了但内容没进 L3 = 丢数据",
			before, after)
	}
}

// TestArchiveKeepsDocumentWhenItHoldsNothing 是「防误删」的另一半：
// 既没三元组也没有块的空文档，删掉是安全的（没有内容可丢），
// 但它必须走**空文档**路径而不是被当成失败重试。
func TestArchiveKeepsDocumentWhenItHoldsNothing(t *testing.T) {
	a, store, _ := archiveHarness(t, func(cfg agent.AgentConfig) (*agent.Agent, error) {
		return agent.Instantiate(agent.TypeNameMain, cfg)
	})

	doc, err := store.ContextToColdContext("context_archived", []coldcontext.ContextEntry{{
		Source: "agent",
		// 空正文 ⇒ 不产生任何文本块。
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		// ContextToColdContext 对「全空事件」返回 nil —— 那本身就是「无内容可归档」。
		t.Skip("全空事件不产生文档，跳过（语义由 nil 表达）")
	}
	doc.LastAccess = time.Now().Add(-96 * time.Hour)
	doc.AccessCount = 0

	// ★ 归档是**类型注册的定时任务**（2026-11-17 用户裁定：基座暴露定时器
	// 回调接口，而不是在 Start 里硬编码五个循环）。
	// ⇒ 判据必须显式注册主 agent 的维护任务全集；Start 只拉起已注册的。
	//   （旧判据依赖"Start 隐含起归档循环"，那是被本次改造移除的内部结构。）
	a.RegisterMaintenanceTasks()
	a.Start()
	defer a.Stop()

	for _, t := range a.PeriodicForLoop() {
		if t.Name == "archive" && t.Run != nil {
			t.Run(a)
		}
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if store.Get(doc.ID) == nil {
			return // 已删除，符合预期
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("★ 空文档既没被删除也没被标记，归档停摆")
}
