package document

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/internal/tfidf"
)

// ChannelCleaner 按事件来源查找输入通道的 Cleaner 函数。
// 返回 nil 表示不使用额外清洗。
type ChannelCleaner func(source string) func(string) string

// Doc — 记忆文档：由上下文提炼而来
//
// ## L1/L2 同构
//
// 文档的主体是 **Events**（事件列表），与 RelevanceContext 的事件列表同一结构。
// 区别不在形态，而在是否被直接投喂给模型：L1 进 system prompt 时序，L2 要经召回才进去。
//
// Content 只是**渲染视图**（供摘要、人工阅读、SDK 的 Doc.Content 字段），
// 不是原文载体。原文在 Events 里，正文内联在文本块里。
type Doc struct {
	ID      string     `json:"id"`
	Summary string     `json:"summary"`
	Content string     `json:"content"` // 渲染视图，不是原文
	Events  []DocEvent `json:"events,omitempty"`

	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Source      string            `json:"source"`
	Meta        map[string]string `json:"meta,omitempty"`
	AccessCount int               `json:"access_count"`
	LastAccess  time.Time         `json:"last_access"`

	// Blocks 是本文档持有的**全部**块（事件里的文本块 + 媒体块去重后的并集）。
	//
	// ★ 为什么还留一个扁平列表而不是只看 Events：
	//   · 向量检索／跨模态融合要能不遍历事件就拿到所有块；
	//   · L2→L3 蒸馏按块迁移；
	//   · 存量文档（无 Events）只有这一个字段有块。
	//
	// ★ 它是**视图**，不是事实源：块的真实归属在 Events[i].Blocks 里。
	//   两者由 ContextToDoc 同一处产出，不得分别修改。
	Blocks []memory.MemoryBlock `json:"blocks,omitempty"`

	Vector   tfidf.Vector `json:"vector,omitempty"`    // TF-IDF 稀疏向量（fallback 时持久化）
	DenseVec []float64    `json:"dense_vec,omitempty"` // 多模态稠密向量（主路径）
	DenseFP  string       `json:"dense_fp,omitempty"`  // DenseVec 所属统一空间指纹，变化时触发重算
}

// DocEvent 是一条被 L2 持有的**原始事件**，与 L1 的 ContextEvent 同构。
//
// ★ 为何正文不在字段里、而在块里
//
// 正文（含多媒体正文）内联在块里：文本块的 Text 是正文，媒体块的 Data 是字节。
// 事件只是块的容器。因此本结构**不重复存一份正文字符串** ——
// 那会出现「块里的正文」与「字段里的正文」两份真相。
//
// 读原文 = 读 Blocks 里文本块的 Text。
type DocEvent struct {
	Timestamp   time.Time        `json:"ts"`
	Source      string           `json:"source"`
	ToolsUsed   []string         `json:"tools_used,omitempty"`
	ToolResults []ToolResultItem `json:"tool_results,omitempty"`

	// Blocks 是本事件持有的块，**正文就在里面**。
	//
	// 顺序约定（复原时按此顺序回填 ContextEvent）：
	//   Input 文本块 → Response 文本块 → 媒体块…
	// 由 docBlocksForEntry 保证；回填由 blocksToInOut 对称解读。
	Blocks []memory.MemoryBlock `json:"blocks,omitempty"`
}

// HasEvents 报告这篇文档是否保存了可逐条回填的原始事件。
//
// 存量文档（Events 引入之前归档的）没有它 —— 调用方必须据此降级到
// 「把 Content 塞进一条事件」，而不是假装能回填。
func (d *Doc) HasEvents() bool { return d != nil && len(d.Events) > 0 }

// docVectorAlgoVersion 是**文档向量算法**的版本标记。
//
// 为什么必需：DenseFP 只记录"哪个向量空间（模型）"，不记录"用什么文本算的"。
// 2026-10-22 把输入从 `Summary + Content` 改成"事件正文"后，指纹不变
// ⇒ 存量文档全部命中 `DenseFP == 指纹` 的跳过条件 ⇒ **永不重算**，
// 修复对已有数据完全无效（实测：1055 篇存量仍是旧分数 0.78/0.79/0.76，
// 区分度 0.003，与新算法的判据结果不符）。
//
// 所以把算法版本编进 DenseFP：算法一变，指纹后缀变，存量自动重算一次。
//
// 变更记录：
//
//	v1  初版：VectorizeDense(Summary + Content)
//	v2  Summary 是 jieba 词袋且全库同质，改为事件正文（见 denseFor 注释）
const docVectorAlgoVersion = "v2"

