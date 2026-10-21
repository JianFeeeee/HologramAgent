package document

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/internal/tfidf"
)

// ChannelCleaner 按事件来源查找输入通道的 Cleaner 函数。
// 返回 nil 表示不使用额外清洗。
type ChannelCleaner func(source string) func(string) string

// Doc — 记忆文档：由上下文提炼而来
type Doc struct {
	ID          string               `json:"id"`
	Summary     string               `json:"summary"`
	Content     string               `json:"content"`
	Tags        []string             `json:"tags"`
	Entities    []string             `json:"entities"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Source      string               `json:"source"`
	Meta        map[string]string    `json:"meta,omitempty"`
	AccessCount int                  `json:"access_count"`
	LastAccess  time.Time            `json:"last_access"`
	Blocks      []memory.MemoryBlock `json:"blocks,omitempty"`    // 一等记忆块（text/image/video/audio）
	Vector      tfidf.Vector         `json:"vector,omitempty"`    // TF-IDF 稀疏向量（fallback 时持久化）
	DenseVec    []float64            `json:"dense_vec,omitempty"` // 多模态稠密向量（主路径）
	DenseFP     string               `json:"dense_fp,omitempty"`  // DenseVec 所属统一空间指纹，变化时触发重算
}

// Store — 文档记忆存储。
// 主路径：denseSpace（稠密多模态向量，与媒体共享空间）。
// Fallback：tfidf（TF-IDF 倒排索引，仅稠密空间不可用时加载）。
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

// NewStore 创建文档存储。tokenizer 由外层注入（如 jieba），核心不直接依赖分词库。
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
		if doc.DenseVec != nil && len(doc.DenseVec) == ds.Dim() && doc.DenseFP == ds.Fingerprint() {
			continue
		}
		vec := s.denseFor(doc)
		if vec == nil {
			continue
		}
		doc.DenseVec = vec
		doc.DenseFP = ds.Fingerprint()
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

// denseFor 计算文档的稠密向量：文本向量与其一等记忆块的媒体向量融合。
//
// 只有与当前统一空间同指纹的块向量才参与融合：不同模型/维度的旧向量
// 属于另一个坐标系，混进去会算出一个两边都不像的方向。
// 任意一路缺失时退化为另一路；都不可用返回 nil。
func (s *Store) denseFor(doc *Doc) []float64 {
	if s.denseSpace == nil || !s.denseSpace.Loaded() {
		return nil
	}
	fp := s.denseSpace.Fingerprint()
	dim := s.denseSpace.Dim()
	var parts [][]float64
	if tv, err := s.denseSpace.VectorizeDense(doc.Summary + " " + doc.Content); err == nil && len(tv) > 0 {
		parts = append(parts, tv)
	}
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
		doc.DenseFP = s.denseSpace.Fingerprint()
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

	var parts []string
	for _, e := range entries {
		line := fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04"), e.Source, e.Content)
		if e.Response != "" {
			line += fmt.Sprintf(" → %s", truncate(e.Response, 100))
		}
		for _, tr := range e.ToolResults {
			line += fmt.Sprintf("\n  [工具] %s: %s", tr.Name, truncate(tr.Output, 200))
		}
		parts = append(parts, line)
	}
	content := strings.Join(parts, "\n")
	contentHash := simpleHash(content)
	summary := summarizeEntries(entries, cleanFn, toolCleanFn, channelCleaner)
	tags := extractTags(entries, cleanFn, toolCleanFn, channelCleaner)
	entities := extractEntities(entries, cleanFn, toolCleanFn, channelCleaner)

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range s.docs {
		if d.Meta != nil && d.Meta["content_hash"] == contentHash {
			d.UpdatedAt = time.Now()
			d.LastAccess = time.Now()
			d.Content = content
			d.Source = source
			d.Summary = summary
			d.Tags = tags
			d.Entities = entities
			d.Blocks = blocksFromEntries(entries)
			d.DenseVec = s.denseFor(d)
			if s.denseSpace != nil {
				d.DenseFP = s.denseSpace.Fingerprint()
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
		ID: id, Summary: summary, Content: content, Tags: tags,
		Entities: entities, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LastAccess: time.Now(), AccessCount: 1, Source: source, Meta: meta,
		Blocks: blocksFromEntries(entries),
	}
	s.docs[id] = doc
	doc.DenseVec = s.denseFor(doc)
	if s.denseSpace != nil {
		doc.DenseFP = s.denseSpace.Fingerprint()
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

func summarizeEntries(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) string {
	if len(entries) == 0 {
		return ""
	}
	sources := make(map[string]int)
	var topics []string
	for _, e := range entries {
		sources[e.Source]++
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		words := memory.ExtractKeywords(cleanText(content))
		topics = append(topics, words...)
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			toolWords := memory.ExtractKeywords(out)
			topics = append(topics, toolWords...)
		}
	}
	summary := fmt.Sprintf("来自 %d 个来源的 %d 条对话", len(sources), len(entries))
	var srcList []string
	for s := range sources {
		srcList = append(srcList, s)
	}
	summary += " (" + strings.Join(srcList, ", ") + ")"
	if len(topics) > 0 {
		seen := make(map[string]bool)
		var uniq []string
		for _, t := range topics {
			if !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		if len(uniq) > 5 {
			uniq = uniq[:5]
		}
		summary += " 涉及: " + strings.Join(uniq, ", ")
	}
	return summary
}

func extractTags(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) []string {
	tagSet := make(map[string]bool)
	for _, e := range entries {
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		for _, kw := range memory.ExtractKeywords(cleanText(content)) {
			tagSet[kw] = true
		}
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			for _, kw := range memory.ExtractKeywords(out) {
				tagSet[kw] = true
			}
		}
	}
	var tags []string
	for t := range tagSet {
		if len(tags) >= 10 {
			break
		}
		tags = append(tags, t)
	}
	return tags
}

func extractEntities(entries []ContextEntry, cleanText func(string) string, toolCleanFn func(name, output string) string, channelCleaner ChannelCleaner) []string {
	var entities []string
	seen := make(map[string]bool)
	for _, e := range entries {
		content := e.Content
		if channelCleaner != nil {
			if c := channelCleaner(e.Source); c != nil {
				content = c(content)
			}
		}
		for _, kw := range memory.ExtractKeywords(cleanText(content)) {
			if len(kw) >= 2 && !seen[kw] {
				seen[kw] = true
				entities = append(entities, kw)
			}
		}
		for _, tr := range e.ToolResults {
			out := tr.Output
			if toolCleanFn != nil {
				if c := toolCleanFn(tr.Name, tr.Output); c == "" {
					continue
				} else {
					out = c
				}
			}
			for _, kw := range memory.ExtractKeywords(out) {
				if len(kw) >= 2 && !seen[kw] {
					seen[kw] = true
					entities = append(entities, kw)
				}
			}
		}
	}
	return entities
}

func truncate(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
}

func simpleHash(s string) string {
	h := fmt.Sprintf("%x", len(s))
	for _, c := range s {
		h += fmt.Sprintf("%x", c)
	}
	return h
}
