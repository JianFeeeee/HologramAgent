package knowledge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

type Knowledge struct {
	Name      string            `json:"name"`
	Content   string            `json:"content"`
	Path      string            `json:"path"`
	Category  string            `json:"category,omitempty"` // 父路径，如 "tech/go"
	Tags      []string          `json:"tags"`
	UpdatedAt time.Time         `json:"updated_at"`
	Meta      map[string]string `json:"meta,omitempty"`

	// Dense 是该条目在**多模态统一空间**（image ⊕ text 共享坐标系）里的向量。
	// nil 表示未嵌入或嵌入失败——检索侧会被长度守卫跳过，退回流沙两路。
	// 不落 content.md：它是可重算的派生数据，落盘只会多个会失效的副本。
	Dense []float64 `json:"-"`
	// DenseFP 是产生 Dense 的模型空间标识（≠ 当前 fingerprint 时视为过期）。
	DenseFP string `json:"-"`
	// Media 是本条目携带的媒体块（媒体是**一等节点**：由自己的向量参与
	// 召回，不依赖任何生成的描述文本）。与 Dense 一同内存持有。
	Media []MediaRef `json:"-"`
}

// MediaRef 是媒体在知识条目里的一等引用。Digest 是内容派生的媒体身份
// （与记忆块的 `MediaBlockID` 同构：同内容⇒同 Digest），但**字节不在别处**：
// 字节作为条目自带数据落在条目目录的 `File` 上（与 content.md 并列，随条目生灭）。
// 向量不入侧车——它随条目在内存/稠密缓存里按需算。
//
// 命名为 KnowledgeMediaRef 而非 MediaRef，是为了不与别的包的类型撞名。
type KnowledgeMediaRef struct {
	Digest string `json:"digest"`
	MIME   string `json:"mime"`
	Kind   string `json:"kind,omitempty"`
	// File 是媒体字节在**条目目录内**的相对文件名（随条目生灭）。
	//
	// ★ 2026-10-11 CAS 淘汰：媒体字节不再寄存于全局内容寻址库，而是作为
	// 条目自带数据落在 entry 目录里（与 content.md、.media.json 并列）。
	// Remove 的 os.RemoveAll 天然把它清掉，不留孤儿。
	File string `json:"file,omitempty"`
	// Data 是写入期的瞬态字节：putMediaFile → Write 之间搬运，Write 落盘后清空。
	// json:"-" 保证它不进 .media.json 侧车（侧车只记引用，不记字节）。
	Data []byte `json:"-"`
}

// KnowledgeMediaRef 的别名，照顾内部可读性。
type MediaRef = KnowledgeMediaRef

// IndexItem — 索引条目，包含向量特征和内容摘要
type IndexItem struct {
	Name    string             `json:"name"`
	Preview string             `json:"preview"` // 前 200 字摘要
	Tags    []string           `json:"tags"`
	Vector  map[string]float64 `json:"vector"` // TF-IDF 特征向量（top-N 特征）
	Size    int                `json:"size"`   // 内容总字节数
}

// TreeIndex — 树状索引节点
type TreeIndex struct {
	Name     string                `json:"name"`
	Children map[string]*TreeIndex `json:"children,omitempty"`
	Items    []IndexItem           `json:"items,omitempty"` // 此节点下的知识条目（含向量）
}

func newTreeIndex(name string) *TreeIndex {
	return &TreeIndex{Name: name, Children: make(map[string]*TreeIndex)}
}

// compressVector 压缩向量：保留 topN 个权重最高的特征
func compressVector(v vector.Vector, topN int) map[string]float64 {
	if len(v) <= topN {
		out := make(map[string]float64, len(v))
		for k, w := range v {
			out[k] = w
		}
		return out
	}
	type kv struct {
		k string
		v float64
	}
	sorted := make([]kv, 0, len(v))
	for k, w := range v {
		sorted = append(sorted, kv{k, w})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].v > sorted[j].v
	})
	if topN > len(sorted) {
		topN = len(sorted)
	}
	sorted = sorted[:topN]
	out := make(map[string]float64, topN)
	for _, kv := range sorted {
		out[kv.k] = kv.v
	}
	return out
}

type Store struct {
	root   string
	vec    *vector.Store
	veczer *vector.TFIDFVectorizer

	// lex 是**词法路**索引（TF-IDF），与 vec（稀疏语义路）同为
	// **退化路径**的组成部分。
	//
	// ★ 主路径已改为纯稠密向量检索（2026-10-08）：注入多模态空间时
	//   SearchIn 直接用 denseHits，**不碰这两路**。它们只在未注入空间
	//   的部署里生效（见 SearchIn 的退化路径注释）。
	//
	// 为何退化路径还要两路：那个组合（词向量/TF-IDF 语义 + TF-IDF 词法）
	// 在 33 条小 KB + 稀疏 fastText 上实测比单路好（MRR 0.271→0.376）。
	// 该结论**不适用于主路径** —— 换成 chineseclip 后稠密路全库排名 1/192，
	// 融合反而把它拖垮（实测 top1 6/10 → 1/10）。
	lex *vector.Store

	mu    sync.RWMutex
	items map[string]*Knowledge

	indexPath      string
	denseCachePath string
	denseDirty     bool
	// indexDirty 标记 .index.json 过期。批量导入时置位但**不立即写**，
	// 由 flushIndex 收口：实测 writeIndexLocked 是 Add 的主开销
	// （N=400 时 6.7ms/次，占单条 Add 的绝大部分）。
	indexDirty bool
	// batchDepth > 0 表示处于批量写入期（ImportDir）。此时逐条写出的
	// flushDenseLocked 只标脏不落盘，由 endBatch 收口一次 —— 否则批量导入
	// 的派生数据写入量是 O(N²)（见 import.go 的说明）。
	batchDepth int
	// denseCacheLoaded 保证缓存只尝试恢复一次；scanned 表示 items 已扫盘就绪。
	// 两个状态位缺一不可：接线（SetDenseSpace）与扫盘（Start）的先后顺序
	// 在调用方是自由的，缓存恢复必须等**两者都就绪**才可能成功，
	// 因此不能在任一单点里无条件做，只能在每次都试一下。
	denseCacheLoaded bool
	scanned          bool
	vectorizer       vector.Vectorizer // 可选：词嵌入向量化器，优先于 TF-IDF

	// dense 是多模态稠密空间（可选）。与 vectorizer 是**两层不同的东西**：
	//   vectorizer 把文本变成稀疏特征（TF-IDF/词向量），供**退化路径**使用；
	//   dense 把 text/image 投到同一个稠密坐标系，让「按图搜知识」
	//   「按文搜含图知识」成立。文档记忆（coldcontextStore）走的就是后者。
	//
	// 为何不把稠密向量塞进 s.vec：vector.Store 是稀疏倒排结构
	// （feature → {docID: weight}），稠密向量会把倒排表退化成全量特征桶，
	// 同时破坏 TF-IDF 语义（vector.MultimodalEmbedder 的注释已明言）。
	// 因此稠密路自成一等路，与另两路并列，不混进任何一个 Store。
	dense vector.MultimodalEmbedder
}

