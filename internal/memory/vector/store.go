package vector

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/JianFeeeee/HomeAgent/pkg/embedding"
)

// Vectorizer 接口：将文本转为向量
//
// 多模态嵌入新增可选的 EmbedImage：支持视觉嵌入的实现者覆写此方法，
// 不支持的（TF-IDF 等）在默认实现里返回 ErrNotSupported。
type Vectorizer interface {
	Vectorize(text string) Vector
	EmbedImage(img []byte, mime string) (Vector, error)
}

// MultimodalEmbedder 是稠密多模态编码器的接口。
//
// 与 Vectorizer（稀疏词向量，供 TF-IDF/倒排检索）刻意区分：多模态模型产出的
// 是共享稠密空间，直接用于媒体块的稠密余弦召回（块自带向量），
// **不得**塞进文档/知识层的稀疏 vector.Store（会破坏倒排剪枝与 TF-IDF 语义）。
//
// 实现不限：可以是内嵌 ONNX，也可以是外部 HTTP 向量服务——
// 内核只依赖本接口，两条路径共享同一套检索/存储基础设施。Fingerprint 是模型
// 空间标识（如模型文件指纹），作为 vec_model 持久化用于切换后重算。
type MultimodalEmbedder interface {
	VectorizeDense(text string) ([]float64, error)
	EmbedImageDense(img []byte, mime string) ([]float64, error)
	Fingerprint() string
	Dim() int
	Loaded() bool
	Close()
}

// MultimodalModality 是统一向量空间支持的输入模态。
// 现内核只消费 text/image；外部 API 路径可能扩展 audio/video，
// 通过类型断言在接口外按需扩展，不破坏现有契约。
type MultimodalModality string

const (
	ModalityText  MultimodalModality = "text"
	ModalityImage MultimodalModality = "image"
	ModalityAudio MultimodalModality = "audio"
	ModalityVideo MultimodalModality = "video"
)

// ErrNotSupported 表示 Vectorizer 不支持该原生模态；调用方不得以描述文本冒充其向量。
var ErrNotSupported = fmt.Errorf("vectorizer does not support image embedding")

// ErrModalityUnsupported 表示该模态不在本统一向量空间的原生覆盖范围内。
//
// 它与普通错误语义不同：调用方应把它当作「这条媒体本空间永远不会有向量」
// 而不是「这次失败了、下次重试」。绝不能拿另一个模型的向量顶替——那会把
// 两套坐标系混进同一空间，检索出来的相似度没有任何意义。
//
// 它是公共 provider 契约里那个哨兵值的别名，两者 errors.Is 互通：
// provider 在自己的包内返回 embedding.ErrUnsupportedModality 即可，
// 内核侧的判断无需改变。
var ErrModalityUnsupported = embedding.ErrUnsupportedModality

// 注：曾经这里还有一个可选的 VideoEmbedder 接口（用类型断言探测视频能力）。
// 已删除：那让核心为每一个新模态长出一套模型专属方法，正是“核心适配模型”的
// 坏味道。模态能力现在是数据（embedding.Info.Modalities），输入是不透明的
// Data+MIME（见 pkg/embedding）。

// Vector 是带权特征映射：feature → weight
type Vector map[string]float64

// Store 向量存储，支持近似查询
type Store struct {
	mu    sync.RWMutex
	docs  []DocVector
	dim   int
	index *InvertedIndex

	// minScore 是候选分数下限。**必须按向量空间标定**：
	// 词向量/多模态余弦通常在 0.3~0.9，而 TF-IDF 余弦只有 0.0~0.2 ——
	// 用同一个阈值会把词法路的大量有效候选静默砍掉
	// （实测：知识库自检索 MRR 0.307 → 0.193 就是这么掉的）。
	minScore float64
}

type DocVector struct {
	ID     string
	Vector Vector
	Text   string
	Meta   map[string]string
}

// DefaultMinScore 是默认候选中选阈值（沿用历史行为）。
const DefaultMinScore = 0.05

// MinScore 返回当前候选中选阈值（供接线处自证用的是哪个阈值）。
func (s *Store) MinScore() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.minScore
}

// SetMinScore 调整候选中选阈值（按向量空间标定，见 minScore 字段注释）。
func (s *Store) SetMinScore(v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.minScore = v
}

func NewStore() *Store {
	return &Store{
		index:    NewInvertedIndex(),
		minScore: DefaultMinScore,
	}
}

func (s *Store) Insert(id, text string, vec Vector, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.docs = append(s.docs, DocVector{
		ID: id, Vector: vec, Text: text, Meta: meta,
	})
	s.index.Add(id, vec)
}

