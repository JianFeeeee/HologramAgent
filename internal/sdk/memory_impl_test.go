package sdk

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	coldcontext "github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

// 插件边界的媒体透传测试（块 union 化之后）。
//
// 断言的都是不变量，而不是"函数被调过"：
//  1. 插件填的字段一个都不许丢（旧实现静默裁掉 Confidence/类型/SentenceText）；
//  2. 媒体字节进块（Data），不再是独立 digest；
//  3. 媒体块挂到正确的 owner 上，删除时随之释放；
//  4. 块身份是内容派生（blk_media_<sha256(mime+data)>），同内容同块。

func newTestStores(t *testing.T) (*memory.GraphDB, *coldcontext.Store, *text.Memory) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ds := coldcontext.NewStore(filepath.Join(dir, "documents"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatalf("doc store start: %v", err)
	}
	t.Cleanup(func() { ds.Stop() })

	tm := text.New(filepath.Join(dir, "text"))
	if err := tm.Start(); err != nil {
		t.Fatalf("text memory start: %v", err)
	}
	t.Cleanup(func() { tm.Stop() })

	return g, ds, tm
}

// mediaBlock 构造一个内容派生 ID 的媒体块（块的底层身份）。
func mediaBlock(mime string, payload []byte) memory.MemoryBlock {
	return memory.MemoryBlock{
		ID:       memory.MediaBlockID(mime, payload),
		Modality: memory.BlockImage,
		Data:     payload,
		MIME:     mime,
		Size:     int64(len(payload)),
	}
}

// ---------- 图记忆 ----------