// mediaSidecarName 是每条知识目录下存放媒体引用的文件名。
//
// 为何媒体引用必须落盘：它是**作者数据**（谁给哪条知识挂了哪张图），
// 不是可重算的派生量。此前它只存在于内存，进程一重启 scanDir 重建条目时
// Media 就空了 —— 图片关联静默消失，而且因为不报错，没有任何迹象。
// 放在条目目录内（与 content.md 并列）而非全局文件：随条目一起生灭，
// Remove 的 os.RemoveAll 天然把它清掉，不会留下孤儿记录。
const mediaSidecarName = ".media.json"

// denseCacheEntry 是单条知识的稠密向量缓存。
type denseCacheEntry struct {
	Dense []float64 `json:"dense"`
	FP    string    `json:"fp"`
	Dim   int       `json:"dim"`
}

// denseCache 是稠密向量的全局缓存文件。
//
// 它是**派生数据**（可由 content.md ⊕ 媒体重算），损坏/丢失只会导致一次
// 重算，不会丢内容。因此与媒体引用分开存放、分开承担风险。
type denseCache struct {
	Fingerprint string                     `json:"fingerprint"`
	Dim         int                        `json:"dim"`
	Entries     map[string]denseCacheEntry `json:"entries"`
}

func NewStore(root string) *Store {
	return &Store{
		root:           root,
		indexPath:      filepath.Join(root, ".index.json"),
		denseCachePath: filepath.Join(root, ".dense.json"),
		vec:            vector.NewStore(),
		lex:            newLexicalStore(),
		// 词法路用**标识符感知**分词器（见 identtoken.go），不是 memory.TokenizeWords：
		// jieba 会把 `PruneOldBackups` 切成单字符，词法路对标识符因此完全失效。
		// 只改知识库这一条路，内存索引/关键词提取仍用原分词器（行为不变）。
		veczer: vector.NewTFIDFVectorizer(identifierTokenizer),
		items:  make(map[string]*Knowledge),
	}
}

// newLexicalStore 造词法路存储。阈值设为 0：TF-IDF 余弦量级只有 0.0~0.2，
// 沿用稠密路的 0.05 会把大量有效候选静默砍掉（实测 MRR 0.307→0.193）。
func newLexicalStore() *vector.Store {
	st := vector.NewStore()
	st.SetMinScore(0)
	return st
}

// SetDenseSpace 注入多模态统一向量空间（text ↔ image 共享坐标系）。
// 未注入时知识库退化为原有的稀疏两路（词向量 + TF-IDF），保持既有行为。
func (s *Store) SetDenseSpace(ds vector.MultimodalEmbedder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dense = ds
	// 接线与扫盘的先后顺序由调用方决定；这里补一次尝试，确保
	// 「先 Start 后 SetDenseSpace」（agent 的现行顺序）也能命中缓存。
	s.maybeLoadDenseCacheLocked()
}

// denseEnabled 报告稠密路是否可用（供 Stats/自证与分支判断）。
// 调用方必须已持锁。
func (s *Store) denseEnabled() bool {
	return s.dense != nil && s.dense.Loaded()
}

// denseFor 计算一条知识的稠密向量：正文文本向量 ⊕ 各媒体块向量。
//
// 只有与当前空间**同指纹且同维度**的媒体向量才参与融合。只比指纹不够：
// 指纹相同但维度不同的向量会被 FuseVectors 按最大维度拼成错维度结果，
// 而它下次又被当成"已对齐"，就永远错下去（coldcontextStore 里踩过同一个坑）。
//
// 返回 nil 表示本条目在当前空间下无向量（检索侧会跳过它）。
func (s *Store) denseFor(k *Knowledge) []float64 {
	if !s.denseEnabled() {
		return nil
	}
	dim := s.dense.Dim()
	var parts [][]float64
	if tv, err := s.dense.VectorizeDense(k.Name + " " + k.Content); err == nil && len(tv) == dim {
		parts = append(parts, tv)
	}
	dir := filepath.Dir(k.Path)
	for _, m := range k.Media {
		if v := s.mediaDense(m, dir); v != nil {
			parts = append(parts, v)
		}
	}
	return vector.FuseVectors(parts...)
}

// mediaDense 读条目目录里的媒体字节并嵌入。任何一步拿不到就返回 nil——
// 媒体缺失不应让整条知识失去文本向量。
//
// ★ 2026-10-11 CAS 淘汰：字节直接从条目目录的 File 读，不再经 mediaGet。
func (s *Store) mediaDense(m KnowledgeMediaRef, dir string) []float64 {
	if !s.denseEnabled() || m.File == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, m.File))
	if err != nil || len(data) == 0 {
		return nil
	}
	mime := m.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	v, err := s.dense.EmbedImageDense(data, mime)
	if err != nil || len(v) != s.dense.Dim() {
		// 模态不在本空间覆盖范围内（如音频）时返回的是
		// ErrModalityUnsupported：那是「永久无向量」，不是「本次失败」。
		// 两者都不重试、也不拿别的模型的向量顶替。
		return nil
	}
	return v
}

// 为一条知识算稠密向量。
//
// 融合顺序为 [文本, 媒体...]：FuseVectors 是逐维求和，对交换律不敏感，
// 顺序不影响结果；这里固定下来只为让日志/调试可复现。
func (s *Store) ReindexDense() (built, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.denseEnabled() {
		return 0, 0
	}
	fp := s.dense.Fingerprint()
	dim := s.dense.Dim()
	for _, k := range s.items {
		if len(k.Dense) == dim && k.DenseFP == fp {
			continue
		}
		v := s.denseFor(k)
		if v == nil {
			skipped++
			continue
		}
		k.Dense, k.DenseFP = v, fp
		built++
	}
	// 落盘，否则磁盘缓存永远对不上当前空间，判定条件永远成立 ——
	// 每次启动都重算同一批（coldcontextStore 的 BuildDenseIndex 踩过这个坑）。
	if built > 0 {
		s.denseDirty = true
	}
	s.flushDenseLocked()
	log.Printf("[knowledge] dense reindex complete: built=%d skipped=%d fp=%s dim=%d", built, skipped, shortFP(fp), dim)
	return built, skipped
}

