package test

// 归档重试计数的模块职责契约：失败可计数、计数跨重启不丢、到上限可观测。
//
// ## 钉的是什么（2026-11-15）
//
// 归档失败此前只是「保留待下一轮」，没有计数、没有退避、没有死信。
// 生产实测：50 个文档每小时重试一次，从 2026-11-11 起连跑 3 天共 2950 次，
// 输入完全相同 ⇒ 必然失败的无限循环。计数只存内存的话，一次重启就归零，
// 于是「无限重试」在重启后重新开始 —— 那正是要防的那件事。
//
// 判据只走导出接口（NoteArchiveFailure / ArchiveAttempts / Get / Store 重开），
// 不读盘上明文、不碰私有字段。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

func tokWords(text string) []string { return strings.Fields(text) }

// TestArchiveFailureCountSurvivesReopen 是本判据的核心：
// 计数必须真的落盘，而不是只活在内存里。
//
// 变异验证：把 NoteArchiveFailure 里写文件的四行删掉 ⇒ 重开后计数回 0 ⇒ 红。
func TestArchiveFailureCountSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	first := coldcontext.NewStore(dir, tokWords)
	doc := &coldcontext.ColdDoc{
		Summary: "二维码会话归档",
		Content: "终端输出与截图",
		Source:  "context_archived",
	}
	if err := first.Insert(doc); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if got := first.NoteArchiveFailure(doc.ID, "block migration incomplete"); got != i {
			t.Fatalf("第 %d 次失败后计数 = %d，期望 %d", i, got, i)
		}
	}
	if got := first.ArchiveAttempts(doc.ID); got != 3 {
		t.Fatalf("ArchiveAttempts = %d，期望 3", got)
	}

	// 重开同一目录：计数必须还在（否则重启即清零 ⇒ 无限重试重新开始）。
	second := coldcontext.NewStore(dir, tokWords)
	if err := second.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer second.Stop()
	if got := second.ArchiveAttempts(doc.ID); got != 3 {
		t.Fatalf("★ 重开后计数 = %d，期望 3（计数没落盘就会归零）", got)
	}
	reloaded := second.Get(doc.ID)
	if reloaded == nil {
		t.Fatal("★ 重开后文档不见了")
	}
	if reloaded.ArchiveLastError != "block migration incomplete" {
		t.Fatalf("最后一次失败原因 = %q，期望原样保留", reloaded.ArchiveLastError)
	}
}

// TestArchiveFailureOnUnknownDocIsNoOp：对不存在的文档记失败不能凭空造条目。
func TestArchiveFailureOnUnknownDocIsNoOp(t *testing.T) {
	s := coldcontext.NewStore(t.TempDir(), tokWords)
	if got := s.NoteArchiveFailure("doc_not_there", "x"); got != 0 {
		t.Fatalf("未知文档的计数 = %d，期望 0", got)
	}
	if s.Get("doc_not_there") != nil {
		t.Fatal("★ 对未知文档记失败竟创建了文档")
	}
}

// TestDirtyTracksDocumentChanges：Dirty 必须在有变化时为真、无变化时为假。
//
// 用途：归档循环此前**每轮无条件** Reindex（重置 sync.Once、重建全部训练文本、
// 全量 Train 并持写锁）—— 每小时一次 O(全库) 停顿，与「这轮有没有新文档」无关。
// Diry 让调用方可以只在真变化时重建。
//
// 变异验证：让 Dirty 恒返回 true ⇒ 本判据的「无变化时必须为假」段落红。
func TestDirtyTracksDocumentChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs")
	s := coldcontext.NewStore(dir, tokWords)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// 新库、无文档：没有任何变化可重建。
	if s.Dirty() {
		t.Fatal("★ 空库报告 dirty")
	}

	if err := s.Insert(&coldcontext.ColdDoc{Summary: "账单", Content: "发票重试", Source: "agent"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !s.Dirty() {
		t.Fatal("★ Insert 后没有报告 dirty（归档就永远不会重建索引）")
	}

	// Stop 会 flush；flush 之后 dirty 必须清掉。
	s.Stop()
	reopened := coldcontext.NewStore(dir, tokWords)
	if err := reopened.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer reopened.Stop()
	if reopened.Dirty() {
		t.Fatal("★ flush 之后仍报告 dirty（每轮都会白重建一次索引）")
	}
}

// TestNoMemorySurvivesL2RoundTrip：NoMemory 声明必须一路带到 L2。
//
// ## 为什么（2026-11-15）
//
// 声明说的是「不参与记忆计算（向量化/关键词/**蒸馏**）」。L1 的
// textForVector 照做了，但 L3 蒸馏会拿 L2 的原文重跑一遍三元组抽取。
// 声明在 L1→L2 边界断掉 ⇒ 同一段内容第一次被挡、第二次被放行 ⇒ 声明失效。
//
// 变异验证：删掉 docEventsFromEntries 里的 `NoMemory: e.NoMemory` ⇒ 红。
func TestNoMemorySurvivesL2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := coldcontext.NewStore(dir, tokWords)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	doc, err := s.ContextToColdContext("context_archived", []coldcontext.ContextEntry{{
		Source:   "agent",
		Content:  "终端 t1 中的进程已退出",
		Response: "收到",
		NoMemory: true,
	}}, nil, nil, nil, nil)
	if err != nil || doc == nil {
		t.Fatalf("ContextToColdContext: %v / %v", err, doc)
	}
	if !doc.Events[0].NoMemory {
		t.Fatal("★ NoMemory 声明在 L1→L2 丢失（L3 蒸馏就会把这段当正常内容抽三元组）")
	}
	s.Stop()

	// 跨重启也要在：它是声明，不是运行时临时状态。
	reopened := coldcontext.NewStore(dir, tokWords)
	if err := reopened.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer reopened.Stop()
	reloaded := reopened.Get(doc.ID)
	if reloaded == nil || len(reloaded.Events) == 0 {
		t.Fatal("★ 重开后文档或事件丢失")
	}
	if !reloaded.Events[0].NoMemory {
		t.Fatal("★ 重开后 NoMemory 声明丢失（没落盘）")
	}
}
