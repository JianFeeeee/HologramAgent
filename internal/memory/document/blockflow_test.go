package document

// L1 ↔ L2 同构协议的判据。
//
// 依据（不是我的推断）：
//
//	assets/docs/zh/ARCHITECTURE.md「三层记忆」
//	  · L1 与 L2 结构完全同构，唯一区别是「是否被直接投喂给模型」
//	  · 正文（含多媒体正文）内联在块里：文本块的 Text 是正文，媒体块的 Data 是字节
//	  · 任何一层都不压缩（视图不是记忆）
//
// 每条判据都对应一个真实发生过的断点，且做过变异测试（见各条注释）。

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newDocTestStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir(), memory.TokenizeWords)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	return s
}

// ① 正文完整性：归档后能逐条取回**完整**正文。
//
// 断点史：原实现 ContextToDoc 把每条事件拍平成一行，
// truncate(Response,100) / truncate(工具输出,200) —— 原文在落库那一刻就不见了。
// 生产实测：1000 篇归档文档中 15.1% 的 response 段正好 103 字符。
//
// 变异：把 docBlocksForEntry 里文本块的 Text 改成截断版 ⇒ 本判据必须红。
func TestL2_ArchivePreservesFullText(t *testing.T) {
	s := newDocTestStore(t)

	longResp := strings.Repeat("这是一段用于验证原文完整性的长正文", 200) + "【哨兵ZQ7】"
	longInput := strings.Repeat("这是用户的原始输入内容", 100) + "【输入哨兵K9】"
	ts := time.Date(2026, 3, 4, 10, 11, 12, 345678901, time.UTC)

	_, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: ts,
		Source:    "webui",
		Content:   longInput,
		Response:  longResp,
		ToolsUsed: []string{"report_fetch"},
		ToolResults: []ToolResultItem{
			{Name: "report_fetch", Output: strings.Repeat("工具输出", 500) + "【工具哨兵M3】"},
		},
	}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	var d *Doc
	for _, cand := range s.RecentDocs(10) {
		d = cand
	}
	if d == nil {
		t.Fatal("没有归档出文档")
	}
	if !d.HasEvents() {
		t.Fatal("文档没有 Events —— L2 必须与 L1 同构，事件列表是主体")
	}

	ev := d.Events[0]
	in, resp, _ := ev.InOut()
	if in != longInput {
		t.Errorf("Input 不完整：len=%d，原长 %d", len(in), len(longInput))
	}
	if resp != longResp {
		t.Errorf("Response 不完整：len=%d，原长 %d\n"+
			"L2 是 L1 的下沉层，不是摘要层 —— 正文不能在落库时被截断", len(resp), len(longResp))
	}
	if !strings.Contains(resp, "【哨兵ZQ7】") {
		t.Error("Response 丢了结尾哨兵")
	}
	if !strings.Contains(in, "【输入哨兵K9】") {
		t.Error("Input 丢了结尾哨兵")
	}
	// 工具输出是事件结构的一部分，必须原样保留。
	if len(ev.ToolResults) != 1 || !strings.Contains(ev.ToolResults[0].Output, "【工具哨兵M3】") {
		t.Errorf("工具输出不完整或丢失: %+v", ev.ToolResults)
	}
	// ToolsUsed 以前完全不存。
	if len(ev.ToolsUsed) != 1 || ev.ToolsUsed[0] != "report_fetch" {
		t.Errorf("ToolsUsed 丢失: %+v", ev.ToolsUsed)
	}
	// 原始时刻（纳秒精度）必须保留，不能只剩 HH:MM。
	if !ev.Timestamp.Equal(ts) {
		t.Errorf("时间戳不精确：got %s，want %s", ev.Timestamp, ts)
	}
}