// AttachMedia 给一条已有知识挂上媒体，并**当场重算**它的稠密向量。
//
// 单独抽出来的理由：媒体入库（AddWithMedia）与媒体后续到达是两条时序，
// 前者少见（大多数场景是先有知识、图片晚一点才上传）。不重算的话，
// 新挂的媒体要等下次 ReindexDense 才参与召回。
func (s *Store) AttachMedia(name string, media ...KnowledgeMediaRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, k, err := s.resolve(name)
	if err != nil {
		return err
	}
	k.Media = append(k.Media, media...)
	// 媒体引用与向量都要落盘：引用是作者数据，向量是派生缓存。
	if err := writeMediaSidecar(filepath.Dir(k.Path), k.Media); err != nil {
		return fmt.Errorf("写媒体引用失败: %w", err)
	}
	if s.denseEnabled() {
		if v := s.denseFor(k); v != nil {
			k.Dense, k.DenseFP = v, s.dense.Fingerprint()
			s.denseDirty = true
		}
	}
	s.flushDenseLocked()
	_ = id
	return nil
}

// DenseStats 报告稠密路的接线与覆盖情况，供状态页/自证使用。
func (s *Store) DenseStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]interface{}{
		"enabled": s.denseEnabled(),
	}
	if s.denseEnabled() {
		fp := s.dense.Fingerprint()
		dim := s.dense.Dim()
		ready, stale := 0, 0
		for _, k := range s.items {
			switch {
			case len(k.Dense) == dim && k.DenseFP == fp:
				ready++
			default:
				stale++
			}
		}
		out["fingerprint"] = fp
		out["dim"] = dim
		out["ready"] = ready
		out["stale"] = stale
	}
	return out
}

// SetVectorizer 设置词嵌入向量化器，优先于 TF-IDF
func (s *Store) SetVectorizer(v vector.Vectorizer) {
	s.vectorizer = v
}

// ReindexWithVectorizer 用给定的向量化器重建稀疏两路索引。
// 稠密路不在此重建：它由独立的 ReindexDense 负责（换模型只影响它）。
func (s *Store) ReindexWithVectorizer(v vector.Vectorizer) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[knowledge] reindex with vectorizer (%d items)", len(s.items))
	s.vec = vector.NewStore()
	// 词法路的 IDF 必须建在全语料上（否则 IDF 没意义）
	s.retrainLexLocked()
	for _, k := range s.items {
		text := k.Name + " " + k.Content
		s.vec.Insert(k.Name, k.Name+": "+k.Content, v.Vectorize(text), map[string]string{
			"name": k.Name, "path": k.Path,
		})
	}
	log.Printf("[knowledge] reindex complete (dense=%d lex=%d)", s.vec.Size(), s.lex.Size())
}

// lexText 是一条知识参与**词法路**（TF-IDF）统计的文本。
//
// 为何用规范名而不是原始入参名：DF 统计必须与 lex 里实际插入的文档
// **逐字一致**，否则重启后重新 Train 的 DF 与运行时增量维护的 DF 会对不上，
// IDF 悄悄漂移。取名一律以 items 里的规范名为准。
func lexText(id, content string) string { return id + " " + content }

// indexDocLocked 把一条知识登记进词法路索引并更新 IDF 统计。
//
// IDF 与索引必须**同步**维护：只插索引不更新 DF，新引入的词 df=0 会被
// Vectorize 当作未知词跳过，于是「新增的知识当场搜不到，重启后才恢复」。
// 同一个词的 DF 也不能重复计：覆盖写同名条目时先 RemoveDoc 旧文本。
// 调用方必须已持写锁。
func (s *Store) indexDocLocked(id string, k *Knowledge) {
	text := lexText(id, k.Content)
	if old, ok := s.items[id]; ok && old != nil {
		s.veczer.RemoveDoc(lexText(id, old.Content))
	}
	s.veczer.AddDoc(text)
	s.lex.Remove(id)
	s.lex.Insert(id, id+": "+k.Content, s.veczer.Vectorize(text), nil)
}

// unindexDocLocked 把一条知识从词法路索引与 IDF 统计里同时摘掉。
// 调用方必须已持写锁。
func (s *Store) unindexDocLocked(id string, k *Knowledge) {
	if k != nil {
		s.veczer.RemoveDoc(lexText(id, k.Content))
	}
	s.lex.Remove(id)
}

// retrainLexLocked 从当前 items 全量重建词法路索引与 IDF。
// 启动与全量重建时走这条（比逐条增量更简单也更一致）。
// 调用方必须已持写锁。
func (s *Store) retrainLexLocked() {
	texts := make([]string, 0, len(s.items))
	for id, k := range s.items {
		texts = append(texts, lexText(id, k.Content))
	}
	if len(texts) > 0 {
		s.veczer.Train(texts)
	} else {
		s.veczer.Train(nil)
	}
	s.lex = newLexicalStore()
	for id, k := range s.items {
		text := lexText(id, k.Content)
		s.lex.Insert(id, id+": "+k.Content, s.veczer.Vectorize(text), nil)
	}
}

// vectorize 优先使用词嵌入向量化器，不可用时回退到 TF-IDF
func (s *Store) vectorize(text string) vector.Vector {
	if s.vectorizer != nil {
		return s.vectorizer.Vectorize(text)
	}
	return s.veczer.Vectorize(text)
}

func (s *Store) Start() error {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return fmt.Errorf("knowledge root: %w", err)
	}
	if err := s.scanAll(); err != nil {
		log.Printf("[knowledge] scan error: %v", err)
	}
	// 稠密向量是派生数据：先尝试从缓存恢复，避免每次启动对每条知识
	// 重跑一次嵌入（外部 HTTP 嵌入服务下就是 N 次网络调用）。
	s.loadDenseCache()
	// 重建索引文件（启动时必写一次，之后标脏延迟到 Flush/Stop）
	if err := s.writeIndex(); err != nil {
		log.Printf("[knowledge] write index error: %v", err)
	}
	log.Printf("[knowledge] started with %d items, %d vectors", len(s.items), s.vec.Size())
	return nil
}

// Stop 落盘未持久化的派生数据（稠密向量缓存）。
// 正文与媒体引用在写入时已落盘，这里只是补上派生缓存。
func (s *Store) Stop() {
	if err := s.Flush(); err != nil {
		log.Printf("[knowledge] flush on stop: %v", err)
	}
}

// 退化路径（未注入多模态空间时）的稀疏两路权重。
//
// 正常路径已不再做融合（稠密路是唯一召回路径，见 Search 的注释），
// 这个权重只影响**没配多模态空间的部署**——那些部署仍靠稀疏两路检索。
//
// sparseSemWeight 是稀疏预算里语义路占的比例（剩下给词法路）。
// 0.5 即历史上实测最优的「语义 0.5 / 词法 0.5」。保留 var 是为了
// rankdiag_test 的 KB_DIAG_SWEEP 仍可扫描。
var sparseSemWeight = 0.5