// Store — 文档记忆存储。
//
// 主路径：denseSpace（稠密向量，与媒体共享的统一空间）。
// Fallback：tfidf（TF-IDF 倒排索引，仅稠密空间不可用时加载）。
//
// ★ 关于 fallback 的定位（不要误删）：
//
//	它是「稠密空间没加载」时的兜底，**不是**「词袋当语义」的第二套真相。
//	设计要求（ARCHITECTURE「向量层级：统一多模态空间（主）→ 词嵌入 → TF-IDF（回退）」）
//	明确保留这条降级链：模型不可用时服务不能整个失去检索能力。
//
//	★ 但它**绝不能**是主路径：一旦稠密空间可用，jieba 词袋就不该参与
//	  （实测主题不同的文档两两余弦 0.85~0.95，检索等同失效 —— 见 denseFor 注释）。
type Store struct {
	dir   string
	mu    sync.RWMutex
	docs  map[string]*Doc
	dirty bool

	// fallback 路径（仅稠密空间不可用时加载）
	tfidfEmb   *tfidf.Embedder
	tfidfIdx   *tfidf.SearchableIndex
	trainTexts []string // 缓存训练文本，延迟训练
	tfidfOnce  sync.Once

	// 主路径
	denseSpace vector.MultimodalEmbedder
}

// NewStore 创建文档存储。tokenizer 由外层注入（如 jieba），供 TF-IDF 回退路径使用
// （仅稠密空间不可用时加载；主路径不碰它）。
func NewStore(dir string, tokenizer tfidf.Tokenizer) *Store {
	return &Store{
		dir:  dir,
		docs: make(map[string]*Doc),
		// tfidf 延迟初始化：只在需要 fallback 时创建
		tfidfEmb: tfidf.NewEmbedder(tokenizer, 4096),
	}
}

// ensureTFIDF 延迟初始化 TF-IDF 索引（仅 fallback 路径）。
// 调用方已持有 s.mu。
func (s *Store) ensureTFIDF() {
	s.tfidfOnce.Do(func() {
		s.tfidfIdx = tfidf.NewSearchableIndex(s.tfidfEmb)
		// 延迟训练：用缓存的文本建立索引
		texts := make(map[string]string, len(s.trainTexts)/2)
		for i := 0; i+1 < len(s.trainTexts); i += 2 {
			texts[s.trainTexts[i]] = s.trainTexts[i+1]
		}
		s.tfidfIdx.Train(texts)
		s.trainTexts = nil // 释放缓存
		s.tfidfEmb.Train(func() []string {
			out := make([]string, 0, len(texts))
			for _, t := range texts {
				out = append(out, t)
			}
			return out
		}())
		log.Printf("[document memory] tfidf fallback loaded: %d docs", len(texts))
	})
}

func (s *Store) Start() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return fmt.Errorf("document store dir: %w", err)
	}
	if err := s.loadAll(); err != nil {
		log.Printf("[document memory] load error: %v", err)
	}
	log.Printf("[document memory] started with %d docs", len(s.docs))
	return nil
}

func (s *Store) Stop() { s.flush() }

// SetDenseSpace 设置稠密多模态向量空间（主路径）。
func (s *Store) SetDenseSpace(ds vector.MultimodalEmbedder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denseSpace = ds
}

// BuildDenseIndex 为所有文档计算稠密向量（文本 ⊕ 媒体块）。
func (s *Store) BuildDenseIndex(ds vector.MultimodalEmbedder) {
	if ds == nil || !ds.Loaded() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("[document memory] building dense index for %d docs (dim=%d)", len(s.docs), ds.Dim())
	count := 0
	for _, doc := range s.docs {
		if doc.DenseVec != nil && len(doc.DenseVec) == ds.Dim() && doc.DenseFP == s.vectorFingerprint() {
			continue
		}
		vec := s.denseFor(doc)
		if vec == nil {
			continue
		}
		doc.DenseVec = vec
		doc.DenseFP = s.vectorFingerprint()
		count++
	}
	if count > 0 {
		// 迁移结果必须落盘。否则磁盘上的 DenseFP 永远对不上当前空间，
		// 判定条件永远成立：每次启动都重算同一批文档，磁盘状态永不收敛。
		// 迁移是一次性的昂贵操作（实测 200 篇约 7s），所以当场写盘，
		// 而不是只依赖关停时的 flush——被 kill -9 也不会白算。
		s.dirty = true
		s.flushLocked()
	}
	log.Printf("[document memory] dense index built: %d new vectors", count)
}