// ② 正文各自成块：Input 一块、Response 一块，都是 BlockText。
//
// 拍板（2026-10-22）：正文各自一块，不合成一个块 —— 两段内容语义常不同，
// 分别成块才能各自被检索。
//
// 变异：把两块合并成一块 ⇒ 本判据必须红。
func TestL2_InputAndResponseAreSeparateTextBlocks(t *testing.T) {
	s := newDocTestStore(t)

	media := memory.MemoryBlock{
		ID: memory.MediaBlockID("image/png", []byte("x")), Modality: memory.BlockImage,
		Data: []byte("x"), MIME: "image/png", Size: 1,
	}
	_, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now(),
		Source:    "qq",
		Content:   "用户问：紫蓝红三色带怎么配",
		Response:  "回答：紫蓝红按 6:3:1 配",
		Blocks:    []memory.MemoryBlock{media},
	}}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]

	var texts []memory.MemoryBlock
	for _, b := range d.Events[0].Blocks {
		if b.Modality == memory.BlockText {
			texts = append(texts, b)
		}
	}
	if len(texts) != 2 {
		t.Fatalf("一条事件应产出 2 个文本块（Input/Response 各一），实际 %d", len(texts))
	}
	if texts[0].Text != "用户问：紫蓝红三色带怎么配" {
		t.Errorf("第一个文本块应是 Input 正文，实际 %q", texts[0].Text)
	}
	if texts[1].Text != "回答：紫蓝红按 6:3:1 配" {
		t.Errorf("第二个文本块应是 Response 正文，实际 %q", texts[1].Text)
	}
	// 文本块的 CreatedAt 必须是事件时刻（回填靠它定位）。
	if !texts[0].CreatedAt.Equal(d.Events[0].Timestamp) {
		t.Error("文本块 CreatedAt 应等于事件时间戳")
	}
	// 媒体块仍在，且不被文本块顶掉。
	var hasMedia bool
	for _, b := range d.Events[0].Blocks {
		if b.ID == media.ID {
			hasMedia = true
		}
	}
	if !hasMedia {
		t.Error("媒体块在归档后丢失")
	}
}

// ③ 逐条保留：一次归档 N 条事件，L2 里就是 N 条，不合并、不丢。
//
// 变异：只保留第一条 ⇒ 红。
func TestL2_KeepsEveryEventIndividually(t *testing.T) {
	s := newDocTestStore(t)

	base := time.Date(2026, 5, 6, 8, 0, 0, 0, time.UTC)
	var entries []ContextEntry
	for i := 0; i < 5; i++ {
		entries = append(entries, ContextEntry{
			Timestamp: base.Add(time.Duration(i) * 13 * time.Minute),
			Source:    "cli",
			Content:   "第" + string(rune('0'+i)) + "条输入",
			Response:  "第" + string(rune('0'+i)) + "条回答",
		})
	}
	if _, err := s.ContextToDoc("context_archived", entries, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]

	if len(d.Events) != 5 {
		t.Fatalf("Events = %d，期望 5（逐条保留，不合并）", len(d.Events))
	}
	for i, ev := range d.Events {
		wantTS := base.Add(time.Duration(i) * 13 * time.Minute)
		if !ev.Timestamp.Equal(wantTS) {
			t.Errorf("第 %d 条时间戳错位: %s vs %s", i, ev.Timestamp, wantTS)
		}
		in, resp, _ := ev.InOut()
		if in != "第"+string(rune('0'+i))+"条输入" || resp != "第"+string(rune('0'+i))+"条回答" {
			t.Errorf("第 %d 条正文错位: input=%q response=%q", i, in, resp)
		}
	}
}

// ④ Content 是视图而不是事实源：它由事件渲染，且不截断。
//
// 拍板（2026-10-22）：Content 保留为视图。
// 判据要点：如果 Content 里还能看到完整正文，说明视图没有偷偷丢信息；
// 同时原文的权威来源是块，Content 改坏了也不影响 InOut()。
func TestL2_ContentIsRenderedViewNotSource(t *testing.T) {
	s := newDocTestStore(t)
	long := strings.Repeat("视图不该丢信息", 300) + "【视图哨兵】"

	if _, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now(), Source: "webui",
		Content: "输入", Response: long,
	}}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]

	if !strings.Contains(d.Content, "【视图哨兵】") {
		t.Error("Content 视图里丢了正文尾部 —— 视图也不该在存储层主动截断")
	}
	// 权威来源是块：把 Content 清空，InOut() 仍要能读回完整正文。
	d.Content = ""
	_, resp, _ := d.Events[0].InOut()
	if !strings.Contains(resp, "【视图哨兵】") {
		t.Error("正文权威来源应是块，不是 Content")
	}
}

