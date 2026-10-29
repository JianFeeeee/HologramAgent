package sdk

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	coldcontext "github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

// 插件侧记忆接口的实现（SDK 桥接层）。
//
// 这一层原先的缺陷是**静默裁字段**：插件把 Triple / Doc 交进来，包装层只挑
// 自己认识的几个字段转成内部结构，其余丢弃且不报错。
//
// 现在的规则：内部结构有的字段一律透传；媒体一律变成一等记忆块。
//
// ★ 2026-10-11：块自带负载（media.Store/CAS 已彻底淘汰）。
//
// 一个 MemoryBlock 要么是文本块（负载在 Text），要么是媒体块（负载在 Data）。
// 不再有块外的字节存储层，因此本层也不再需要 media.Store 句柄 ——
// 附件字节直接进块的 Data，随块同生同灭。

// sdkBlockSeq 保证块 ID 全局唯一：Graph 的 memory_blocks 以 id 为主键，
// 不同文档里序号相同的块会在 L2→L3 迁移时相互覆盖。
var sdkBlockSeq int64

func sdkNewBlockID() string {
	return fmt.Sprintf("blk_%d_%d", time.Now().UnixNano(), atomic.AddInt64(&sdkBlockSeq, 1))
}

// sdkBlockModality 按 MIME 大类推断块的模态。
func sdkBlockModality(mime string) memory.BlockModality {
	switch {
	case bytes.HasPrefix([]byte(mime), []byte("image/")):
		return memory.BlockImage
	case bytes.HasPrefix([]byte(mime), []byte("video/")):
		return memory.BlockVideo
	case bytes.HasPrefix([]byte(mime), []byte("audio/")):
		return memory.BlockAudio
	default:
		return memory.BlockText
	}
}

// sdkBlocksFromAttachments 把附件直接变成一等记忆块（字节进块的 Data）。
//
// 去重按内容字节：同一份内容被多次提交时只留一个块（块的身份仍是内容，
// 只是这个"内容身份"不再需要一层独立的寻址存储来承载）。
func sdkBlocksFromAttachments(atts []MediaAttachment, tool string) []memory.MemoryBlock {
	if len(atts) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var blocks []memory.MemoryBlock
	for _, a := range atts {
		if len(a.Data) == 0 {
			continue
		}
		key := string(a.Data)
		if seen[key] {
			continue
		}
		seen[key] = true
		mime := a.MIME
		if mime == "" {
			mime = "application/octet-stream"
		}
		blocks = append(blocks, memory.MemoryBlock{
			ID:        sdkNewBlockID(),
			Modality:  sdkBlockModality(mime),
			Data:      a.Data,
			MIME:      mime,
			Size:      int64(len(a.Data)),
			Source:    tool,
			CreatedAt: time.Now(),
		})
	}
	return blocks
}

// ---------- 图记忆 ----------

type graphMemory struct {
	db     *memory.GraphDB
	plugin string
}

func NewGraphMemory(db *memory.GraphDB) MemoryAPI { return &graphMemory{db: db} }

// NewGraphMemoryWithPlugin 创建带上报名的图记忆包装。
//
// 旧签名曾接 media.Store；块自带负载后不再需要它，故改名为 WithPlugin。
func NewGraphMemoryWithPlugin(plugin string, db *memory.GraphDB) MemoryAPI {
	return &graphMemory{db: db, plugin: plugin}
}

func (m *graphMemory) Recall(query []string, depth int) ([]Entity, []Relation, error) {
	if m.db == nil {
		return nil, nil, nil
	}
	result, err := m.db.Recall(query, nil, depth, "")
	if err != nil {
		return nil, nil, err
	}
	entities := make([]Entity, len(result.Entities))
	for i, e := range result.Entities {
		entities[i] = Entity{Name: e.Name, Type: e.Type, MentionCount: e.MentionCount}
	}
	relations := make([]Relation, len(result.Relations))
	for i, r := range result.Relations {
		relations[i] = Relation{
			SourceName:   r.SourceName,
			TargetName:   r.TargetName,
			RelationType: r.RelationType,
			Confidence:   r.Confidence,
		}
	}
	return entities, relations, nil
}