// denseFor 计算文档的稠密向量：**事件正文** ⊕ 同事件媒体块的向量。
//
// ## 为什么不能用 Summary
//
// 旧实现是 `VectorizeDense(doc.Summary + " " + doc.Content)`，而 Summary
// 由 summarizeEntries → jieba 词袋生成，同一来源的摘要**逐字同质**：
//
//	来自 1 个来源的 1 条对话 (src0) 涉及: 用户, 提问
//	来自 1 个来源的 1 条对话 (src1) 涉及: 用户, 提问
//
// 实测（真实 chineseclip，4 篇主题完全不同的短正文）两两余弦 0.85~0.95 ——
// 几乎同向；查询分数全挤在 0.70~0.78，区分度 0.08 ⇒ 检索等同失效。
//
// 这与设计直接矛盾：ARCHITECTURE 写的是「统一多模态空间（主）」，而
// Summary 属于主路径的输入，却由最末一级的词法分词产出。
//
// ## 媒体怎么办：不需要单独生成摘要
//
// 媒体块与文本块**在同一事件内**（Doc.Events[i].Blocks），事件的正文进入
// 向量即覆盖了它的语义。为媒体单独生成描述正是 CAS 时代「描述式索引」
// 的错误（描述是模型生成的二手信息），已在 v1.2.0 整体拆除，不重走。
//
// 若将来要「按图搜图」，那是视觉路（RecallBlocks 读块自己的向量）的职责，
// 已有独立通路，与本函数无关。
//
// ## 存量兼容
//
// 无 Events 的旧文档（本次改动前归档的）退回 Content —— 那是当时唯一的
// 原文载体，虽然它是渲染视图，但比 Summary 更接近内容。
func (s *Store) denseFor(doc *Doc) []float64 {
	if s.denseSpace == nil || !s.denseSpace.Loaded() {
		return nil
	}
	fp := s.denseSpace.Fingerprint()
	dim := s.denseSpace.Dim()
	var parts [][]float64

	// 文本路：用事件正文（原文），不用 Summary（jieba 词袋）。
	// 媒体块的语义由同一事件的文本覆盖，因此这里不需要为媒体单独编码。
	if text := documentTextForVector(doc); text != "" {
		if tv, err := s.denseSpace.VectorizeDense(text); err == nil && len(tv) > 0 {
			parts = append(parts, tv)
		}
	}

	// 媒体路：块自带的多模态向量（同空间同维度才参与融合）。
	for _, b := range doc.Blocks {
		// 只比指纹不够：指纹相同但**维度不同**的块会被 FuseVectors 按
		// 「最大维度」拼成错维度向量（并覆盖掉文本向量），而结果又被
		// 标上当前指纹——于是该文档在检索侧被长度守卫永久跳过，
		// 且每次启动都会重算。维度不符的块一律不参与融合。
		if len(b.Vector) == dim && b.Fingerprint == fp {
			parts = append(parts, b.Vector)
		}
	}
	return vector.FuseVectors(parts...)
}

// vectorFingerprint 返回「向量空间指纹 + 文档向量算法版本」。
//
// 只比空间指纹不够：换算法（同一模型、不同输入文本）时必须触发重算，
// 否则存量数据永远停留在旧算法的结果上（实测后果见 docVectorAlgoVersion）。
func (s *Store) vectorFingerprint() string {
	if s.denseSpace == nil {
		return ""
	}
	return s.denseSpace.Fingerprint() + "|" + docVectorAlgoVersion
}