// ⑤ 幂等：同一批事件重复归档，块 ID 不变、事件不翻倍。
//
// 为何重要：Prune 可能因重试/重复触发而对同一批事件归档两次；
// 块 ID 由内容派生，必须收敛而不是长出两套。
func TestL2_RepeatedArchiveIsIdempotent(t *testing.T) {
	s := newDocTestStore(t)
	ts := time.Date(2026, 7, 8, 9, 10, 11, 123456789, time.UTC)
	entries := []ContextEntry{{
		Timestamp: ts, Source: "qq", Content: "同一条输入", Response: "同一条回答",
	}}

	if _, err := s.ContextToDoc("context_archived", entries, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	first := s.RecentDocs(1)[0]
	id1 := first.Events[0].Blocks[0].ID

	if _, err := s.ContextToDoc("context_archived", entries, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// 内容哈希相同 ⇒ 命中已有文档（不新建），事件列表被重写为同一份。
	if n := len(s.Stats()); n == 0 {
		t.Fatal("stats 异常")
	}
	if got := len(s.RecentDocs(10)); got != 1 {
		t.Fatalf("重复归档产生了 %d 篇文档，期望 1（同内容应命中已有文档）", got)
	}
	d := s.RecentDocs(1)[0]
	if len(d.Events) != 1 {
		t.Fatalf("重复归档后 Events=%d，期望 1（不应翻倍）", len(d.Events))
	}
	if d.Events[0].Blocks[0].ID != id1 {
		t.Errorf("同一事件重复归档得到不同块 ID：%s → %s（块 ID 必须内容稳定）",
			id1, d.Events[0].Blocks[0].ID)
	}
}

// ⑥ 存量兼容：没有 Events 的旧文档必须能被读，且不伪造成"有原始事件"。
//
// 决定（2026-10-22）：存量等系统自然冲刷，不迁移。
// 但新代码必须能读旧格式 —— 否则升级瞬间老文档全部不可召回。
func TestL2_LegacyDocWithoutEventsStillLoads(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir, memory.TokenizeWords)

	// 手工写一份旧格式 JSON（无 events 字段）。
	legacy := map[string]interface{}{
		"id":           "doc_legacy_1",
		"summary":      "旧格式文档",
		"content":      "[10:00] old: 这是旧格式的渲染视图",
		"source":       "context_archived",
		"created_at":   "2026-01-02T03:04:05Z",
		"updated_at":   "2026-01-02T03:04:05Z",
		"access_count": 1,
		"last_access":  "2026-01-02T03:04:05Z",
	}
	raw, _ := json.Marshal(legacy)
	if err := writeFile(dir+"/doc_legacy_1.json", raw); err != nil {
		t.Fatal(err)
	}

	if err := s.Start(); err != nil {
		t.Fatalf("读取旧格式失败（不得 panic）：%v", err)
	}
	got := s.Get("doc_legacy_1")
	if got == nil {
		t.Fatal("旧格式文档未被加载")
	}
	if got.HasEvents() {
		t.Error("旧格式文档不该被伪造成有 Events")
	}
	if got.Content == "" {
		t.Error("旧格式文档的 Content 视图丢失")
	}
}

func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }

// ⑦ 蒸馏/抽取必须基于原文，不能基于视图。
//
// 断点史：docToTriples 曾抽取 doc.Content —— 而 Content 在 Events 引入前
// 是压缩渲染（Response 截 100 rune）。在被截断的句子上做依存句法分析，
// 往往解析不出主谓宾，三元组抽取直接失败（生产日志里大量
// 「三元组 N 条全被实体名校验拒绝」即与此相关）。
//
// 本判据守的是 document 包这一侧的能力：
// 只要给出原文，就能完整取回 —— 不被视图的形变影响。
func TestL2_OriginalTextAvailableIndependentOfView(t *testing.T) {
	s := newDocTestStore(t)

	full := strings.Repeat("原文必须完整用于抽取与蒸馏", 500) + "【原文哨兵D2】"
	if _, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now(), Source: "webui",
		Content: "输入", Response: full,
	}}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]

	// 刻意把视图弄坏：原文的权威来源必须是块，不是 Content。
	d.Content = "视图被破坏了"

	var b strings.Builder
	for _, ev := range d.Events {
		_, resp, _ := ev.InOut()
		b.WriteString(resp)
	}
	if got := b.String(); got != full {
		t.Errorf("事件正文取不回原文：len=%d，原长 %d", len(got), len(full))
	}
	if !strings.Contains(b.String(), "【原文哨兵D2】") {
		t.Error("原文尾部丢失")
	}
}

// ──────────────────────────────────────────────
// 压力测试：新形态（事件列表 + 正文成块）的规模与并发
// ──────────────────────────────────────────────

// 大规模归档：500 条事件 → 1000 个文本块，必须能落盘再完整读回。
//
// 关注点：新形态把每条事件变成 2 个块，块数翻倍。
// 若某处仍按"一篇文档一个块"或"块数=事件数"假设，规模上来就会暴露。
func TestL2_StressLargeArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("压力测试")
	}
	const n = 500
	s := newDocTestStore(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := make([]ContextEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = ContextEntry{
			Timestamp: base.Add(time.Duration(i) * time.Second),
			Source:    "stress",
			Content:   strings.Repeat("输入", 20) + "|" + string(rune('A'+i%26)),
			Response:  strings.Repeat("回答", 20) + "|" + string(rune('a'+i%26)),
		}
	}

	if _, err := s.ContextToDoc("context_archived", entries, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]
	if len(d.Events) != n {
		t.Fatalf("Events = %d，期望 %d", len(d.Events), n)
	}
	var textBlocks int
	for _, ev := range d.Events {
		for _, b := range ev.Blocks {
			if b.Modality == memory.BlockText {
				textBlocks++
			}
		}
	}
	if textBlocks != 2*n {
		t.Fatalf("文本块 = %d，期望 %d（每条事件 Input/Response 各一块）", textBlocks, 2*n)
	}

	// 落盘再重新加载：验证序列化能承载这个规模、且逐条完整。
	s.Stop()
	s2 := NewStore(s.dir, memory.TokenizeWords)
	if err := s2.Start(); err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	defer s2.Stop()

	d2 := s2.Get(d.ID)
	if d2 == nil {
		t.Fatal("重新加载后文档丢失")
	}
	if len(d2.Events) != n {
		t.Fatalf("重新加载后 Events = %d，期望 %d", len(d2.Events), n)
	}
	// 抽查首尾，确认正文没被合并或错位。
	for _, i := range []int{0, n / 2, n - 1} {
		in, resp, _ := d2.Events[i].InOut()
		wantIn := strings.Repeat("输入", 20) + "|" + string(rune('A'+i%26))
		wantResp := strings.Repeat("回答", 20) + "|" + string(rune('a'+i%26))
		if in != wantIn || resp != wantResp {
			t.Errorf("第 %d 条正文错位: in=%q resp=%q", i, in, resp)
		}
		if !d2.Events[i].Timestamp.Equal(base.Add(time.Duration(i) * time.Second)) {
			t.Errorf("第 %d 条时间戳错位", i)
		}
	}
}