// Commit 把插件的三元组写入图库，并把结构化媒体变成 L3 一等块。
//
// 媒体通过 sentence --contains--> block 结构边挂接，不读写任何正文 marker。
// 块自带字节负载，因此需要媒体元数据（Data/MIME）而不仅是一个 digest。
func (m *graphMemory) Commit(triples []Triple) error {
	if m.db == nil {
		return nil
	}
	ts := make([]memory.Triple, 0, len(triples))
	for _, t := range triples {
		mt := memory.Triple{
			Subject:      t.Subject,
			Relation:     t.Relation,
			Object:       t.Object,
			Confidence:   t.Confidence,
			SubjectType:  t.SubjectType,
			ObjectType:   t.ObjectType,
			SentenceText: t.SentenceText,
			MediaDigests: t.MediaDigests,
		}
		ts = append(ts, mt)
	}

	sentenceIDs, _, _, err := m.db.CommitWithMedia(ts, "plugin", 0)
	if err != nil {
		return err
	}
	m.bindSentences(sentenceIDs, ts)
	return nil
}

// bindSentences 把每个三元组显式携带的媒体变成 L3 一等记忆块，
// 并以 sentence --contains--> block 结构边关联。
//
// ★ sentenceIDs 是「原句文本 → 原句**块 ID**」（原为 sentences 表行号）。
// ★ 媒体负载：Triple.MediaDigests 只带短前缀（跨进程/跨层的轻量引用），
// 这里按前缀在**本进程已建的块**里找不到字节时如实记日志，不静默造空块。
func (m *graphMemory) bindSentences(sentenceIDs map[string]string, triples []memory.Triple) {
	if m.db == nil || len(sentenceIDs) == 0 {
		return
	}
	bound := 0
	for _, t := range triples {
		if len(t.MediaDigests) == 0 {
			continue
		}
		sid := sentenceIDs[t.SentenceText]
		if sid == "" {
			continue
		}
		for _, ref := range t.MediaDigests {
			// ref 可以是完整块 ID，也可以是前缀（插件跨进程只带短引用）。
			// 媒体块 ID 是内容派生的 blk_media_<hash>，故前缀足以唯一定位。
			b, ok, err := m.db.BlockByIDPrefix(ref)
			if err != nil {
				log.Printf("[sdk media] plugin %s lookup block %s failed: %v", m.plugin, ref, err)
				continue
			}
			if !ok {
				log.Printf("[sdk media] plugin %s media ref %s has no unique match, skipping", m.plugin, ref)
				continue
			}
			if err := m.db.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
				log.Printf("[sdk media] plugin %s L3 memory block write failed: %v", m.plugin, err)
				continue
			}
			if err := m.db.AddMemoryBlockEdge("block", sid, "block", b.ID, "contains"); err != nil {
				log.Printf("[sdk media] plugin %s sentence->block edge creation failed: %v", m.plugin, err)
				continue
			}
			bound++
		}
	}
	if bound > 0 {
		log.Printf("[sdk media] plugin %s triples wrote %d L3 memory block(s)", m.plugin, bound)
	}
}

func (m *graphMemory) Introspect() (map[string]interface{}, error) {
	if m.db == nil {
		return map[string]interface{}{}, nil
	}
	return m.db.Introspect()
}

func (m *graphMemory) MergeEntities(source, target string) (int, error) {
	if m.db == nil {
		return 0, nil
	}
	return m.db.MergeEntities(source, target)
}

func (m *graphMemory) Purge(criteria map[string]string, mode string) (int, error) {
	if m.db == nil {
		return 0, nil
	}
	return m.db.Purge(criteria, mode)
}

func (m *graphMemory) GraphData() (map[string]interface{}, error) {
	if m.db == nil {
		return map[string]interface{}{}, nil
	}
	return m.db.GraphData()
}