// denseSpaceWeight 已随「稠密路成为唯一召回路径」退场（2026-10-08）：
// 既然不再融合，就不存在「稠密路占多少预算」的问题。
// 保留本注释是因为 rankdiag_test 的历史实测门槛引用过它。

// Search 检索知识库：**稠密路 + 词法路按 RRF 融合**。
//
// # 现行为（2026-10-31 改）
//
// 两路各自给候选排序，再用倒数排名融合（RRF）合并：
//
//	score(d) = 1.0/(60+rank_dense) + 0.5/(60+rank_lex)
//
// 稠密路是语义主线（权重 1.0），词法路只做**精确命名**的救助（权重 0.5）。
//
// # 为什么不回到三路融合（2026-10-08 那次为什么淘汰它）
//
// 当时是「稠密 + 稀疏语义 + 词法」三路，每路按**查询内最大值**归一化到 1.0
// 再加权相加。那个归一化是致命的：
//
//	实测（真实 KB 192 条）词法路对 `the_jet_engine` 的原始分只有 0.027，
//	归一化后变成 1.0，与稠密路的 1.0 **等权相加** —— 弱信号被抬到
//	与强信号同样的分量，噪声压倒信号。
//
// 于是改成「稠密路唯一」，top1 从 1/10 提到 6/10。
//
// # 为什么现在要加回词法路（2026-10-31）
//
// 「稠密路唯一」在**英文标识符**这类查询上留下了洞。实测（真实 196 条库）：
//
//	query=PruneOldBackups   → 目标条目从未进前五
//	query=minSceneEvidence  → 同上
//	query=strip_path        → 同上
//
// 返回的全是 154 条 cangjie 文档里反复霸榜的那几篇。原因是词法路当年被淘汰
// 时，把**分词器的缺陷**误判成了词法路的属性：jieba 对英文标识符是
// **按字符切**的（`PruneOldBackups` → `[P r u n e O l d B a c k p s]`），
// 任意两篇含英文字母的文档 token 重叠率都能到 50%。
//
// 修掉分词器（identtoken.go：标识符整词 + 分隔符部件，且不拆 camelCase）后，
// 同三条查询词法路**唯一命中**目标条目。而稠密路对「只在文中出现一次专名」
// 本就无能为力（chineseclip 的纯文本语义弱，见 multimodal-space 文档的已知代价）。
//
// 用 RRF 而不是当初那套归一化：RRF 只看**名次**、不看分数，两路量纲不可比
// 的问题自动消失，也不会把 0.02 的弱信号抬到与 1.0 等权（第 100 名只贡献
// 1/160，第 1 名贡献 1/61）。
//
// 回归判据：dense_recall_quality_test.go（带标注的 10 条查询集 + 标识符用例）。
func (s *Store) Search(query string, topK int) []*Knowledge {
	return s.SearchIn(query, "", topK)
}

// SearchIn 与 Search 同语义，但可把召回范围限定在某个分类子树内。
//
// category 为空 = 全库（等价于 Search）。非空时**前缀匹配**该分类路径：
// 查 "tech" 命中 "tech/go"、"tech/rust" 下的条目；查 "tech/go" 只命中
// 它的子孙。这样分层才真正参与召回——此前分层只是存储布局，检索是全库
// 平铺，`SearchTree`/`SearchCategories` 两个死代码想做的事没落到检索上。
//
// 为何用前缀而不是精确相等：分类是**层级**，不是标签。要么看整棵子树，
// 要么用精确路径定位到某一层；只匹配精确相等会让 "tech" 查不到
// "tech/go" 里的东西，那正是层级索引最该提供的价值。
func (s *Store) SearchIn(query, category string, topK int) []*Knowledge {
	s.mu.RLock()
	defer s.mu.RUnlock()

	category = strings.Trim(strings.TrimSpace(category), "/")
	inScope := func(id string) bool {
		if category == "" {
			return true
		}
		k, ok := s.items[id]
		if !ok {
			return false
		}
		// 条目自身的 Category 或条目全名以该前缀开头都算命中：
		// Category 是父路径，而条目全名是 category/叶名，两者都要覆盖
		// （顶层无分类的条目 Category 为空，只能靠全名判断）。
		return k.Category == category ||
			strings.HasPrefix(k.Category, category+"/") ||
			strings.HasPrefix(id, category+"/")
	}

	if topK <= 0 {
		topK = 5
	}
	if category != "" && !s.hasInScopeLocked(category) {
		return nil // 该分类下没有任何条目，省掉全库打分
	}

	// ★ 稠密路 + 词法路按 **RRF（倒数排名融合）** 合并（2026-10-31 改）。
	//
	// ## 为什么不再是「稠密路唯一」
	//
	// 2026-10-08 去掉词法路的理由是「词法路对英文标识符失效」。那个观察是对的，
	// 但归因错了 —— 失效的是**分词器**（jieba 把 `PruneOldBackups` 切成
	// `[P r u n e O l d B a c k p s]`），不是词法路本身。修掉分词器
	// （见 identtoken.go）后，同一批查询词法路能精确命中：
	//
	//	PruneOldBackups  → hologramagent_kernel_assembly_upgrade（唯一命中）
	//	minSceneEvidence → hologramagent_kernel_facts
	//	strip_path       → changelog_v1.4.0
	//
	// 而稠密路对这些**只在文中出现一次的专名**无能为力（chineseclip 纯文本语义弱），
	// 实测这三条查询的目标条目一次都进不了前五，返回的是 154 条 cangjie 文档里
	// 反复霸榜的那几篇。
	//
	// ## 为什么用 RRF 而不是 2026-10-08 淘汰的那套
	//
	// 被淘汰的是「逐路按查询内最大值归一化再加权相加」。那个做法把词法路 0.027 的
	// 原始分抬成 1.0，与稠密路的 1.0 等权 —— 是**归一化**破坏了排序，不是词法路没用。
	// RRF 只用**名次**不用分数，天然不受两路量纲差异影响，也不会放大弱信号：
	// 第 100 名贡献 1/(60+100)，与第 1 名的 1/61 差 2.6 倍，不会等权。
	//
	// 两路权重：稠密 1.0、词法 0.5 —— 语义仍是主线，词法只做精确命名的救助。
	if s.denseEnabled() {
		qv, err := s.dense.VectorizeDense(query)
		if err != nil || len(qv) == 0 {
			// 向量化失败或得到空向量（空/纯空白查询）。
			// 退回词法路：它不依赖模型，至少还能按字面命中。
			return s.searchLexOnly(query, inScope, topK)
		}
		dense := s.denseHits(qv)
		lex := s.lexHits(query)

		// 任一路为空都不影响另一路（RRF 对缺席的路径就是不加分）。
		fused := fuseRRF(dense, lex, 1.0, 0.5, rrfK)
		out := make([]*Knowledge, 0, topK)
		for _, h := range fused {
			if !inScope(h.id) {
				continue
			}
			if k, ok := s.items[h.id]; ok {
				out = append(out, k)
			}
			if len(out) >= topK {
				break
			}
		}
		return out
	}

	// ——— 退化路径：未注入多模态空间 ———
	//
	// 有意保留，不是遗漏：把 core.memory.multimodal_space.provider 留空的
	// 部署仍需可检索，否则知识库直接变成「完全不能用」——那是比排序差
	// 更严重的失败。此路径的排序质量与接入稠密路之前逐字一致
	// （见 TestNoDenseSpaceKeepsLegacyBehavior）。
	if s.vec.Size() == 0 && s.lex.Size() == 0 {
		return nil
	}

	// 各路分别打分，再按查询内最大值归一化加权融合。
	// 三个来源形状不同（稀疏路给 DocVectorHit），统一成 scoreHit 再交给
	// addPath 收口。
	scores := make(map[string]float64)
	addPath := func(hits []scoreHit, weight float64) {
		// 归一化取**作用域内**的最大值：拿全库最大值归一会让限定分类后的
		// 分数被一个范围外的条目压低，跨路相对权重随之失真。
		max := 0.0
		for _, h := range hits {
			if h.score > max && inScope(h.id) {
				max = h.score
			}
		}
		if max <= 0 {
			return // 该路对这条查询（在作用域内）没有信号，全量让给其余路
		}
		for _, h := range hits {
			if !inScope(h.id) {
				continue
			}
			scores[h.id] += weight * h.score / max
		}
	}

	// 稀疏语义路 + 词法路（仅退化路径使用）。
	denseHits := s.vec.SearchScored(s.vectorize(query), s.vec.Size())
	lexHits := s.lex.SearchScored(s.veczer.Vectorize(query), s.lex.Size())
	addPath(toHits(denseHits), sparseSemWeight)
	addPath(toHits(lexHits), 1-sparseSemWeight)

	if len(scores) == 0 {
		return nil
	}

	ids := make([]string, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j] // 分数相同时按名字定序（保证结果可重复）
	})

	var out []*Knowledge
	for _, id := range ids {
		if k, ok := s.items[id]; ok {
			out = append(out, k)
		}
		if len(out) >= topK {
			break
		}
	}
	return out
}