func (s *Store) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]DocVector, 0, len(s.docs))
	for _, d := range s.docs {
		if d.ID != id {
			filtered = append(filtered, d)
		}
	}
	s.docs = filtered
	s.index.Remove(id)
}

func (s *Store) Search(query Vector, topK int) []DocVector {
	hits := s.SearchScored(query, topK)
	if len(hits) == 0 {
		return nil
	}
	out := make([]DocVector, len(hits))
	for i, h := range hits {
		out[i] = h.Doc
	}
	return out
}

// DocVectorHit 是一篇文档的相似度候选及其原始 cosine 分数。
// 跨模态融合需要分数做归一化；纯排序的 Search 不暴露它。
type DocVectorHit struct {
	Doc   DocVector
	Score float64
}

// SearchScored 与 Search 同语义，但返回带原始 cosine 分数的候选。
func (s *Store) SearchScored(query Vector, topK int) []DocVectorHit {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.docs) == 0 || len(query) == 0 {
		return nil
	}

	candidates := s.index.Search(query, len(s.docs))
	type scored struct {
		doc   DocVector
		score float64
	}

	var results []scored
	seen := make(map[string]bool)
	for _, id := range candidates {
		if seen[id] {
			continue
		}
		seen[id] = true
		for _, d := range s.docs {
			if d.ID == id {
				score := CosineSimilarity(query, d.Vector)
				if score > s.minScore {
					results = append(results, scored{d, score})
				}
				break
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].score > results[j].score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	out := make([]DocVectorHit, len(results))
	for i, r := range results {
		out[i] = DocVectorHit{Doc: r.doc, Score: r.score}
	}
	return out
}

func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.docs)
}

func (s *Store) All() []DocVector {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DocVector, len(s.docs))
	copy(out, s.docs)
	return out
}

// Tokenizer 将文本拆分为词级 token
type Tokenizer func(string) []string

// NGramTokenizer 创建字符 n-gram tokenizer（降级方案）
func NGramTokenizer(maxN int) Tokenizer {
	return func(text string) []string {
		return extractNGrams(text, maxN)
	}
}

// TFIDFVectorizer 使用 tokenizer + TF-IDF
type TFIDFVectorizer struct {
	mu        sync.RWMutex
	tokenizer Tokenizer
	docFreq   map[string]float64 // feature → 文档频率
	totalDocs int
}

func NewTFIDFVectorizer(tokenizer Tokenizer) *TFIDFVectorizer {
	if tokenizer == nil {
		tokenizer = NGramTokenizer(2)
	}
	return &TFIDFVectorizer{
		tokenizer: tokenizer,
		docFreq:   make(map[string]float64),
	}
}

func (v *TFIDFVectorizer) Train(docs []string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.docFreq = make(map[string]float64)
	v.totalDocs = 0

	seen := make(map[string]map[string]bool)
	for _, doc := range docs {
		v.addDocLocked(doc, seen)
	}
}

// AddDoc 把一篇新文档计入 DF 统计（增量）。
//
// 存在的理由：Train 是全量重训，而知识库的 Add 是逐条发生的。此前 Add 只把
// 文本追进一个 summaries 切片、不更新 DF，于是**新引入的词 df=0**，
// 而 Vectorize 会跳过 df<=0 的特征 —— 运行时新增的知识当场搜不到，
// 重启（重新 Train）后才恢复。这不是优化项，是功能缺陷。
//
// 注意：document 是**文档级去重**的（同一词在同篇里多次出现只记 1 次 df），
// 与 Train 里那份 seen 表的语义必须一致，否则 IDF 会随写入路径不同而漂移。
func (v *TFIDFVectorizer) AddDoc(doc string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.addDocLocked(doc, nil)
}

// RemoveDoc 把一篇文档从 DF 统计中移出（AddDoc 的逆操作）。
//
// totalDocs 可能减到 0；此后 Vectorize 会走 totalDocs < 3 的退化分支
// （直接给 tf，不乘 IDF），这是可接受的行为 —— 库里都没东西了，
// IDF 本来也无从谈起。采用**下界守卫**：减到 0 后即使 RemoveDoc 被多调
// 一次，也不会变成负数。
func (v *TFIDFVectorizer) RemoveDoc(doc string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	for f := range v.seenFeatures(doc) {
		if v.docFreq[f] > 0 {
			v.docFreq[f]--
			if v.docFreq[f] == 0 {
				// 删掉零频条目：否则 DF 表会被"曾经出现过一次"的词永久撑大，
				// 而这正是 summaries 只增不减之外的第二处泄漏。
				delete(v.docFreq, f)
			}
		}
	}
	if v.totalDocs > 0 {
		v.totalDocs--
	}
}

