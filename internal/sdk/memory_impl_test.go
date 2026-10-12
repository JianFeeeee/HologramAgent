package sdk

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
	doc "github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/media"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
)

// 插件边界的媒体透传测试。
//
// 断言的都是不变量，而不是"函数被调过"：
//  1. 插件填的字段一个都不许丢（旧实现静默裁掉 Confidence/类型/SentenceText）；
//  2. 插件不必知道媒体标记格式，内核负责补；
//  3. 媒体引用挂到正确的 owner 上，删除时释放；
//  4. mediaStore 为 nil 时整条链路退化成纯文本，不 panic 不报错。

func newTestStores(t *testing.T) (*memory.GraphDB, *doc.Store, *text.Memory, *media.Store) {
	t.Helper()
	dir := t.TempDir()

	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	ds := doc.NewStore(filepath.Join(dir, "documents"), memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatalf("doc store start: %v", err)
	}
	t.Cleanup(func() { ds.Stop() })

	tm := text.New(filepath.Join(dir, "text"))
	if err := tm.Start(); err != nil {
		t.Fatalf("text memory start: %v", err)
	}
	t.Cleanup(func() { tm.Stop() })

	ms, err := media.New(filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("media.New: %v", err)
	}
	t.Cleanup(func() { ms.Close() })

	return g, ds, tm, ms
}

// putMedia 存一份媒体，返回完整 digest。
func putMedia(t *testing.T, ms *media.Store, payload string) string {
	t.Helper()
	d, err := ms.Put([]byte(payload), media.Item{MIME: "image/png"})
	if err != nil {
		t.Fatalf("media.Put: %v", err)
	}
	return d
}

// ---------- 图记忆 ----------