// KnowledgeEntryInput 是知识条目的写入参数（纯文本 / 带媒体）。
// 走 struct 而非多个位置参数：媒体与文本在 5 个方法里成对出现，
// 位置参数会让调用点难以自明（且带媒体时必填空串）。
type KnowledgeEntryInput struct {
	Name    string
	Content string
	Media   []KnowledgeMediaRef
}

// Add 写入一条纯文本知识。媒体请用 AddWithMedia。
func (s *Store) Add(name, content string) error {
	return s.AddWithMedia(name, content, nil)
}

// AddWithMedia 写入一条知识，可携带媒体块（媒体作为一等节点参与稠密召回）。
//
// 与 Add 的区别只在于媒体：稠密向量会把正文向量与各媒体向量**融合**成一个
// 向量（同一坐标系内求和后归一化），所以一条带图的知识既能被文字搜到，
// 也能被“这张图”本身搜到。
//
// 为何 EmbedImageDense 可能失败：当前模态不在本空间覆盖范围（如音频）时返回
// ErrModalityUnsupported。此时**静默跳过该媒体**、仅用文本建立向量——
// 绝不能拿另一个模型的向量顶替，那会把两套坐标系混进同一空间，相似度全无意义。
func (s *Store) AddWithMedia(name, content string, media []KnowledgeMediaRef) error {
	return s.Write(KnowledgeEntryInput{Name: name, Content: content, Media: media})
}

// Write 按输入参数写入一条知识。
func (s *Store) Write(in KnowledgeEntryInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// id 同时是三样东西：内存 map 的键、LLM 可见的知识名、盘上相对目录。
	// 三者必须逐字相同——扫盘重建（scanDir）读回的是真实目录名，若与 Add 时的
	// 键不一致，重启那一刻知识名就变了，knowledge_list / knowledge_delete 的
	// key 全部对不上，删除还会静默失败（详见 Remove）。
	id, err := normalizeName(in.Name)
	if err != nil {
		return err
	}
	category := ""
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		category = id[:idx]
	}
	dir := filepath.Join(s.root, filepath.FromSlash(id))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create knowledge dir: %w", err)
	}

	path := filepath.Join(dir, "content.md")
	if err := os.WriteFile(path, []byte(in.Content), 0644); err != nil {
		return fmt.Errorf("write knowledge: %w", err)
	}

	// 媒体字节落进条目目录（CAS 淘汰后，字节是条目自带数据）。
	// 失败只跳过该媒体并记录——正文已落盘，图片写不进顶多搜不到图。
	if err := s.writeMediaFiles(dir, in.Media); err != nil {
		log.Printf("[knowledge] media bytes write error for %s: %v", in.Name, err)
	}

	now := time.Now()
	k := &Knowledge{
		Name:      id,
		Content:   in.Content,
		Path:      path,
		Category:  category,
		Tags:      memory.ExtractKeywords(filepath.ToSlash(id) + " " + in.Content),
		UpdatedAt: now,
		Media:     in.Media,
	}
	s.items[id] = k

	// 覆盖同名条目时必须先摘掉旧向量。
	//
	// vector.Store.Insert 是**追加**语义（s.docs = append + index.Add），不按 id
	// 去重。少了这一步，更新一条知识会在向量索引里留下上一版的副本：条目数看起来
	// 是对的，只有向量数比条目数多——而检索可能因此命中已被替换掉的旧内容。
	s.vec.Remove(id)

	text := in.Name + " " + in.Content
	vec := s.vectorize(text)
	s.vec.Insert(id, in.Name+": "+in.Content, vec, map[string]string{
		"name": in.Name, "path": path,
	})
	// 词法路：重建本条索引 + 增量维护 IDF（覆盖写时先摘掉旧文本的贡献）
	s.indexDocLocked(id, k)

	// 媒体引用是作者数据，必须落盘（放条目目录内，随条目生灭）。
	if err := writeMediaSidecar(dir, in.Media); err != nil {
		// 侧车写失败不阻断知识本身：正文已落盘，媒体丢了只影响跨模态召回，
		// 且下次 AddWithMedia/AttachMedia 会补写。但要留下痕迹。
		log.Printf("[knowledge] media sidecar write error for %s: %v", in.Name, err)
	}

	// 稠密路：算完就挂上，使新写入的条目立即可被跨模态召回命中
	// （不必等下次 ReindexDense）。
	if s.denseEnabled() {
		if v := s.denseFor(k); v != nil {
			k.Dense, k.DenseFP = v, s.dense.Fingerprint()
			s.denseDirty = true
		}
	}

	s.flushDenseLocked()
	// 索引写入改为「标脏 + 延迟收口」，见 indexDirty 字段注释。
	s.indexDirty = true
	log.Printf("[knowledge] added: %s (%d bytes, %d media)", in.Name, len(in.Content), len(in.Media))
	return nil
}