// documentTextForVector 取文档用于向量化的文本：**正文**。
//
// 有 Events 时按事件顺序拼原文（来自块里的正文）；无 Events 的存量文档
// 退回 Content（渲染视图，但仍是原文最接近的载体）。
// 绝不含 Summary —— 那是 jieba 词袋，会把全库拉向同一方向。
func documentTextForVector(doc *Doc) string {
	if doc == nil {
		return ""
	}
	if !doc.HasEvents() {
		return strings.TrimSpace(doc.Content)
	}
	var b strings.Builder
	for _, ev := range doc.Events {
		in, resp, _ := ev.InOut()
		if in != "" {
			b.WriteString(in)
			b.WriteString("\n")
		}
		if resp != "" {
			b.WriteString(resp)
			b.WriteString("\n")
		}
		for _, tr := range ev.ToolResults {
			if tr.Output != "" {
				b.WriteString(tr.Output)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// Reindex 重建 TF-IDF 索引（fallback 路径变更时调用）。
func (s *Store) Reindex() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tfidfOnce = sync.Once{} // 重置延迟初始化
	texts := make(map[string]string, len(s.docs))
	for _, doc := range s.docs {
		texts[doc.ID] = doc.Summary + " " + doc.Content
	}
	// 缓存文本供 ensureTFIDF 延迟训练
	s.trainTexts = make([]string, 0, len(texts)*2)
	for id, t := range texts {
		s.trainTexts = append(s.trainTexts, id, t)
	}
	s.ensureTFIDF()
}

func (s *Store) Insert(doc *Doc) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if doc.ID == "" {
		doc.ID = fmt.Sprintf("doc_%d", time.Now().UnixNano())
		doc.CreatedAt = time.Now()
	}
	doc.UpdatedAt = time.Now()
	doc.LastAccess = time.Now()
	if doc.AccessCount == 0 {
		doc.AccessCount = 1
	}
	s.docs[doc.ID] = doc

	text := doc.Summary + " " + doc.Content

	// 主路径：稠密向量（文本 ⊕ 媒体块）
	if s.denseSpace != nil && s.denseSpace.Loaded() && len(doc.DenseVec) == 0 {
		doc.DenseVec = s.denseFor(doc)
		doc.DenseFP = s.vectorFingerprint()
	}

	// Fallback 路径：缓存文本，延迟训练
	if s.tfidfIdx != nil {
		s.tfidfIdx.Add(doc.ID, text)
	} else {
		s.trainTexts = append(s.trainTexts, doc.ID, text)
	}

	path := filepath.Join(s.dir, doc.ID+".json")
	data, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, data, 0644)
	s.dirty = true
	return nil
}

// ContextToDoc 将上下文对话历史提炼为文档。
func (s *Store) ContextToDoc(source string, entries []ContextEntry, _ interface{}, cleanFn func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) (*Doc, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if cleanFn == nil {
		cleanFn = func(text string) string { return text }
	}

	// ★★ 原文落位：正文进块，事件列表成为文档主体（L1/L2 同构）
	//
	// 此前这里把每条事件拍平成一行字符串，并对正文 truncate(..,100)、
	// 对工具输出 truncate(..,200) —— 原文在落库那一刻就不存在了。
	// 现在事件逐条保留，正文各自成为文本块（内联在块的 Text 里）。
	//
	// Content 只是渲染视图（见 contentFromEvents），不是原文载体。
	events := docEventsFromEntries(entries)
	content := contentFromEvents(events)
	contentHash := simpleHash(content)
	summary := summarizeEntries(entries, cleanFn, toolCleanFn, channelCleaner)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range s.docs {
		if d.Meta != nil && d.Meta["content_hash"] == contentHash {
			d.UpdatedAt = time.Now()
			d.LastAccess = time.Now()
			d.Content = content
			d.Events = events
			d.Source = source
			d.Summary = summary
			d.Blocks = flattenEventBlocks(events)
			d.DenseVec = s.denseFor(d)
			if s.denseSpace != nil {
				d.DenseFP = s.vectorFingerprint()
			}
			s.dirty = true
			return d, nil
		}
	}

	id := fmt.Sprintf("doc_%d", time.Now().UnixNano())
	meta := map[string]string{"content_hash": contentHash}
	if source == "context_archived" {
		meta["is_archived_context"] = "true"
	}
	doc := &Doc{
		ID: id, Summary: summary, Content: content,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LastAccess: time.Now(), AccessCount: 1, Source: source, Meta: meta,
		Events: events,
		Blocks: flattenEventBlocks(events),
	}
	s.docs[id] = doc
	doc.DenseVec = s.denseFor(doc)
	if s.denseSpace != nil {
		doc.DenseFP = s.vectorFingerprint()
	}
	text := summary + " " + content
	if s.tfidfIdx != nil {
		s.tfidfIdx.Add(id, text)
	} else {
		s.trainTexts = append(s.trainTexts, id, text)
	}

	path := filepath.Join(s.dir, id+".json")
	data, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(path, data, 0644)
	s.dirty = true
	return doc, nil
}

// Consume 向量相似度查询并移除文档
func (s *Store) Consume(text string, topK int) []*Doc {
	s.mu.Lock()
	defer s.mu.Unlock()
	if topK <= 0 {
		topK = 5
	}

	// 主路径：稠密检索
	if s.denseSpace != nil && s.denseSpace.Loaded() {
		if qv, err := s.denseSpace.VectorizeDense(text); err == nil {
			results := s.denseSearchScored(qv, topK)
			var docs []*Doc
			for _, r := range results {
				if d, ok := s.docs[r.Doc.ID]; ok {
					s.removeDoc(r.Doc.ID)
					s.dirty = true
					docs = append(docs, d)
				}
			}
			return docs
		}
	}

	// Fallback：TF-IDF 倒排检索（延迟初始化）
	s.ensureTFIDF()
	results := s.tfidfIdx.Search(text, topK)
	var docs []*Doc
	for _, r := range results {
		if d, ok := s.docs[r.ID]; ok {
			s.removeDoc(r.ID)
			s.dirty = true
			docs = append(docs, d)
		}
	}
	return docs
}

func (s *Store) Query(text string, topK int) []*Doc {
	hits := s.QueryScored(text, topK)
	out := make([]*Doc, len(hits))
	for i, h := range hits {
		out[i] = h.Doc
	}
	return out
}

// DocHit 是一篇文档记忆的相似度候选及原始分数。
type DocHit struct {
	Doc   *Doc
	Score float64
}

func (s *Store) QueryScored(text string, topK int) []DocHit {
	if topK <= 0 {
		topK = 5
	}

	var hits []DocHit

	// 主路径：稠密检索。只读，持读锁。
	s.mu.RLock()
	if s.denseSpace != nil && s.denseSpace.Loaded() {
		if qv, err := s.denseSpace.VectorizeDense(text); err == nil {
			hits = s.denseSearchScored(qv, topK)
		}
	}
	s.mu.RUnlock()

	// Fallback（需要写锁来 ensureTFIDF）
	if hits == nil {
		s.mu.Lock()
		s.ensureTFIDF()
		results := s.tfidfIdx.Search(text, topK)
		for _, r := range results {
			if d, ok := s.docs[r.ID]; ok {
				hits = append(hits, DocHit{Doc: d, Score: r.Score})
			}
		}
		s.mu.Unlock()
	}

	// 访问计数是**写**：必须离开读锁后再取写锁更新。
	//
	// 此前在 RLock 下直接 `d.AccessCount++` / `d.LastAccess = time.Now()`，
	// 与 FindColdDocs / flush 的读并发构成数据竞争（-race 实测），且不置脏 ——
	// 计数只在内存里涨，优雅关停也不落盘，FindColdDocs 的「≤2 次访问」
	// 冷度判据跨重启失真。
	s.markAccess(hits)
	return hits
}

// markAccess 记录一次检索命中：累加访问计数、刷新最后访问时间并置脏等待落盘。
// 调用方不得持有 s.mu。
func (s *Store) markAccess(hits []DocHit) {
	if len(hits) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, h := range hits {
		if h.Doc == nil {
			continue
		}
		if d, ok := s.docs[h.Doc.ID]; ok {
			d.AccessCount++
			d.LastAccess = now
		}
	}
	s.dirty = true
}

func (s *Store) denseSearchScored(queryVec []float64, topK int) []DocHit {
	if len(queryVec) == 0 {
		return nil
	}
	type scored struct {
		did   string
		score float64
	}
	var results []scored
	for _, doc := range s.docs {
		if len(doc.DenseVec) != len(queryVec) {
			continue
		}
		score := denseCosine(queryVec, doc.DenseVec)
		if score > 0.01 {
			results = append(results, scored{doc.ID, score})
		}
	}
	if len(results) == 0 {
		return nil
	}
	sort.Slice(results, func(i, j int) bool { return results[i].score > results[j].score })
	if len(results) > topK {
		results = results[:topK]
	}
	out := make([]DocHit, len(results))
	for i, r := range results {
		out[i] = DocHit{Doc: s.docs[r.did], Score: r.score}
	}
	return out
}

func denseCosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idxSize := 0
	if s.tfidfIdx != nil {
		idxSize = s.tfidfIdx.Size()
	}
	return map[string]interface{}{
		"doc_count":   len(s.docs),
		"index_count": idxSize,
		"dir":         s.dir,
	}
}

