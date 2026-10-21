package document

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func TestInsertAndQuery(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	doc := &Doc{
		Summary: "用户喜欢喝咖啡",
		Content: "用户提到他每天早上都会喝一杯黑咖啡",
		Tags:    []string{"咖啡", "习惯"},
		Source:  "manual",
	}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}

	if doc.ID == "" {
		t.Error("doc ID should be auto-generated")
	}

	stats := s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc, got %d", stats["doc_count"])
	}
}

func TestQuery(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_query_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "咖啡是一种饮品", Content: "咖啡因提神", Source: "manual"})
	s.Insert(&Doc{Summary: "茶叶也有咖啡因", Content: "茶和咖啡都提神", Source: "manual"})
	s.Insert(&Doc{Summary: "今天天气很好", Content: "适合出去散步", Source: "manual"})

	results := s.Query("咖啡", 5)
	if len(results) == 0 {
		t.Fatal("expected results for '咖啡'")
	}

	if results[0].AccessCount <= 0 {
		t.Error("access count should be updated on query")
	}
}

func TestContextToDoc(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_ctx_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	entries := []ContextEntry{
		{Timestamp: time.Now(), Source: "user", Content: "我喜欢编程", Response: "很好"},
		{Timestamp: time.Now(), Source: "user", Content: "特别是Go语言", Response: "Go很棒"},
	}

	doc, err := s.ContextToDoc("test", entries, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if doc == nil {
		t.Fatal("expected non-nil doc")
	}
	if doc.Summary == "" {
		t.Error("summary should not be empty")
	}
	if doc.Content == "" {
		t.Error("content should not be empty")
	}
}

func TestFindColdDocs(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_cold_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	hot := &Doc{Summary: "常用的信息", Content: "经常被查询", Source: "manual"}
	hot.AccessCount = 10
	hot.LastAccess = time.Now()
	s.Insert(hot)

	cold := &Doc{Summary: "很久没用的信息", Content: "几乎不被访问", Source: "manual"}
	s.Insert(cold)
	// Insert 会重置 LastAccess，手动改为过去的
	cold.LastAccess = time.Now().Add(-100 * time.Hour)
	cold.AccessCount = 1

	// 应该只找到 cold（72h 前未访问且访问 ≤ 2）
	coldDocs := s.FindColdDocs(72*time.Hour, 2)
	if len(coldDocs) != 1 {
		t.Fatalf("expected 1 cold doc, got %d", len(coldDocs))
	}
	if coldDocs[0].Summary != "很久没用的信息" {
		t.Errorf("expected cold doc, got %s", coldDocs[0].Summary)
	}
}

func TestRecentDocs(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_recent_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "第一条", Content: "a", Source: "manual"})
	time.Sleep(time.Millisecond)
	s.Insert(&Doc{Summary: "第二条", Content: "b", Source: "manual"})

	recent := s.RecentDocs(1)
	if len(recent) != 1 {
		t.Fatalf("expected 1 recent doc, got %d", len(recent))
	}
	if recent[0].Summary != "第二条" {
		t.Errorf("expected newest doc, got %s", recent[0].Summary)
	}
}

func TestReindex(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_reindex_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "测试重索引", Content: "验证索引重建", Source: "manual"})
	s.Reindex()

	results := s.Query("重索引", 5)
	if len(results) == 0 {
		t.Error("reindex should preserve searchability")
	}
}

func TestSummarizeEntries(t *testing.T) {
	entries := []ContextEntry{
		{Source: "user", Content: "今天天气如何"},
		{Source: "user", Content: "明天会下雨吗"},
	}
	summary := summarizeEntries(entries, func(s string) string { return s }, nil, nil)
	if summary == "" {
		t.Error("summary should not be empty")
	}
	if !contains(summary, "2") {
		t.Errorf("summary should mention count, got: %s", summary)
	}
}

func TestExtractKeywords(t *testing.T) {
	kws := memory.ExtractKeywords("今天天气很好")
	if len(kws) == 0 {
		t.Error("should extract keywords from Chinese text")
	}
}

func TestExtractTags(t *testing.T) {
	entries := []ContextEntry{
		{Content: "我喜欢喝咖啡和编程"},
	}
	tags := extractTags(entries, func(s string) string { return s }, nil, nil)
	if len(tags) == 0 {
		t.Error("should extract tags")
	}
}

func TestInsertEmptyDoc(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_empty_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	doc := &Doc{Summary: "", Content: "", Source: "manual"}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}
	if doc.ID == "" {
		t.Error("doc ID should be generated even for empty content")
	}
}