// 树状检索与分类检索已于 2026-09 移除：全仓无调用方，且停留在 Search 修复
// **之前**的单路口径（直接 s.vec.Search，无词法融合、0.05 阈值）。
// 留着它们等于埋一份已知的检索质量回归；真需要按分类召回，应给 Search 加
// category 过滤参数，而不是复活这两个。

// Remove 删除一条知识。
//
// 盘上路径取自**条目自记的 Path**（Add 写入 / scanDir 扫盘时记下的事实），
// 不再用 name 重新拼一遍：拼出来的路径和真实落点只要有一个字符对不上，
// os.RemoveAll 就删空目录返 nil，工具层回报"已删除"而文件与索引条目都还在。
//
// 不存在的条目返回 ErrNotFound（webui 的 DELETE 处理器把 error 映射成 404，
// 正是这个语义）。只删不存在的条目是幂等操作，不算错误。
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, k, err := s.resolve(name)
	if err != nil {
		return err
	}

	itemDir := filepath.Dir(k.Path)
	if err := os.RemoveAll(itemDir); err != nil {
		return err
	}
	// 顺带清掉空掉的分类目录：名字去掉最后一段就是分类路径，分类下最后一条
	// 被删后目录会空留在盘上，越积越多。只往上到 s.root 为止，**绝不动 root**
	// （root 被删 = 整个知识库连同索引一起没了）。
	rootClean := filepath.Clean(s.root)
	for dir := filepath.Clean(filepath.Dir(itemDir)); strings.HasPrefix(dir, rootClean+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break // 非空或无权限，留给上层判断
		}
	}

	s.unindexDocLocked(id, k)
	delete(s.items, id)
	s.vec.Remove(id)
	s.indexDirty = true
	return nil
}

func (s *Store) Stats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"knowledge_count": len(s.items),
		"vector_count":    s.vec.Size(),
		"root":            s.root,
		"index_file":      s.indexPath,
	}
}

func (s *Store) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var names []string
	for _, k := range s.items {
		names = append(names, k.Name)
	}
	sort.Strings(names)
	return names
}

// BuildTree 从当前知识库构建树状索引（含向量特征）
func (s *Store) BuildTree() *TreeIndex {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.buildTreeLocked()
}

// buildTreeLocked 与 BuildTree 同义，但**不取锁**——供已持写锁的路径调用。
// 为什么需要：writeIndex 会走 BuildTree（RLock），而 Add/Remove 持的是写锁，
// 直接调用会死锁；此前就是因此把索引写丢进了无追踪的 goroutine 里，
// 结果是「失败只打日志」+ 与调用方（含测试的临时目录清理）竞态。
// buildTreeLocked 从当前条目重建树状索引。
//
// 向量直接取自 s.vec（稀疏语义路的既有结果），**不再逐条重算**：
// 此前每条都调一次 s.vectorize()，那是全量分词 + TF-IDF 加权，
// 而结果与 s.vec 里已经存着的向量是同一个东西。实测这是 Add 单条
// 耗时随库规模线性增长的主因（2.1ms@50 → 13.7ms@400）。
// 调用方必须已持锁。
func (s *Store) buildTreeLocked() *TreeIndex {
	// 一次 O(N) 取全量向量建表（纯内存拷贝），替代 N 次分词计算。
	vecByID := make(map[string]vector.Vector, s.vec.Size())
	for _, d := range s.vec.All() {
		vecByID[d.ID] = d.Vector
	}

	root := newTreeIndex("root")
	for _, k := range s.items {
		node := root
		if k.Category != "" {
			parts := strings.Split(k.Category, "/")
			for _, part := range parts {
				if part == "" {
					continue
				}
				if _, ok := node.Children[part]; !ok {
					node.Children[part] = newTreeIndex(part)
				}
				node = node.Children[part]
			}
		}
		// 取该条目的向量并压缩（命中不到就留空向量，不再回退去重算——
		// 那会把本函数重新拖回 O(N × 分词)）
		vec := vecByID[k.Name]
		preview := []rune(k.Content)
		previewStr := ""
		// 按**字符**截断：preview 是中文文本，字节切会产生非法 UTF-8。
		if r := []rune(string(preview)); len(r) > 200 {
			previewStr = string(r[:200]) + "..."
		} else {
			previewStr = string(preview)
		}
		item := IndexItem{
			Name:    k.Name,
			Preview: previewStr,
			Tags:    k.Tags,
			Vector:  compressVector(vec, 20),
			Size:    len(k.Content),
		}
		node.Items = append(node.Items, item)
	}
	return root
}

// writeIndex 写入 .index.json 树状索引文件（含向量和摘要）
func (s *Store) writeIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushIndexLocked()
}

// flushIndexLocked 把过期的 .index.json 写回（调用方须持写锁）。
//
// 为什么延迟：批量导入 400 条就是 400 次全量序列化（实测 6.7ms/次），
// 而该文件**目前没有任何读取方**（Start 是全量扫盘重建索引）。
// 改为标脏 + 在 Stop/Flush 时收口，导入成本降为一次写。
// 若将来真把它当缓存读回，必须先把"读取"实现补上，再考虑是否仍需延迟。
func (s *Store) flushIndexLocked() error {
	if !s.indexDirty {
		return nil
	}
	if err := s.writeIndexLocked(); err != nil {
		log.Printf("[knowledge] write index error: %v", err)
		return err
	}
	s.indexDirty = false
	return nil
}

// Flush 把待落盘的派生数据（树索引）写回。批量导入后由调用方显式调用，
// 否则要等 Stop。
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flushDenseLocked(); err != nil {
		return err
	}
	return s.flushIndexLocked()
}

// writeIndexLocked 与 writeIndex 同义但**不取锁**（调用方已持锁）。
func (s *Store) writeIndexLocked() error {
	tree := s.buildTreeLocked()
	data, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return err
	}
	// tmp + rename：直接 os.WriteFile 会在中途崩溃时留下半截 JSON。
	// 本文件目前没有任何读取方（Start 是全量扫盘重建索引），所以损坏的
	// 后果只是「导出物不可读」；但那是运气，不该依赖——何况将来若真把它
	// 当缓存读回来，半截文件会被当成有效索引。
	tmp := s.indexPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.indexPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ——— internal ———