func (s *Store) FindColdDocs(maxAge time.Duration, minAccess int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cutoff := time.Now().Add(-maxAge)
	var cold []*Doc
	for _, d := range s.docs {
		if d.AccessCount <= minAccess && d.LastAccess.Before(cutoff) {
			cold = append(cold, d)
		}
	}
	return cold
}

// Get 返回指定文档（不存在时为 nil）。
func (s *Store) Get(id string) *Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.docs[id]
}

// Blocks 返回全部文档持有的一等记忆块（供跨层存活判定）。
func (s *Store) Blocks() []memory.MemoryBlock {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []memory.MemoryBlock
	for _, d := range s.docs {
		out = append(out, d.Blocks...)
	}
	return out
}

func (s *Store) RecentDocs(n int) []*Doc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var list []*Doc
	for _, d := range s.docs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.After(list[j].CreatedAt) })
	if len(list) > n {
		list = list[:n]
	}
	return list
}

func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.docs[id]; ok {
		s.removeDoc(id)
		s.dirty = true
	}
}

func (s *Store) removeDoc(id string) {
	delete(s.docs, id)
	if s.tfidfIdx != nil {
		s.tfidfIdx.Remove(id)
	}
	os.Remove(filepath.Join(s.dir, id+".json"))
}

func (s *Store) loadAll() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		// 只要求 .json：Insert 接受任意 ID 并落盘为 <id>.json，若这里再按
		// doc_ 前缀过滤，传自定义 ID 的文档重启后会静默消失。
		// 空 ID 仍会被下面的校验跳过。
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var doc Doc
		if json.Unmarshal(data, &doc) != nil || doc.ID == "" {
			continue
		}
		s.docs[doc.ID] = &doc
		// 缓存文本，延迟训练（确保TFIDF在首次需要时才加载）
		s.trainTexts = append(s.trainTexts, doc.ID, doc.Summary+" "+doc.Content)
	}
	return nil
}