func TestPersistence(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_persist_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	// 写
	s1 := NewStore(dir, memory.TokenizeWords)
	s1.Start()
	s1.Insert(&Doc{Summary: "持久化测试", Content: "应该被保存到磁盘", Source: "manual"})
	s1.Stop()

	// 读
	s2 := NewStore(dir, memory.TokenizeWords)
	s2.Start()
	defer s2.Stop()

	stats := s2.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after reload, got %d", stats["doc_count"])
	}

	results := s2.Query("持久化", 5)
	if len(results) == 0 {
		t.Error("search should work after reload")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestFlushNoDirty(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_flush_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()

	// 不插任何文档，flush 不应报错
	s.Stop()
}

func TestRemove(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_remove_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "会被删除", Content: "a", Source: "manual"})
	s.Insert(&Doc{Summary: "会保留", Content: "b", Source: "manual"})

	// 删除前应该有 2 个
	stats := s.Stats()
	if stats["doc_count"].(int) != 2 {
		t.Fatalf("expected 2 docs before remove, got %d", stats["doc_count"])
	}

	// 遍历找到 "会被删除" 的 ID
	var rmID string
	for _, d := range s.docs {
		if d.Summary == "会被删除" {
			rmID = d.ID
			break
		}
	}
	if rmID == "" {
		t.Fatal("could not find test doc")
	}

	s.Remove(rmID)

	stats = s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after remove, got %d", stats["doc_count"])
	}

	// 搜索不应再找到
	results := s.Query("删除", 5)
	if len(results) > 0 {
		t.Error("removed doc should not appear in search results")
	}
}

func TestRemoveNonexistent(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_rm_nonexist_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	s.Insert(&Doc{Summary: "一个文档", Content: "x", Source: "manual"})

	// 删除不存在的 ID 不应 panic
	s.Remove("nonexistent_id")

	stats := s.Stats()
	if stats["doc_count"].(int) != 1 {
		t.Errorf("expected 1 doc after remove nonexistent, got %d", stats["doc_count"])
	}
}

func TestSummarizeEntriesWithToolCleanFn(t *testing.T) {
	tests := []struct {
		name        string
		toolCleanFn func(name, output string) string
		wantTopics  []string
		notTopics   []string
	}{
		{
			name:        "nil toolCleanFn uses raw output",
			toolCleanFn: nil,
			wantTopics:  []string{"手机", "电脑"},
			notTopics:   nil,
		},
		{
			name: "NoMemory returns empty skips tool output",
			toolCleanFn: func(name, output string) string {
				return ""
			},
			wantTopics: nil,
			notTopics:  []string{"手机", "电脑"},
		},
		{
			name: "Cleaner applies filter",
			toolCleanFn: func(name, output string) string {
				return "电脑 编程"
			},
			wantTopics: []string{"电脑", "编程"},
			notTopics:  nil,
		},
	}

	entry := ContextEntry{
		Content: "今天天气",
		ToolResults: []ToolResultItem{
			{Name: "test_tool", Output: "手机 电脑"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			summary := summarizeEntries([]ContextEntry{entry}, func(s string) string { return s }, tc.toolCleanFn, nil)
			for _, w := range tc.wantTopics {
				if !contains(summary, w) {
					t.Errorf("summary should contain %q, got: %s", w, summary)
				}
			}
			for _, n := range tc.notTopics {
				if contains(summary, n) {
					t.Errorf("summary should NOT contain %q, got: %s", n, summary)
				}
			}
		})
	}
}

func TestExtractTagsWithToolCleanFn(t *testing.T) {
	entries := []ContextEntry{
		{
			Content: "对话",
			ToolResults: []ToolResultItem{
				{Name: "search", Output: "编程和咖啡"},
			},
		},
	}

	// toolCleanFn 返回 "" → NoMemory，工具输出被跳过
	tagsSkip := extractTags(entries, func(s string) string { return s }, func(name, output string) string { return "" }, nil)
	for _, tag := range tagsSkip {
		if tag == "编程" || tag == "咖啡" {
			t.Errorf("NoMemory tool should not contribute keywords, got tag: %s", tag)
		}
	}

	// toolCleanFn 返回清洗文本 → 用清洗后内容提取关键词
	tagsClean := extractTags(entries, func(s string) string { return s }, func(name, output string) string { return "咖啡 编程" }, nil)
	found := false
	for _, tag := range tagsClean {
		if tag == "编程" {
			found = true
		}
	}
	if !found {
		t.Errorf("cleaner output keywords should appear in tags, got: %v", tagsClean)
	}
}