func (s *Store) scanAll() error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// skip hidden dirs
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		s.scanDir("", entry.Name())
	}

	s.retrainLexLocked()
	for _, k := range s.items {
		text := k.Name + " " + k.Content
		s.vec.Insert(k.Name, k.Name+": "+k.Content, s.vectorize(text), map[string]string{
			"name": k.Name, "path": k.Path,
		})
	}
	s.scanned = true
	// 若接线早于扫盘（测试与部分调用方会这么做），这里补一次缓存恢复。
	s.maybeLoadDenseCacheLocked()

	return nil
}

// scanDir 递归扫描目录
// category: 父级路径（从知识库根目录算起），如 "tech/go"
// dirName: 当前目录相对路径（从知识库根目录算起）
func (s *Store) scanDir(category, dirName string) {
	dir := filepath.Join(s.root, dirName)
	contentPath := filepath.Join(dir, "content.md")
	data, err := os.ReadFile(contentPath)
	if err == nil {
		name := dirName
		content := string(data)
		now := time.Now()
		k := &Knowledge{
			Name:      name,
			Content:   content,
			Path:      contentPath,
			Category:  category,
			Tags:      memory.ExtractKeywords(dirName + " " + content),
			UpdatedAt: now,
		}
		k.Media = readMediaSidecar(dir)
		s.items[name] = k
		return
	}

	// 无 content.md => 是分类目录，递归子目录
	subEntries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, sub := range subEntries {
		if !sub.IsDir() || strings.HasPrefix(sub.Name(), ".") {
			continue
		}
		childDir := dirName + "/" + sub.Name()
		s.scanDir(dirName, childDir)
	}
}

// ErrInvalidName 表示知识名不合法：空段、`.`、`..` 或以点开头的段。
var ErrInvalidName = errors.New("knowledge: 知识名不合法")

// ErrNotFound 表示要删除/读取的知识不存在。
var ErrNotFound = errors.New("knowledge: 知识不存在")

// normalizeName 把外部传入的知识名规范成**唯一**的规范名。
//
// 规范名同时充当三样东西：内存 map 的键、LLM 可见的知识名、盘上相对目录。
// 三者必须逐字相同——扫盘重建（scanDir）读回的是真实目录名，若与 Add 时的
// 键不一致，重启那一刻知识名就变了，knowledge_list / knowledge_delete 的
// key 全部对不上，删除还会静默失败（详见 Remove）。
//
// 为何**逐段** sanitize 而非整串：sanitize 内含 TrimSpace，只作用于整串两端。
// 整串处理时 "tech/ Go /note" 得到 id="tech/_go_/note"（段内前后空格变 "_"），
// 而建目录时逐段 sanitize 得到 "tech/_go/note"（段内空格被 TrimSpace 掉）——
// 两者从**第一次落盘起**就对不上。这不是重启才产生的漂移。
func normalizeName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: 空名", ErrInvalidName)
	}
	segs := strings.Split(name, "/")
	out := make([]string, 0, len(segs))
	for _, seg := range segs {
		s := sanitize(seg)
		// 空段 / "." / ".." 会让 filepath.Join 逃出知识根（实测 Remove("..")
		// 直接删掉整个 data 目录）；以点开头的段会被 scanDir 当隐藏目录跳过，
		// 变成"内存有、盘上扫不回"的幽灵条目。
		if s == "" || s == "." || s == ".." || strings.HasPrefix(s, ".") {
			return "", fmt.Errorf("%w: %q 含有空段、点段或隐藏段 %q", ErrInvalidName, name, seg)
		}
		out = append(out, s)
	}
	return strings.Join(out, "/"), nil
}

// findByLeaf 按最后一段（叶名）找条目，返回命中的 id 与命中数（大小写敏感）。
// 保留给需要精确叶名的调用方；resolve 用的是 findByLeafFold。
func (s *Store) findByLeaf(id string) (string, int) {
	leaf := id
	if idx := strings.LastIndex(id, "/"); idx >= 0 {
		leaf = id[idx+1:]
	}
	found, n := "", 0
	for k := range s.items {
		l := k
		if idx := strings.LastIndex(k, "/"); idx >= 0 {
			l = k[idx+1:]
		}
		if l == leaf {
			found, n = k, n+1
		}
	}
	return found, n
}

func sanitize(name string) string {
	name = strings.ToLower(name)
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}

// findByLeafFold 与 findByLeaf 同义，但叶名比较大小写不敏感——
// 遗留盘上目录可能带大写。
func (s *Store) findByLeafFold(want string) (string, int) {
	want = strings.ToLower(want)
	found, n := "", 0
	for k := range s.items {
		l := k
		if idx := strings.LastIndex(k, "/"); idx >= 0 {
			l = k[idx+1:]
		}
		if strings.ToLower(l) == want {
			found, n = k, n+1
		}
	}
	return found, n
}

// resolve 把外部传入的名字解析到一个真实存在的条目。
//
// 为何不能只查规范名：scanDir 是按**盘上目录原样**建键的，所以修复前 Add
// 留下的目录（大写、带空格，如 "Tech/Upper"）在 items 里的键就是那个原样名。
// 直接拿 normalizeName 的结果去查会查不中，而盘上条目又确实存在——
// 结果就是老条目删不掉、清不清（实测）。查找按三层退让，但**删的路径
// 永远取自条目自记的 Path**，所以退让本身不带来误删风险。
func (s *Store) resolve(name string) (string, *Knowledge, error) {
	// 1. 原样精确匹配（scanDir 建键与新建的规范名都会命中这里）
	if k, ok := s.items[name]; ok {
		return name, k, nil
	}

	norm, nerr := normalizeName(name)
	leaf := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		leaf = name[i+1:]
	}
	// 2. 规范名匹配
	if nerr == nil {
		if k, ok := s.items[norm]; ok {
			return norm, k, nil
		}
		if i := strings.LastIndex(norm, "/"); i >= 0 {
			leaf = norm[i+1:]
		} else {
			leaf = norm
		}
	}

	// 3. 叶名匹配（大小写不敏感）。唯一命中才接受——多条同名时宁可不删，
	// 也不能猜错目录。
	if id, n := s.findByLeafFold(leaf); n == 1 {
		return id, s.items[id], nil
	} else if n > 1 {
		return "", nil, fmt.Errorf("%w: %q 命中 %d 条条目，请用全名", ErrNotFound, name, n)
	}

	// 都不中：名字本身非法就报非法（更具体），否则就是不存在。
	if nerr != nil {
		return "", nil, nerr
	}
	return "", nil, fmt.Errorf("%w: %q", ErrNotFound, name)
}