func (s *Store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

// flushLocked 是 flush 的核心，调用方必须已持有 s.mu。
func (s *Store) flushLocked() {
	if !s.dirty {
		return
	}
	for _, doc := range s.docs {
		data, _ := json.MarshalIndent(doc, "", "  ")
		os.WriteFile(filepath.Join(s.dir, doc.ID+".json"), data, 0644)
	}
	s.dirty = false
}

// ——— 内部工具函数（从上下文提炼文档所需）———

type ToolResultItem struct {
	Name   string `json:"name"`
	Output string `json:"output"`
}

type ContextEntry struct {
	Timestamp   time.Time
	Source      string
	Content     string
	Response    string
	ToolsUsed   []string
	ToolResults []ToolResultItem
	Blocks      []memory.MemoryBlock // 一等记忆块随事件一起迁移到文档
}

func blocksFromEntries(entries []ContextEntry) []memory.MemoryBlock {
	seen := make(map[string]bool)
	var out []memory.MemoryBlock
	for _, e := range entries {
		for i := range e.Blocks {
			b := e.Blocks[i]
			if b.ID == "" || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return out
}

// summarizeEntries 生成文档摘要 —— **纯展示用**，不参与任何向量计算。
//
// ★ 这里曾经调用 memory.ExtractKeywords（jieba Tag 分词 + 停用词），
//
//	产出形如「来自 1 个来源的 1 条对话 (agent) 涉及: map, content, 是不是」。
//	两个后果都是实测的：
//
//	1. **不是摘要，是词袋清单**：模型与人都无法从中知道这篇讲什么；
//	2. **全库同质**：同一来源的摘要逐字相同，一旦被当作向量输入
//	   （denseFor 曾用 doc.Summary），四篇主题完全不同的文档两两余弦
//	   达到 0.85~0.95，检索等同失效。
//
// 现在改为取**首条事件的正文开头**：那是真实内容，可读、可判断。
// 仍然只是展示视图 —— 原文在 Events 的文本块里，摘要是它的短引用。
func summarizeEntries(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) string {
	if len(entries) == 0 {
		return ""
	}
	// 优先取用户/agent 的实质正文（而非工具输出），取到即止。
	for _, e := range entries {
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		if cleanText != nil {
			content = cleanText(content)
		}
		if line := firstMeaningfulLine(content); line != "" {
			return truncateRunes(line, 80)
		}
		if e.Response != "" {
			if line := firstMeaningfulLine(e.Response); line != "" {
				return truncateRunes(line, 80)
			}
		}
	}
	return fmt.Sprintf("%d 条事件", len(entries))
}

// firstMeaningfulLine 取文本中第一行有实义的内容（跳过空行、纯符号行）。
func firstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 跳过纯符号/空白
		hasWord := false
		for _, r := range line {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				hasWord = true
				break
			}
		}
		if hasWord {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return ""
}

// truncateRunes 按字符（非字节）截断。
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// simpleHash 是用于「内容是否同一份」比对的快速摘要。
//
// ★ 旧实现是 O(n²) 且哈希串长度正比于内容长度：
//
//	h := fmt.Sprintf("%x", len(s))
//	for _, c := range s { h += fmt.Sprintf("%x", c) }   // 每 rune 一次 Sprintf + 字符串拼接
//
// 两个后果都是实测出来的：
//
//  1. **性能**：1.25MB 正文归档耗时 270 秒（压力测试实测）。
//     字符串 `+=` 每轮重新分配并拷贝整个累积串 ⇒ 总拷贝量 O(n²)。
//  2. **体积**：哈希串本身是内容的 2.2 倍
//     （生产实测 content 52072 字符 → content_hash 114491 字符）。
//     它存在每篇文档的 meta 里，等于把正文又存了一遍还更胖。
//
// 换成 sha256 前 16 字节：定长 32 hex、O(n) 一次遍历。
// 用途不变（仅在同一批归档里去重），不需要密码学强度之外的任何性质。
func simpleHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// ──────────────────────────────────────────────
// L1 ↔ L2 的块级互转
//
// 设计前提：正文（含多媒体正文）内联在块里，事件只是块的容器。
// 因此归档 = 把事件正文落成文本块 + 保留事件原有的媒体块；
// 召回 = 从块读回正文，按原时间戳插回 L1。
// ──────────────────────────────────────────────

// entryTextBlockID 由「事件身份 + 正文角色」派生文本块 ID。
//
// 为何不能像原句块那样只用内容派生：
//
//	SentenceBlockID(sha256(text)) 会让**两句话恰好相同**的正文合并成一个块。
//	对话里「好的。」「收到」这类短答应大量重复，合并后
//	「哪一条事件说过这句话」就不可知了 —— 而 L2 必须能按事件逐条回填。
//
// 所以 ID 掺入事件身份（时间戳纳秒 + 来源 + 角色），保证一事一块，
// 同时仍是**内容稳定**的：同一条事件重复归档得到同一个块 ID（幂等）。
func entryTextBlockID(evtTS time.Time, source, role, text string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(evtTS.UnixNano(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return "blk_txt_" + hex.EncodeToString(h.Sum(nil)[:12])
}

// TextBlockRole 标记文本块在一对事件里的角色。
//
// 正文各自一块（不是 Input+Response 合成一块），为的是两段内容能
// **分别按自己的向量被检索**：用户的提问与模型的回答语义常常不同。
const (
	TextBlockRoleInput    = "input"
	TextBlockRoleResponse = "response"
)

// docBlocksForEntry 把一条上下文事件落成「文本块 + 媒体块」的块列表。
//
// 顺序约定（回填时按此对称解读）：
//
//	Input 文本块（若有）→ Response 文本块（若有）→ 媒体块…
//
// 媒体块原样保留（迁移语义：同一块 ID、同一向量，只换持有层）。
func docBlocksForEntry(e ContextEntry) []memory.MemoryBlock {
	var out []memory.MemoryBlock
	now := e.Timestamp
	if now.IsZero() {
		now = time.Now()
	}

	// 文本块：正文内联在 Text 里，不带向量。
	//
	// 为什么不在这里算向量：L2 的检索向量由 Doc.denseFor 用整篇文档文本算一次，
	// 单条事件的文本块不参与 L2 召回（它是回填用的载体）。
	// 这与「向量只算一次」一致 —— 不在迁移路径上重算。
	if t := strings.TrimSpace(e.Content); t != "" {
		out = append(out, memory.MemoryBlock{
			ID:        entryTextBlockID(now, e.Source, TextBlockRoleInput, t),
			Modality:  memory.BlockText,
			Text:      t,
			Source:    e.Source,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	if t := strings.TrimSpace(e.Response); t != "" {
		out = append(out, memory.MemoryBlock{
			ID:        entryTextBlockID(now, e.Source, TextBlockRoleResponse, t),
			Modality:  memory.BlockText,
			Text:      t,
			Source:    e.Source,
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	out = append(out, e.Blocks...)
	return out
}

// blocksToEntry 是 docBlocksForEntry 的逆操作：从块列表读回事件的
// Input / Response 正文与它持有的块。
//
// 识别规则：文本块按 ID 前缀里的角色区分（Input 在前、Response 在后），
// 其余块（媒体/未知来源的文本块）原样作为「事件持有的块」返回。
func blocksToEntry(ev *DocEvent) (input, response string, held []memory.MemoryBlock) {
	if ev == nil {
		return "", "", nil
	}
	for i := range ev.Blocks {
		b := ev.Blocks[i]
		if b.Modality != memory.BlockText {
			held = append(held, b)
			continue
		}
		switch {
		case input == "" && isEntryTextBlock(b, ev, TextBlockRoleInput):
			input = b.Text
		case response == "" && isEntryTextBlock(b, ev, TextBlockRoleResponse):
			response = b.Text
		default:
			// 不是本事件派生出的文本块（例如蒸馏来的原句块）：当作持有块。
			held = append(held, b)
		}
	}
	return input, response, held
}

// isEntryTextBlock 判断文本块是否为本事件按该角色派生出来的那一块。
func isEntryTextBlock(b memory.MemoryBlock, ev *DocEvent, role string) bool {
	return b.ID == entryTextBlockID(ev.Timestamp, ev.Source, role, b.Text)
}

// docEventsFromEntries 把 L1 交来的一批事件转成 L2 的事件列表。
func docEventsFromEntries(entries []ContextEntry) []DocEvent {
	if len(entries) == 0 {
		return nil
	}
	out := make([]DocEvent, 0, len(entries))
	for _, e := range entries {
		ev := DocEvent{
			Timestamp: e.Timestamp,
			Source:    e.Source,
			ToolsUsed: append([]string(nil), e.ToolsUsed...),
			Blocks:    docBlocksForEntry(e),
		}
		for _, tr := range e.ToolResults {
			ev.ToolResults = append(ev.ToolResults, ToolResultItem{Name: tr.Name, Output: tr.Output})
		}
		out = append(out, ev)
	}
	return out
}

// flattenEventBlocks 取事件列表里全部块（去重后的并集），供 Doc.Blocks 使用。
//
// 它是视图：真实归属在 Events[i].Blocks 里。之所以仍要扁平化，
// 是因为向量融合、L3 蒸馏、存量兼容三条路都按「文档持有哪些块」工作。
func flattenEventBlocks(events []DocEvent) []memory.MemoryBlock {
	seen := make(map[string]bool)
	var out []memory.MemoryBlock
	for _, ev := range events {
		for i := range ev.Blocks {
			b := ev.Blocks[i]
			if b.ID == "" || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			out = append(out, b)
		}
	}
	return out
}

// contentFromEvents 用事件正文渲染出 Doc.Content。
//
// ★ 这是**视图**，不是原文：它为了让摘要/人工阅读/SDK 的 Doc.Content 有东西可看，
// 也为了让存量逻辑（依赖 Content 做摘要与 TF-IDF）继续工作。
// 原文永远以 Events[i].Blocks 里文本块的 Text 为准。
//
// 为什么视图里也**不截断**：视图可以比原文简洁，但不该主动丢掉信息——
// 真正决定给模型看多少的是渲染层（按 token 预算），不是存储层。
func contentFromEvents(events []DocEvent) string {
	var parts []string
	for _, ev := range events {
		input, response, _ := blocksToEntry(&ev)
		line := fmt.Sprintf("[%s] %s: %s", ev.Timestamp.Format("15:04"), ev.Source, input)
		if response != "" {
			line += " → " + response
		}
		for _, tr := range ev.ToolResults {
			line += fmt.Sprintf("\n  [工具] %s: %s", tr.Name, tr.Output)
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// InOut 从一条 L2 事件读回它的 Input 正文、Response 正文与持有的块。
//
// 这是 blocksToEntry 的导出形态：L1（agent/core）需要它来回填 ContextEvent，
// 而正文住在块里，所以必须由本包解读块的角色与归属。
func (ev *DocEvent) InOut() (input, response string, held []memory.MemoryBlock) {
	return blocksToEntry(ev)
}