// BlocksChangedSince 实现 MemoryAPI（星图轻量轮询）。
func (m *graphMemory) BlocksChangedSince(since time.Time, limit int) ([]MemoryBlockView, error) {
	if m.db == nil {
		return nil, nil
	}
	blocks, err := m.db.BlocksChangedSince(since, limit)
	if err != nil {
		return nil, err
	}
	out := make([]MemoryBlockView, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, MemoryBlockView{
			ID:        b.ID,
			Modality:  string(b.Modality),
			Text:      b.Text,
			HasData:   len(b.Data) > 0,
			MIME:      b.MIME,
			Source:    b.Source,
			Tool:      b.Tool,
			Scene:     b.Scene,
			CreatedAt: b.CreatedAt,
			UpdatedAt: b.UpdatedAt,
		})
	}
	return out, nil
}

// MediaBytes 实现 MemoryAPI：按块引用取回媒体字节与 MIME。
//
// ★ CAS 淘汰后插件取媒体的唯一入口。块 ID 就是媒体身份
// （内容派生 blk_media_<hash>）；ref 可为完整 ID 或唯一前缀。
// 负载不在 blockColumns 里（召回只比向量），所以先解析引用再单取 data。
func (m *graphMemory) MediaBytes(ref string) ([]byte, string, bool, error) {
	if m.db == nil || strings.TrimSpace(ref) == "" {
		return nil, "", false, nil
	}
	b, ok, err := m.db.BlockByIDPrefix(ref)
	if err != nil {
		return nil, "", false, err
	}
	if !ok {
		return nil, "", false, nil
	}
	data, err := m.db.BlockData(b.ID)
	if err != nil {
		return nil, "", false, err
	}
	if len(data) == 0 {
		return nil, "", false, nil
	}
	return data, b.MIME, true, nil
}

// ---------- 文本记忆 ----------

type textMemoryImpl struct {
	tm     *text.Memory
	plugin string
}

func NewTextMemory(tm *text.Memory) TextMemoryAPI { return &textMemoryImpl{tm: tm} }

// NewTextMemoryWithPlugin 创建带上报名的文本记忆包装。
func NewTextMemoryWithPlugin(plugin string, tm *text.Memory) TextMemoryAPI {
	return &textMemoryImpl{tm: tm, plugin: plugin}
}

// Append 追加一条文本事件。
//
// 文本记忆是追加写 JSONL 的原始日志，只有字符串字段，没有块容器；
// 因此附件在这里无法结构化存下。不假装用文本标记承载媒体——
// 需要保存媒体请用文档记忆或图记忆（它们持有一等记忆块）。
func (m *textMemoryImpl) Append(evt TextEvent) error {
	if m.tm == nil {
		return nil
	}
	if len(evt.Attachments) > 0 {
		log.Printf("[sdk media] plugin %s appended %d attachment(s) to text memory, ignored: "+
			"the text layer is a string log with no block storage; use doc/graph memory for media", m.plugin, len(evt.Attachments))
	}
	return m.tm.Append(text.Event{
		Timestamp: evt.Timestamp, Source: evt.Role, Input: evt.Content, AgentID: evt.Channel,
	})
}

func (m *textMemoryImpl) RecentEvents(n int) ([]TextEvent, error) {
	if m.tm == nil {
		return nil, nil
	}
	got, err := m.tm.RecentEvents(n)
	if err != nil {
		return nil, err
	}
	out := make([]TextEvent, len(got))
	for i, e := range got {
		out[i] = TextEvent{
			Role: e.Source, Content: e.Input, Timestamp: e.Timestamp, Channel: e.AgentID,
		}
	}
	return out, nil
}

func (m *textMemoryImpl) Stats() map[string]interface{} {
	if m.tm == nil {
		return map[string]interface{}{}
	}
	return m.tm.Stats()
}

// ---------- 文档记忆（知识库） ----------

type docMemoryImpl struct {
	ds     *coldcontext.Store
	plugin string
}