// scoreHit 是融合三路时统一的 (条目, 相似度) 形状。包级命名而非函数内
// 匿名 struct：三个来源（稠密/稀疏语义/词法）必须落在**同一**类型上，
// 否则 addPath 无法作为泛型收口点。
type scoreHit struct {
	id    string
	score float64
}

// hasInScopeLocked 报告某分类子树下是否存在条目（调用方须持锁）。
// 用于在分类过滤下提前返回，避免三路对全库白打分。
func (s *Store) hasInScopeLocked(category string) bool {
	for _, k := range s.items {
		if k.Category == category ||
			strings.HasPrefix(k.Category, category+"/") ||
			strings.HasPrefix(k.Name, category+"/") {
			return true
		}
	}
	return false
}

// denseHits 在多模态空间内对全库打分。
//
// 维度守卫是硬要求：不同模型/维度的向量混进来算出的余弦没有意义
// （会得到一个夹在两套坐标系之间的方向，且“看起来还挺像”）。维度不符
// 一律跳过。同维但指纹过期的（模型换过）也跳过。
func (s *Store) denseHits(queryVec []float64) []scoreHit {
	if !s.denseEnabled() || len(queryVec) == 0 {
		return nil
	}
	dim := s.dense.Dim()
	fp := s.dense.Fingerprint()
	out := make([]scoreHit, 0, len(s.items))
	for _, k := range s.items {
		if len(k.Dense) != dim || k.DenseFP != fp {
			continue
		}
		if score := vector.DenseCosine(queryVec, k.Dense); score > 0.01 {
			out = append(out, scoreHit{id: k.Name, score: score})
		}
	}
	// 分数相同时按名字定序：map 迭代顺序随机，缺了这一步同分条目的
	// 相对次序会随每次调用变化（Search 的主排序早有这条，denseHits 漏了），
	// 表现为「同样的查询两次给出不同首位」——测试偶发、用户看到结果在跳。
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].id < out[j].id
	})
	return out
}

// toHits 把稀疏路的 DocVectorHit 归一成 addPath 用的 (id, score) 形状。
func toHits(in []vector.DocVectorHit) []scoreHit {
	out := make([]scoreHit, 0, len(in))
	for _, h := range in {
		out = append(out, scoreHit{id: h.Doc.ID, score: h.Score})
	}
	return out
}

// shortFP 截断 fingerprint 为可读日志格式。
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}

// flushDenseLocked 把脏的稠密缓存落盘（调用方须持写锁）。
//
// 为何在 Add 当场落盘而不是等 Stop：进程可能被 kill -9，那时没有任何
// 优雅关停钩子可跑，这批向量的计算就白费了（coldcontextStore 的 BuildDenseIndex
// 出于同样理由选择当场写盘）。
func (s *Store) flushDenseLocked() error {
	if !s.denseDirty {
		return nil
	}
	// 批量期不落盘：saveDenseCacheLocked 是全量序列化 + 重写整个文件，
	// 逐条做就是 O(N²)。由 endBatch 收口一次。
	if s.batchDepth > 0 {
		return nil
	}
	s.saveDenseCacheLocked()
	s.denseDirty = false
	return nil
}

// ——— 稠密向量缓存 ———

// loadDenseCache 读回稠密向量缓存。只在该空间未变更时命中。
//
// 缓存本身是派生数据，坏了就当没有（下次重算），绝不返回 error 卡住启动。
func (s *Store) loadDenseCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maybeLoadDenseCacheLocked()
}

// maybeLoadDenseCacheLocked 在「已接线 + 已扫盘 + 未加载过」时恢复缓存的
// 稠密向量。调用方必须已持锁。
func (s *Store) maybeLoadDenseCacheLocked() {
	if s.denseCacheLoaded || !s.scanned || !s.denseEnabled() {
		return
	}
	s.denseCacheLoaded = true
	data, err := os.ReadFile(s.denseCachePath)
	if err != nil {
		return
	}
	var c denseCache
	if json.Unmarshal(data, &c) != nil {
		return
	}
	// 换过模型/维度后整份作废：否则会把一个坐标系的向量当另一个用
	if c.Fingerprint != s.dense.Fingerprint() || c.Dim != s.dense.Dim() {
		return
	}
	dim := s.dense.Dim()
	loaded := 0
	for id, e := range c.Entries {
		k, ok := s.items[id]
		if !ok || len(e.Dense) != dim {
			continue
		}
		k.Dense, k.DenseFP = e.Dense, e.FP
		loaded++
	}
	log.Printf("[knowledge] dense cache restored: %d vectors (fp=%s dim=%d)", loaded, shortFP(c.Fingerprint), dim)
}

// saveDenseCacheLocked 把稠密向量写回缓存文件（调用方须持写锁）。
//
// 用 tmp+rename 原子替换：写一半的缓存文件会被下次启动当成"损坏"而整体丢弃，
// 代价是一次全量重算——可接受，但不该每次都发生。
func (s *Store) saveDenseCacheLocked() {
	if !s.denseEnabled() {
		return
	}
	fp, dim := s.dense.Fingerprint(), s.dense.Dim()
	c := denseCache{Fingerprint: fp, Dim: dim, Entries: map[string]denseCacheEntry{}}
	for id, k := range s.items {
		if len(k.Dense) == dim && k.DenseFP == fp {
			c.Entries[id] = denseCacheEntry{Dense: k.Dense, FP: k.DenseFP, Dim: dim}
		}
	}
	data, err := json.Marshal(c)
	if err != nil {
		log.Printf("[knowledge] dense cache marshal error: %v", err)
		return
	}
	tmp := s.denseCachePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[knowledge] dense cache write error: %v", err)
		return
	}
	if err := os.Rename(tmp, s.denseCachePath); err != nil {
		os.Remove(tmp)
		log.Printf("[knowledge] dense cache commit error: %v", err)
	}
}

// ——— 媒体引用持久化 ———

// readMediaSidecar 读条目目录下的媒体引用文件。没有文件 = 无媒体（正常）。
func readMediaSidecar(dir string) []KnowledgeMediaRef {
	data, err := os.ReadFile(filepath.Join(dir, mediaSidecarName))
	if err != nil {
		return nil
	}
	var refs []KnowledgeMediaRef
	if json.Unmarshal(data, &refs) != nil || len(refs) == 0 {
		return nil
	}
	return refs
}

// writeMediaSidecar 把媒体引用写回条目目录。
// 媒体为空时删掉该文件，避免留下 "[]" 这种无意义的残留。
func writeMediaSidecar(dir string, refs []KnowledgeMediaRef) error {
	p := filepath.Join(dir, mediaSidecarName)
	if len(refs) == 0 {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(refs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0644)
}