// 旧实现只搬 Subject/Relation/Object，其余字段静默丢弃：
// 插件标注的类型全部落成默认 Concept，置信度全成 1.0，SentenceText 直接消失。
func TestGraphCommit_CarriesAllFields(t *testing.T) {
	g, _, _ := newTestStores(t)
	m := NewGraphMemory(g)

	err := m.Commit([]Triple{{
		Subject:      "张三",
		Relation:     "养",
		Object:       "橘猫",
		Confidence:   0.75,
		SubjectType:  "Person",
		ObjectType:   "Animal",
		SentenceText: "张三养了一只橘猫。",
	}})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := g.Recall([]string{"张三"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	var subjType, objType string
	for _, e := range res.Entities {
		switch e.Name {
		case "张三":
			subjType = e.Type
		case "橘猫":
			objType = e.Type
		}
	}
	if subjType != "Person" || objType != "Animal" {
		t.Errorf("实体类型 = (%q,%q)，期望 (Person,Animal)", subjType, objType)
	}
}

// 插件只给块 ID 前缀，句子由内核合成；块必须挂到该句子（sentence --contains--> block）。
func TestGraphCommit_BindsMediaFromDigests(t *testing.T) {
	g, _, _ := newTestStores(t)
	payload := []byte("png-bytes")
	b := mediaBlock("image/png", payload)

	// 先把媒体块落库（字节随块同生灭）。
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}

	m := NewGraphMemoryWithPlugin("tester", g)
	if err := m.Commit([]Triple{{
		Subject:      "配色图",
		Relation:     "包含",
		Object:       "三色带",
		SentenceText: "这张图是紫蓝红三色带。",
		MediaDigests: []string{b.ID[:12]}, // 插件手里通常只有短前缀
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := g.Recall([]string{"配色图"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(res.Relations) == 0 || res.Relations[0].SentenceText == "" {
		t.Fatalf("没有原句落点 —— 媒体块无从挂接（relations=%+v）", res.Relations)
	}
	sid := res.Relations[0].SentenceText

	// 句子文本保持原样：不再往正文里贴媒体标记。
	if strings.Contains(res.Relations[0].SentenceText, b.ID[:12]) {
		t.Errorf("句子文本不该被媒体标记污染: %q", res.Relations[0].SentenceText)
	}

	blocks, err := g.BlocksForNode("block", memory.SentenceBlockID(sid))
	if err != nil {
		t.Fatalf("BlocksForNode: %v", err)
	}
	if len(blocks) != 1 || blocks[0].ID != b.ID {
		t.Errorf("句子 %q 的媒体块 = %+v，期望 [%s]", sid, blocks, b.ID)
	}
	// 召回路径不带负载（性能纪律）；字节按块 ID 单取 —— 块即 union。
	got, err := g.BlockData(b.ID)
	if err != nil {
		t.Fatalf("BlockData: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("块字节 = %q，期望 %q", got, payload)
	}
}

// 同一个媒体块 ID 在同一三元组里重复出现（短/完整混写）时只挂一个块。
func TestGraphCommit_DedupesRepeatedDigest(t *testing.T) {
	g, _, _ := newTestStores(t)
	b := mediaBlock("image/png", []byte("dup-bytes"))
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}

	m := NewGraphMemoryWithPlugin("tester", g)
	if err := m.Commit([]Triple{{
		Subject:      "重复图",
		Relation:     "标记",
		Object:       "一次",
		SentenceText: "同一张图说了两遍。",
		MediaDigests: []string{b.ID[:12], b.ID},
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := g.Recall([]string{"重复图"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	blocks, err := g.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("同一内容应只产生 1 个块，实际 %d: %+v", len(blocks), blocks)
	}
}

// 块身份是内容派生：同内容必然同块 ID，不会因入口不同而重复。
func TestMediaBlockID_Deterministic(t *testing.T) {
	a := memory.MediaBlockID("image/png", []byte("same-bytes"))
	b := memory.MediaBlockID("image/png", []byte("same-bytes"))
	if a != b {
		t.Errorf("同内容应得同块 ID: %q vs %q", a, b)
	}
	c := memory.MediaBlockID("image/png", []byte("other-bytes"))
	if a == c {
		t.Errorf("不同内容应得不同块 ID: %q vs %q", a, c)
	}
}

// 引用了不存在的块时如实记日志跳过，不静默造空块。
func TestGraphCommit_UnknownBlockRefSkipped(t *testing.T) {
	g, _, _ := newTestStores(t)
	m := NewGraphMemoryWithPlugin("tester", g)

	if err := m.Commit([]Triple{{
		Subject:      "未知块",
		Relation:     "引用",
		Object:       "不存在的块",
		SentenceText: "引用了不存在块。",
		MediaDigests: []string{"deadbeefcafe"},
	}}); err != nil {
		t.Fatalf("引用不存在块时 Commit 不该失败: %v", err)
	}
	res, _ := g.Recall([]string{"未知块"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	blocks, err := g.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("引用不存在块时不该造块，实际 %+v", blocks)
	}
}

// Recall 必须把置信度带回插件：拿不到它，插件只能把所有召回结果等同看待。
func TestGraphRecall_CarriesConfidence(t *testing.T) {
	g, _, _ := newTestStores(t)
	m := NewGraphMemoryWithPlugin("tester", g)

	// 实体名至少两个字符：validEntityName 会静默跳过单字实体，
	// 那样 Commit 返回 nil 但什么都没写，测试会退化成假阳性。
	if err := m.Commit([]Triple{{
		Subject: "甲方", Relation: "疑似", Object: "乙方", Confidence: 0.3,
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	_, rels, err := m.Recall([]string{"甲方"}, 2)
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(rels) == 0 {
		t.Fatal("召回为空")
	}
	if rels[0].Confidence != 0.3 {
		t.Errorf("Confidence = %v，期望 0.3", rels[0].Confidence)
	}
}

// ---------- 文档记忆（知识库） ----------

// 附件带 Data → 字节直接进块，成为文档直接持有的一等块。
func TestDocInsertWithMedia_StoresAndBinds(t *testing.T) {
	_, ds, _ := newTestStores(t)
	dm := NewDocMemoryWithPlugin("tester", ds)

	d := &Doc{Title: "带图笔记", Content: "这是正文。"}
	err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png",
		Data: []byte("attachment-bytes"),
		Name: "chart.png",
	}})
	if err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}
	if d.ID == "" {
		t.Fatal("ID 未回填 —— 插件拿不到刚写入文档的 id")
	}

	// 正文保持原样：不再往 Content 里拼任何媒体标记。
	if strings.Contains(d.Content, "image/png") {
		t.Errorf("正文不该被媒体标记污染: %q", d.Content)
	}

	blocks := ds.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("文档记忆块 = %+v，期望 1 条", blocks)
	}
	// 字节就在块里，直接读回。
	if string(blocks[0].Data) != "attachment-bytes" {
		t.Errorf("块字节 = %q，期望 attachment-bytes", blocks[0].Data)
	}
	if blocks[0].MIME != "image/png" {
		t.Errorf("块 MIME = %q，期望 image/png", blocks[0].MIME)
	}
}

// 同一个附件字节出现两次 → 只产生一个块（内容去重）。
func TestDocInsertWithMedia_DedupesIdenticalBytes(t *testing.T) {
	_, ds, _ := newTestStores(t)
	dm := NewDocMemoryWithPlugin("tester", ds)

	d := &Doc{Title: "重复附件", Content: "正文"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{
		{MIME: "image/png", Data: []byte("existing")},
		{MIME: "image/png", Data: []byte("existing")},
	}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}
	if blocks := ds.Blocks(); len(blocks) != 1 {
		t.Errorf("相同字节应只产生 1 个块，实际 %d", len(blocks))
	}
}

// Query 必须回媒体元数据但**不回字节**：一次检索可能命中几十份媒体，
// 全塞回插件会把跨进程消息撑爆。
func TestDocQuery_FillsMediaMetadataWithoutBytes(t *testing.T) {
	_, ds, _ := newTestStores(t)
	dm := NewDocMemoryWithPlugin("tester", ds)

	d := &Doc{Title: "紫蓝红三色带", Content: "配色说明"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("query-bytes"),
	}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}

	got := dm.Query("紫蓝红三色带 配色说明", 3)
	if len(got) == 0 {
		t.Fatal("检索不到刚写入的文档")
	}
	var hit *Doc
	for _, g := range got {
		if g.ID == d.ID {
			hit = g
		}
	}
	if hit == nil {
		t.Fatalf("检索结果里没有目标文档: %+v", got)
	}
	if len(hit.MediaDigests) != 1 {
		t.Errorf("MediaDigests = %v，期望 1 条", hit.MediaDigests)
	}
	if len(hit.Attachments) != 1 {
		t.Fatalf("Attachments = %v，期望 1 条", hit.Attachments)
	}
	att := hit.Attachments[0]
	if att.MIME != "image/png" || att.Digest != hit.MediaDigests[0] {
		t.Errorf("附件元数据 = %+v，期望 mime=image/png 且 digest 与 MediaDigests 一致", att)
	}
	if len(att.Data) != 0 {
		t.Errorf("Attachments 不该带字节（%d 字节）—— 需要时按块 ID 单取", len(att.Data))
	}
}

// 文档被删除时它持有的一等记忆块随之消失，块里的字节也随之消失。
func TestDocRemove_DropsBlocks(t *testing.T) {
	_, ds, _ := newTestStores(t)
	dm := NewDocMemoryWithPlugin("tester", ds)

	d := &Doc{Title: "待删除", Content: "正文"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("to-be-freed"),
	}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}
	if blocks := ds.Blocks(); len(blocks) != 1 {
		t.Fatalf("前置条件不成立，块 = %+v", blocks)
	}

	dm.Remove(d.ID)

	if blocks := ds.Blocks(); len(blocks) != 0 {
		t.Errorf("删除文档后仍持有 %+v", blocks)
	}
}

// ---------- 文本记忆 ----------

// 文本记忆是追加写 JSONL 的字符串日志，没有块容器。
// 它不会愄造文本标记来承载媒体：附件被明确忽略并记录日志，
// 需要保存媒体请用文档/图记忆。
func TestTextMemory_AttachmentsIgnoredNotFaked(t *testing.T) {
	_, _, tm := newTestStores(t)
	m := NewTextMemoryWithPlugin("tester", tm)

	if err := m.Append(TextEvent{
		Role:    "user",
		Content: "看这张图",
		Attachments: []MediaAttachment{{
			MIME: "image/png", Data: []byte("text-mem-bytes"),
		}},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := m.RecentEvents(5)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("读不到刚追加的事件")
	}
	last := got[len(got)-1]
	if last.Content != "看这张图" {
		t.Errorf("正文应保持原样，实际 %q", last.Content)
	}
	if len(last.Attachments) != 0 {
		t.Errorf("文本层不该凭空造出附件（没有块存储可挂）: %+v", last.Attachments)
	}
}

// ---------- MediaBytes：CAS 淘汰后插件取媒体的唯一路径 ----------

// 判据（不变量，不是"函数被调过"）：
//  1. 按完整块 ID 取回字节与 MIME，逐字节一致
//  2. 按唯一前缀也能取回（跨进程/跨层只带短引用是常态）
//  3. 引用不存在 → ok=false 而非 error（"没这个媒体"是调用方能处理的常态）
//  4. 过短前缀（歧义风险）→ ok=false；宁可不解析也不挂错媒体
//  5. 文本块（无负载）→ ok=false，不返回空切片冒充"有字节"
func TestMemoryBytes_ByBlockRef(t *testing.T) {
	g, _, _ := newTestStores(t)
	payload := []byte("PNGDATA-0123456789")
	b := mediaBlock("image/png", payload)
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}
	m := NewGraphMemoryWithPlugin("test", g)

	// 1. 完整块 ID
	data, mime, ok, err := m.MediaBytes(b.ID)
	if err != nil {
		t.Fatalf("MediaBytes(全 ID): %v", err)
	}
	if !ok {
		t.Fatal("完整块 ID 应能取回字节")
	}
	if string(data) != string(payload) {
		t.Errorf("字节 = %q，期望 %q", data, payload)
	}
	if mime != "image/png" {
		t.Errorf("MIME = %q，期望 image/png", mime)
	}

	// 2. 唯一前缀（blk_media_ + 前 4 位 hex 已 ≥12 字符）
	prefix := b.ID[:len("blk_media_")+4]
	data2, _, ok2, err := m.MediaBytes(prefix)
	if err != nil || !ok2 {
		t.Fatalf("唯一前缀应能取回字节: ok=%v err=%v", ok2, err)
	}
	if string(data2) != string(payload) {
		t.Errorf("前缀取回字节不一致: %q", data2)
	}

	// 3. 不存在的引用 → ok=false, err=nil
	if _, _, ok3, err := m.MediaBytes("blk_media_deadbeef0000"); err != nil || ok3 {
		t.Errorf("不存在的引用应 ok=false err=nil，实为 ok=%v err=%v", ok3, err)
	}

	// 4. 过短前缀 → ok=false
	if _, _, ok4, err := m.MediaBytes("blk_"); err != nil || ok4 {
		t.Errorf("过短前缀应不解析，实为 ok=%v err=%v", ok4, err)
	}

	// 5. 文本块（无负载）→ ok=false
	textBlock := memory.MemoryBlock{ID: "blk_src_abc123def456", Modality: memory.BlockText, Text: "纯文本"}
	if err := g.PutMemoryBlocks([]memory.MemoryBlock{textBlock}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok5, err := m.MediaBytes(textBlock.ID); err != nil || ok5 {
		t.Errorf("无负载的块应 ok=false，实为 ok=%v err=%v", ok5, err)
	}

	// 6. 空引用 → ok=false，不 panic
	if _, _, ok6, _ := m.MediaBytes("  "); ok6 {
		t.Error("空引用应 ok=false")
	}
}