func NewDocMemory(ds *coldcontext.Store) DocMemoryAPI { return &docMemoryImpl{ds: ds} }

// NewDocMemoryWithPlugin 创建带上报名的文档记忆包装。
func NewDocMemoryWithPlugin(plugin string, ds *coldcontext.Store) DocMemoryAPI {
	return &docMemoryImpl{ds: ds, plugin: plugin}
}

// Query 检索文档，并从文档持有的一等块补齐媒体元数据。
//
// 只返回块的 MIME/ID，不返回字节：字节留在这条文档的块里，
// 需要时按块 ID 单取（避免把整批媒体读进内存）。
func (m *docMemoryImpl) Query(text string, topK int) []*Doc {
	if m.ds == nil {
		return nil
	}
	got := m.ds.Query(text, topK)
	out := make([]*Doc, len(got))
	for i, d := range got {
		out[i] = &Doc{ID: d.ID, Title: d.Summary, Content: d.Content}
		m.fillMedia(out[i], d)
	}
	return out
}

// fillMedia 填充文档的媒体字段。
//
// 唯一的来源是文档直接持有的一等记忆块：媒体不靠正文标记、
// 也不靠任何生成的描述文本。
func (m *docMemoryImpl) fillMedia(out *Doc, d *coldcontext.ColdDoc) {
	if d == nil {
		return
	}
	for _, b := range d.Blocks {
		if b.Modality == memory.BlockText && len(b.Data) == 0 {
			continue
		}
		out.MediaDigests = append(out.MediaDigests, b.ID)
		out.Attachments = append(out.Attachments, MediaAttachment{
			Digest: b.ID,
			MIME:   b.MIME,
		})
	}
}

// appendBlocks 把新的块追加到已有块之后（按内容去重）。
func appendBlocks(existing []memory.MemoryBlock, add []memory.MemoryBlock) []memory.MemoryBlock {
	seen := make(map[string]bool, len(existing))
	for _, b := range existing {
		seen[string(b.Data)] = true
	}
	for _, b := range add {
		k := string(b.Data)
		if k != "" && seen[k] {
			continue
		}
		existing = append(existing, b)
		if k != "" {
			seen[k] = true
		}
	}
	return existing
}

// Insert 写入文档。
func (m *docMemoryImpl) Insert(d *Doc) error { return m.InsertWithMedia(d, nil) }

// InsertWithMedia 写入文档并关联媒体。
//
// 媒体直接成为文档持有的一等记忆块（字节在块的 Data）。
// 不往正文写 marker——文档向量会融合这些块的媒体向量（同一统一空间），
// 图片按自己的向量被召回。
func (m *docMemoryImpl) InsertWithMedia(d *Doc, attachments []MediaAttachment) error {
	if m.ds == nil || d == nil {
		return nil
	}
	target := &coldcontext.ColdDoc{ID: d.ID, Summary: d.Title, Content: d.Content}
	if target.Source == "" {
		target.Source = "plugin:" + m.plugin
	}

	target.Blocks = appendBlocks(target.Blocks,
		sdkBlocksFromAttachments(attachments, "plugin_doc:"+m.plugin))

	if err := m.ds.Insert(target); err != nil {
		return err
	}
	d.ID = target.ID
	return nil
}

// Remove 删除文档。
//
// 块随文档一起消失，块里的字节也随之消失——无需引用计数或外部 GC：
// 字节就在块里，块没了字节就没了。
func (m *docMemoryImpl) Remove(id string) {
	if m.ds == nil {
		return
	}
	m.ds.Remove(id)
}

func (m *docMemoryImpl) Stats() map[string]interface{} {
	if m.ds == nil {
		return map[string]interface{}{}
	}
	return m.ds.Stats()
}

var _ MemoryAPI = (*graphMemory)(nil)
var _ TextMemoryAPI = (*textMemoryImpl)(nil)
var _ DocMemoryAPI = (*docMemoryImpl)(nil)