func TestContextToDocContentPreservesRawToolOutput(t *testing.T) {
	dir, err := os.MkdirTemp("", "doc_toolclean_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	s := NewStore(dir, memory.TokenizeWords)
	s.Start()
	defer s.Stop()

	entries := []ContextEntry{
		{
			Timestamp: time.Now(),
			Source:    "user",
			Content:   "查天气",
			ToolResults: []ToolResultItem{
				{Name: "weather", Output: "{\"temp\": 25}"},
			},
		},
	}

	// toolCleanFn 返回清洗文本，但 Content 必须保留原始输出
	cleaner := func(name, output string) string {
		return "天气 温度"
	}
	doc, err := s.ContextToDoc("test", entries, nil, nil, cleaner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(doc.Content, "{\"temp\": 25}") {
		t.Errorf("Content should preserve raw tool output, got: %s", doc.Content)
	}
	if doc.Summary == "" {
		t.Error("summary should not be empty")
	}
}

// ———— 回归：向量迁移的落盘与维度一致性 ————
//
// 以下四条来自 v1.2.0-beta.2 的压测（报告 /var/tmp/stress/REPORT.md）：
// 迁移结果不落盘（每次启动白算一遍）、块指纹对但维度错时污染文档向量、
// 以及 Insert 与 loadAll 的 ID 约定不对称。

// fakeSpace 是可控的统一向量空间；calls 记录被真正要求算向量的次数，
// 用来直接证明「已对齐的文档不再重算」——比读日志断言可靠。
type fakeSpace struct {
	fp    string
	dim   int
	calls int
}

func (f *fakeSpace) VectorizeDense(string) ([]float64, error) {
	f.calls++
	v := make([]float64, f.dim)
	for i := range v {
		v[i] = float64(i + 1)
	}
	return v, nil
}
func (f *fakeSpace) EmbedImageDense([]byte, string) ([]float64, error) { return f.VectorizeDense("") }
func (f *fakeSpace) Fingerprint() string                               { return f.fp }
func (f *fakeSpace) Dim() int                                          { return f.dim }
func (f *fakeSpace) Loaded() bool                                      { return true }
func (f *fakeSpace) Close()                                            {}

// 迁移结果必须落盘：迁移后换一个 Store 实例（模拟重启）读到的应是新空间向量，
// 且再跑一次迁移不应重算任何文档。
func TestBuildDenseIndexPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	sp := &fakeSpace{fp: "space-NEW-8", dim: 8}

	s1 := NewStore(dir, memory.TokenizeWords)
	if err := s1.Start(); err != nil {
		t.Fatal(err)
	}
	// 模拟上个向量空间留下的状态：维度与指纹都对不上
	stale := make([]float64, 999)
	for i := range stale {
		stale[i] = 0.01
	}
	if err := s1.Insert(&Doc{ID: "doc_persist", Summary: "迁移", Content: "落盘",
		DenseVec: stale, DenseFP: "space-OLD-999"}); err != nil {
		t.Fatal(err)
	}
	s1.SetDenseSpace(sp)
	s1.BuildDenseIndex(sp)
	if got := s1.Get("doc_persist"); got == nil || len(got.DenseVec) != 8 || got.DenseFP != sp.fp {
		t.Fatalf("迁移未在内存生效: %+v", got)
	}
	// 不调用 Stop 就另开一个实例：体现「迁移当场落盘」，不依赖关停
	s2 := NewStore(dir, memory.TokenizeWords)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	loaded := s2.Get("doc_persist")
	if loaded == nil {
		t.Fatal("重启后文档不见了")
	}
	if len(loaded.DenseVec) != 8 || loaded.DenseFP != sp.fp {
		t.Fatalf("迁移结果未落盘：期望 dim=8 fp=%q，实际 dim=%d fp=%q"+
			"（后果：每次启动都重算同一批文档，磁盘状态永不收敛）",
			sp.fp, len(loaded.DenseVec), loaded.DenseFP)
	}
	// 已对齐 → 一次向量计算都不该发生
	sp2 := &fakeSpace{fp: sp.fp, dim: 8}
	s2.SetDenseSpace(sp2)
	s2.BuildDenseIndex(sp2)
	if sp2.calls != 0 {
		t.Fatalf("已对齐的文档被重算了 %d 次（期望 0）", sp2.calls)
	}
}

// 块向量维度与当前空间不符时不得参与融合：否则 512 维文本 + 2048 维块
// 会被 FuseVectors 按最大维度拼成 2048 维、并带上当前指纹，导致该文档在
// 检索侧被长度守卫永久跳过且每次启动重算。
func TestDenseForIgnoresBlockWithMismatchedDim(t *testing.T) {
	dir := t.TempDir()
	sp := &fakeSpace{fp: "space-NEW-8", dim: 8}
	s := NewStore(dir, memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	bad := make([]float64, 2048)
	for i := range bad {
		bad[i] = 0.02
	}
	good := make([]float64, 8)
	for i := range good {
		good[i] = 0.5
	}
	doc := &Doc{ID: "doc_bad", Summary: "坏块", Content: "文本向量应当生效",
		Blocks: []memory.MemoryBlock{
			{ID: "blk_bad", Vector: bad, Fingerprint: sp.fp},   // 指纹对、维度错 → 必须忽略
			{ID: "blk_good", Vector: good, Fingerprint: sp.fp}, // 指纹与维度都对 → 参与融合
		}}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}
	s.SetDenseSpace(sp)
	s.BuildDenseIndex(sp)

	got := s.Get("doc_bad")
	if got == nil {
		t.Fatal("文档未加载")
	}
	if len(got.DenseVec) != 8 {
		t.Fatalf("坏块污染了文档向量：期望 %d 维，实际 %d 维（指纹 %q）",
			8, len(got.DenseVec), got.DenseFP)
	}
	// 同维度的正常块仍须参与融合：不应因为这次修复而整体失效
	textOnly := &fakeSpace{fp: sp.fp, dim: 8}
	textVec, _ := textOnly.VectorizeDense(doc.Summary + " " + doc.Content)
	if equalFloats(got.DenseVec, textVec) {
		t.Fatal("同维度的媒体块没有参与融合（修复过度）")
	}
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Insert 接受任意 ID，loadAll 就必须把它读回来，否则自定义 ID 的文档
// 重启后静默消失。
func TestLoadAllLoadsCustomID(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStore(dir, memory.TokenizeWords)
	if err := s1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert(&Doc{ID: "my-notes", Summary: "自定义 ID", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	s1.Stop()

	s2 := NewStore(dir, memory.TokenizeWords)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	if s2.Get("my-notes") == nil {
		t.Fatal("自定义 ID 的文档重启后消失（Insert 与 loadAll 的 ID 约定不对称）")
	}
}

// Stop 必须把内存态变更写盘（关停链上没有它时 flush 形同虚设）。
func TestStopFlushesDirtyDocs(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStore(dir, memory.TokenizeWords)
	if err := s1.Start(); err != nil {
		t.Fatal(err)
	}
	if err := s1.Insert(&Doc{ID: "doc_flush", Summary: "关停落盘", Content: "内容"}); err != nil {
		t.Fatal(err)
	}
	// 直接改内存并置脏，模拟「只在内存里发生的变更」
	s1.mu.Lock()
	s1.docs["doc_flush"].Summary = "关停落盘（已改）"
	s1.dirty = true
	s1.mu.Unlock()
	s1.Stop()

	s2 := NewStore(dir, memory.TokenizeWords)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()
	if got := s2.Get("doc_flush"); got == nil || got.Summary != "关停落盘（已改）" {
		t.Fatalf("Stop 未落盘: %+v", got)
	}
}

// TestQueryScoredConcurrentAndDirty 锁死两点：
//  1. QueryScored 更新访问计数必须离开读锁（旧实现在 RLock 下写，-race 会报）；
//  2. 更新后必须置脏，否则优雅关停也不落盘、FindColdDocs 的冷度判据跨重启失真。
func TestQueryScoredConcurrentAndDirty(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	for i := 0; i < 8; i++ {
		// 文档必须互相区分：若 8 篇文本完全相同，查询词出现在每一篇里，
		// IDF = log(N/df) = 0，词向量全零、检索恒为空，测试就成了假阴性。
		summary, content := "无关的天气与散步记录", "今天适合出门"
		if i < 3 {
			summary, content = "并发检索目标", "并发内容"
		}
		if err := s.Insert(&Doc{ID: fmt.Sprintf("d%d", i), Summary: summary, Content: content}); err != nil {
			t.Fatal(err)
		}
	}
	// 清掉 Insert 置的脏位，验证 QueryScored 自己会置脏。
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 30; k++ {
				s.QueryScored("并发检索目标", 5)
				s.FindColdDocs(72*time.Hour, 2)
			}
		}()
	}
	wg.Wait()

	s.mu.RLock()
	dirty := s.dirty
	access := s.docs["d0"].AccessCount
	s.mu.RUnlock()
	if access <= 1 {
		t.Errorf("AccessCount 未随检索累加: %d", access)
	}
	if !dirty {
		t.Error("QueryScored 更新访问计数后未置脏")
	}
}