// 旧实现只搬 Subject/Relation/Object，其余字段静默丢弃：
// 插件标注的类型全部落成默认 Concept，置信度全成 1.0，SentenceText 直接消失。
func TestGraphCommit_CarriesAllFields(t *testing.T) {
	g, _, _, _ := newTestStores(t)
	m := NewGraphMemory("tester", g)

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
		t.Fatal("召回不到刚提交的关系")
	}
	r := res.Relations[0]
	if r.Confidence != 0.75 {
		t.Errorf("Confidence = %v，期望 0.75（插件标注的置信度被丢弃）", r.Confidence)
	}
	if r.SentenceText != "张三养了一只橘猫。" {
		t.Errorf("SentenceText = %q，期望原句（丢了它媒体就没有落点）", r.SentenceText)
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

// 插件只给 digest，句子由内核合成；块必须挂到该句子（sentence --contains--> block）。
func TestGraphCommit_BindsMediaFromDigests(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	digest := putMedia(t, ms, "png-bytes")

	m := NewGraphMemoryWithMedia("tester", g, ms)
	if err := m.Commit([]Triple{{
		Subject:      "配色图",
		Relation:     "包含",
		Object:       "三色带",
		SentenceText: "这张图是紫蓝红三色带。",
		MediaDigests: []string{digest[:12]}, // 插件手里通常只有短 digest
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, err := g.Recall([]string{"配色图"}, nil, 2, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// ★ 判据从 SentenceID 改成**原句文本非空**（2026-10-04）
	//
	//   SentenceID 是**旧 sentences 表的行号**，退场后不存在，
	//   而它是这个断言里唯一与「有没有句子落点」有关的字段 ——
	//   于是断言恒红，而底下的媒体挂接其实一直是好的。
	//
	//   真正要验证的是「关系能回溯到原句」⇒ 原句文本非空。
	//   媒体块挂接的正确性由下面 BlocksForNode 那段验。
	if len(res.Relations) == 0 || res.Relations[0].SentenceText == "" {
		t.Fatalf("没有原句落点 —— 媒体块无从挂接（relations=%+v）", res.Relations)
	}
	// ★ 媒体的挂载点从「sentences 表行号」改成「原句块 ID」。
	//
	// 变更原因：sentences 表退场后行号不存在，而 media 边要挂在
	// 「原句块 --contains--> 媒体块」上（CommitWithMedia 的返回值
	// 已随之从 map[string]int64 改为 map[string]string）。
	//
	// 端点 kind 也从 "sentence" 改成 "block"。
	sid := res.Relations[0].SentenceText

	// 句子文本保持原样：不再往正文里贴媒体标记。
	if strings.Contains(res.Relations[0].SentenceText, digest[:12]) {
		t.Errorf("句子文本不该被媒体标记污染: %q", res.Relations[0].SentenceText)
	}

	blocks, err := g.BlocksForNode("block", memory.SentenceBlockID(sid))
	if err != nil {
		t.Fatalf("BlocksForNode: %v", err)
	}
	if len(blocks) != 1 || blocks[0].PayloadDigest != digest {
		t.Errorf("句子 %q 的媒体块 = %+v，期望 [%s]", sid, blocks, digest)
	}
}

// 同一个 digest 在同一三元组里重复出现（短/完整混写）时只能建一个块。
func TestGraphCommit_DedupesRepeatedDigest(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	digest := putMedia(t, ms, "dup-bytes")

	m := NewGraphMemoryWithMedia("tester", g, ms)
	if err := m.Commit([]Triple{{
		Subject:      "重复图",
		Relation:     "标记",
		Object:       "一次",
		SentenceText: "同一张图说了两遍。",
		MediaDigests: []string{digest[:12], digest},
	}}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	res, _ := g.Recall([]string{"重复图"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	// ★ 挂载点已从「sentences 表行号」改成「原句块 ID」
	//   （CommitWithMedia 返回值随之改为 map[string]string）
	blocks, err := g.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("同一 digest 应只产生 1 个块，实际 %d: %+v", len(blocks), blocks)
	}
}

// mediaStore 为 nil 时仍要能提交（媒体是增强，不是必需品）；只是不会建块。
func TestGraphCommit_NilMediaStoreDegrades(t *testing.T) {
	g, _, _, _ := newTestStores(t)
	m := NewGraphMemory(g)

	if err := m.Commit([]Triple{{
		Subject:      "无存储",
		Relation:     "仍可",
		Object:       "提交",
		SentenceText: "无媒体存储时的句子。",
		MediaDigests: []string{"aabbccddeeff"},
	}}); err != nil {
		t.Fatalf("Commit 在无媒体存储时不该失败: %v", err)
	}

	res, _ := g.Recall([]string{"无存储"}, nil, 2, "")
	if len(res.Relations) == 0 {
		t.Fatal("召回不到关系")
	}
	// ★ 挂载点已从「sentences 表行号」改成「原句块 ID」
	//   （CommitWithMedia 返回值随之改为 map[string]string）
	blocks, err := g.BlocksForNode("block",
		memory.SentenceBlockID(res.Relations[0].SentenceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 0 {
		t.Fatalf("无媒体存储时不该建块，实际 %+v", blocks)
	}
}

// Recall 必须把置信度带回插件：拿不到它，插件只能把所有召回结果等同看待。
func TestGraphRecall_CarriesConfidence(t *testing.T) {
	g, _, _, ms := newTestStores(t)
	m := NewGraphMemoryWithMedia("tester", g, ms)

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

// 附件带 Data → 落进 CAS 并成为文档直接持有的一等块。
func TestDocInsertWithMedia_StoresAndBinds(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

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
	if len(blocks) != 1 || blocks[0].PayloadDigest == "" {
		t.Fatalf("文档记忆块 = %+v，期望 1 条", blocks)
	}
	// 内容可读，说明真的落盘了而不只是记了个 digest。
	got, err := ms.Get(blocks[0].PayloadDigest)
	if err != nil || string(got) != "attachment-bytes" {
		t.Errorf("媒体内容读回失败: %v / %q", err, got)
	}
}

// 只给 Digest 的附件是「引用已有内容」，不该报错也不该重复落盘。
func TestDocInsertWithMedia_DigestOnlyReference(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	digest := putMedia(t, ms, "existing")
	before := ms.Stats()["count"]

	dm := NewDocMemoryWithMedia("tester", ds, ms)
	d := &Doc{Title: "引用已有", Content: "正文"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{Digest: digest[:10]}}); err != nil {
		t.Fatalf("InsertWithMedia: %v", err)
	}

	if after := ms.Stats()["count"]; after != before {
		t.Errorf("媒体条数从 %v 变成 %v —— 引用已有内容不该新增", before, after)
	}
	blocks := ds.Blocks()
	if len(blocks) != 1 || blocks[0].PayloadDigest != digest {
		t.Errorf("引用 = %+v，期望 [%s]", blocks, digest)
	}
}

// Query 必须回媒体元数据但**不回字节**：一次检索可能命中几十份媒体，
// 全塞回插件会把跨进程消息撑爆。
func TestDocQuery_FillsMediaMetadataWithoutBytes(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

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
		t.Errorf("Attachments 不该带字节（%d 字节）—— 需要时按 digest 单取", len(att.Data))
	}
}

// 文档被删除时它持有的一等记忆块随之消失，媒体不再被任何记忆块持有。
func TestDocRemove_DropsBlocks(t *testing.T) {
	_, ds, _, ms := newTestStores(t)
	dm := NewDocMemoryWithMedia("tester", ds, ms)

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
		t.Errorf("删除文档后仍持有 %+v —— 媒体仍被记忆引用", blocks)
	}
}

// mediaStore 为 nil 时 Insert/Query/Remove 必须与本特性上线前完全一致。
func TestDocMemory_NilMediaStoreDegrades(t *testing.T) {
	_, ds, _, _ := newTestStores(t)
	dm := NewDocMemory(ds)

	d := &Doc{Title: "无媒体存储", Content: "正文照常写入"}
	if err := dm.InsertWithMedia(d, []MediaAttachment{{
		MIME: "image/png", Data: []byte("ignored"),
	}}); err != nil {
		t.Fatalf("无媒体存储时写入不该失败: %v", err)
	}
	if d.ID == "" {
		t.Error("ID 仍应回填")
	}
	got := dm.Query("无媒体存储 正文照常写入", 3)
	if len(got) == 0 {
		t.Fatal("检索不到文档")
	}
	if len(got[0].Attachments) != 0 {
		t.Errorf("无媒体存储时不该有附件: %+v", got[0].Attachments)
	}
	dm.Remove(d.ID) // 不该 panic
}

// ---------- 文本记忆 ----------

// 文本记忆是追加写 JSONL 的字符串日志，没有块容器。
// 它不会愄造文本标记来承载媒体：附件被明确忽略并记录日志，
// 需要保存媒体请用文档/图记忆。
func TestTextMemory_AttachmentsIgnoredNotFaked(t *testing.T) {
	_, _, tm, ms := newTestStores(t)
	m := NewTextMemoryWithMedia("tester", tm, ms)

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