// addDocLocked 是 AddDoc/Train 共用的记账内核。seen 非 nil 时复用调用方的表
// （Train 的整轮去重），nil 时本函数内使用自己的表。
//
// 调用方必须已持写锁。
func (v *TFIDFVectorizer) addDocLocked(doc string, seen map[string]map[string]bool) {
	var local map[string]bool
	if seen != nil {
		if seen[doc] == nil {
			seen[doc] = make(map[string]bool)
		}
		local = seen[doc]
	} else {
		local = make(map[string]bool)
	}

	for _, f := range v.tokenizer(doc) {
		if local[f] {
			continue
		}
		local[f] = true
		v.docFreq[f]++
	}
	v.totalDocs++
}

// seenFeatures 返回一篇文档的**去重**特征集（与 addDocLocked 的口径一致）。
// 调用方必须已持写锁。
func (v *TFIDFVectorizer) seenFeatures(doc string) map[string]bool {
	out := make(map[string]bool)
	for _, f := range v.tokenizer(doc) {
		out[f] = true
	}
	return out
}

func (v *TFIDFVectorizer) Vectorize(text string) Vector {
	v.mu.RLock()
	defer v.mu.RUnlock()

	features := v.tokenizer(text)
	tf := make(map[string]float64)
	for _, f := range features {
		tf[f]++
	}
	maxTF := 0.0
	for _, c := range tf {
		if c > maxTF {
			maxTF = c
		}
	}

	vec := make(Vector)
	for f, count := range tf {
		tfNorm := count / maxTF
		if v.totalDocs < 3 {
			vec[f] = tfNorm
			continue
		}
		df := v.docFreq[f]
		if df <= 0 {
			continue
		}
		// 平滑 IDF，高频词趋近 0，低频词趋近 log(N)
		idf := math.Log(float64(v.totalDocs+1) / (df + 1))
		if idf < 0.1 {
			continue
		}
		vec[f] = tfNorm * idf
	}
	return vec
}

// extractNGrams 提取 n-gram 特征（主要用于中文）
func extractNGrams(text string, maxN int) []string {
	runes := []rune(strings.ToLower(text))
	var features []string
	seen := make(map[string]bool)

	for n := 1; n <= maxN; n++ {
		for i := 0; i <= len(runes)-n; i++ {
			gram := string(runes[i : i+n])
			gram = strings.TrimSpace(gram)
			if gram == "" {
				continue
			}
			if !seen[gram] {
				seen[gram] = true
				features = append(features, gram)
			}
		}
	}
	return features
}

func CosineSimilarity(a, b Vector) float64 {
	var dot, normA, normB float64
	for f, va := range a {
		dot += va * b[f]
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// DenseCosine 计算两个 []float64 稠密向量的余弦相似度。
// 与 CosineSimilarity（稀疏 map）数学等价，但面向稠密多模态向量。
func DenseCosine(a, b []float64) float64 {
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

// InvertedIndex 倒排索引，加速向量搜索
type InvertedIndex struct {
	mu       sync.RWMutex
	postings map[string]map[string]float64 // feature → {docID: weight}
}

func NewInvertedIndex() *InvertedIndex {
	return &InvertedIndex{
		postings: make(map[string]map[string]float64),
	}
}

func (idx *InvertedIndex) Add(docID string, vec Vector) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for feature, weight := range vec {
		if idx.postings[feature] == nil {
			idx.postings[feature] = make(map[string]float64)
		}
		idx.postings[feature][docID] = weight
	}
}

func (idx *InvertedIndex) Remove(docID string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	for feature, postings := range idx.postings {
		delete(postings, docID)
		if len(postings) == 0 {
			delete(idx.postings, feature)
		}
	}
}

func (idx *InvertedIndex) Search(query Vector, maxResults int) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	scores := make(map[string]float64)
	for feature, qw := range query {
		if postings, ok := idx.postings[feature]; ok {
			for docID, dw := range postings {
				scores[docID] += qw * dw
			}
		}
	}

	type pair struct {
		id    string
		score float64
	}
	var sorted []pair
	for id, score := range scores {
		sorted = append(sorted, pair{id, score})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].score > sorted[j].score
	})

	if len(sorted) > maxResults {
		sorted = sorted[:maxResults]
	}
	out := make([]string, len(sorted))
	for i, p := range sorted {
		out[i] = p.id
	}
	return out
}