// 并发召回压力：Consume 持有写锁，而 QueryScored 走读锁 + markAccess。
// 新形态下 Consume 要回填更多数据，不能引入竞态或丢文档。
func TestL2_StressConcurrentQueryAndConsume(t *testing.T) {
	if testing.Short() {
		t.Skip("压力测试")
	}
	s := newDocTestStore(t)

	const docs = 60
	for i := 0; i < docs; i++ {
		if _, err := s.ContextToDoc("context_archived", []ContextEntry{{
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Source:    "stress",
			Content:   "并发查询目标 " + string(rune('A'+i%26)) + " 编号" + string(rune('0'+i%10)),
			Response:  "回答 " + string(rune('A'+i%26)),
		}}, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				switch w % 3 {
				case 0:
					s.QueryScored("并发查询目标", 5)
				case 1:
					s.Consume("并发查询目标", 3)
				case 2:
					s.FindColdDocs(time.Hour, 10)
				}
			}
		}(w)
	}
	wg.Wait()
	// 只要不 panic / 不 -race 报错即通过；文档数只会因 Consume 减少。
	if got := len(s.RecentDocs(1000)); got > docs {
		t.Errorf("文档数异常增长: %d > %d", got, docs)
	}
}

// 大正文：单条事件 1MB 正文，必须完整往返（不截断、不 OOM）。
func TestL2_StressLargePayloadRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("压力测试")
	}
	s := newDocTestStore(t)

	huge := strings.Repeat("大正文压测", 250000) + "【巨尾哨兵P8】" // ~1.25MB
	if _, err := s.ContextToDoc("context_archived", []ContextEntry{{
		Timestamp: time.Now(), Source: "stress",
		Content: "输入", Response: huge,
	}}, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	d := s.RecentDocs(1)[0]
	_, resp, _ := d.Events[0].InOut()
	if len(resp) != len(huge) {
		t.Fatalf("大正文往返长度不符: %d vs %d", len(resp), len(huge))
	}
	if !strings.Contains(resp, "【巨尾哨兵P8】") {
		t.Error("大正文尾部丢失")
	}
}

// ⑧ simpleHash 必须 O(n) 且定长。
//
// 断点史（2026-10-22 压力测试实测）：
//
//	h += fmt.Sprintf("%x", c)   // 每 rune 一次 Sprintf + 字符串拼接 ⇒ O(n²)
//
// 1.25MB 正文归档耗时 **270 秒**；且哈希串长度是内容的 2.2 倍
// （生产实测 content 52072 字符 → content_hash 114491 字符）。
//
// 本判据同时钉两点：长度定长（不随内容膨胀）、时间线性（不是 O(n²)）。
func TestL2_HashIsLinearAndFixedLength(t *testing.T) {
	small := "小内容"
	large := strings.Repeat("大内容", 200000)

	hs, hl := simpleHash(small), simpleHash(large)
	if len(hs) != len(hl) {
		t.Errorf("哈希长度随内容变化: %d vs %d（必须定长）", len(hs), len(hl))
	}
	if len(hl) > 64 {
		t.Errorf("哈希本身过长: %d 字符（会成为文档体积负担）", len(hl))
	}

	// 线性性：64 倍输入不应产生平方级增长。留足余量避免机器抖动误判。
	t0 := time.Now()
	for i := 0; i < 32; i++ {
		simpleHash(large)
	}
	big := time.Since(t0)

	t1 := time.Now()
	for i := 0; i < 32; i++ {
		simpleHash(large[:len(large)/64])
	}
	sm := time.Since(t1)

	// O(n²) 下这个比值应接近 64²=4096；线性下约 64。
	// 取 300 作为阈值：远离线性（64），远低于平方（4096）。
	if sm > 0 && big > 300*sm {
		t.Errorf("哈希耗时随输入超线性增长：大 %.1fms / 小 %.1fms = %.0f 倍"+
			"（输入差 64 倍，线性应约 64 倍，O(n²) 会到 4096 倍）",
			float64(big.Microseconds())/1000, float64(sm.Microseconds())/1000,
			float64(big)/float64(sm))
	}
}
