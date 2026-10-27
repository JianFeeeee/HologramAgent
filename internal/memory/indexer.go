package memory

import (
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

type Indexer struct {
	db       *GraphDB
	vec      *vector.Store
	veczer   *vector.TFIDFVectorizer
	mu       sync.RWMutex
	trained  bool
	recalled map[string]bool // 已通过工具调用显式召回的实体名，自动注入时跳过

	// recalledOrder 记录 recalled 的插入顺序，用于超限时按 FIFO 淘汰。
	recalledOrder []string

	// 增量同步状态：运行中新增的实体此前要等下一个归档心跳（Indexer.Sync）
	// 才进向量索引，在那之前只能靠关键词路命中——"刚记住的东西过一会儿才想得
	// 起来"就是这么来的。这里记下上次同步的实体数，召回前发现数量变了就补一次。
	lastSyncCount int
	lastSyncAt    time.Time

	// dense 是多模态稠密空间（可选）。场景召回的相关性用它算余弦 ——
	// 块里本来就有向量（memory_blocks.vector），所以"相关"就是一次向量比较，
	// 不需要另建索引，也不该退化成字符串匹配。
	dense vector.MultimodalEmbedder
}

// SetDenseSpace 注入稠密多模态空间（由内核装配时传入）。
func (idx *Indexer) SetDenseSpace(ds vector.MultimodalEmbedder) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	idx.dense = ds
	idx.mu.Unlock()
}

// retrainInterval 是增量重训的最小间隔。
//
// 为什么不每次都重训：TF-IDF 向量器是**全局**重训（词表与 idf 都变），
// 一次是 O(实体数)。没有下限时，密集写入的场景下每轮召回都触发一次全量重训，
// 把一个读操作变成写放大的热点。心跳（30min）+ 这个下限，够快也够稳。
const retrainInterval = 30 * time.Second

// maxRecalledEntities 是「已召回实体」去重集的上限。
//
// 无上限时它只增不减：进程活得越久，被永久跳过的实体越多，自动注入
// 越来越「沉默」——一个只在长跑进程里才暴露的隐蔽退化。超限后淘汰最旧的
// 名字（允许重新注入），而不是丢弃整个集合。
const maxRecalledEntities = 1024

func NewIndexer(db *GraphDB) *Indexer {
	return &Indexer{
		db:       db,
		vec:      vector.NewStore(),
		veczer:   vector.NewTFIDFVectorizer(TokenizeWords),
		recalled: make(map[string]bool),
	}
}

// MarkRecalled 标记实体名已被工具调用显式召回，后续自动注入时跳过
func (idx *Indexer) MarkRecalled(names ...string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, name := range names {
		if idx.recalled[name] {
			continue
		}
		idx.recalled[name] = true
		idx.recalledOrder = append(idx.recalledOrder, name)
	}
	for len(idx.recalledOrder) > maxRecalledEntities {
		oldest := idx.recalledOrder[0]
		idx.recalledOrder = idx.recalledOrder[1:]
		delete(idx.recalled, oldest)
	}
}

// Sync 从图数据库中同步实体名到向量索引
func (idx *Indexer) Sync() error {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	if idx.db == nil {
		return nil
	}

	result, err := idx.db.Recall(nil, nil, 1, "")
	if err != nil || result == nil {
		return err
	}

	// 收集实体名
	var names []string
	for _, e := range result.Entities {
		names = append(names, e.Name)
	}

	if len(names) == 0 {
		// 没有实体也是一次成功的同步：记下基线，否则 syncIfStale 会在
		// 「基线还停在旧值」与「本轮无实体」之间反复误判、每 30s 重跑一次。
		idx.lastSyncCount = 0
		idx.lastSyncAt = time.Now()
		return nil
	}

	// 训练向量化器
	idx.veczer.Train(names)

	// 重建向量索引
	idx.vec = vector.NewStore()
	for _, e := range result.Entities {
		vec := idx.veczer.Vectorize(e.Name)
		idx.vec.Insert(fmt.Sprintf("entity_%d", e.ID), e.Name, vec, map[string]string{
			"type": "entity",
			"name": e.Name,
		})
	}

	idx.trained = true
	idx.lastSyncCount = len(names)
	idx.lastSyncAt = time.Now()
	log.Printf("[indexer] synced %d entities to vector index", len(names))
	return nil
}

// syncIfStale 在实体数发生变化时补一次重建（调用方必须**不持锁**）。
//
// 返回是否发生了重建。计数查询走 idx_count 索引，成本可忽略；
// 只有真的变了、且离上次同步超过 retrainInterval 才重训。
func (idx *Indexer) syncIfStale() bool {
	if idx.db == nil {
		return false
	}
	// ★★ 基线改数**块**（2026-10-04）
	//
	// 原来数旧表 entities —— 而旧表双写已停，那张表不再增长
	// ⇒ count 恒为 0 ⇒ expected 恒为 0 ⇒
	//    **首次 syncIfStale 也判成「无需同步」**
	// ⇒ 索引永远不建立，而没有任何报错。
	//
	// 判据 TestSyncIfStaleBaseline 的「首次应建立索引」当场抓住。
	//
	// ★ 口径与 Sync 保持一致：Sync 走「无关键词全量召回」，
	//   实体数被 maxFullRecallEntities 封顶。
	//   ★ 用 COUNT(*) 而非走召回：Sync 内部也用这个封顶口径，
	//   两边一致才不会「永远不相等」——
	//   那会导致每 30s 全量重训，把一条读路径变成写放大热点。
	var count int
	if err := idx.db.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE text_content != ''`).Scan(&count); err != nil {
		return false
	}
	expected := count
	if expected > maxFullRecallEntities {
		expected = maxFullRecallEntities
	}

	idx.mu.RLock()
	known, last := idx.lastSyncCount, idx.lastSyncAt
	idx.mu.RUnlock()
	if expected == known {
		return false
	}
	if !last.IsZero() && time.Since(last) < retrainInterval {
		return false
	}
	if err := idx.Sync(); err != nil {
		log.Printf("[indexer] incremental sync failed (keeping old index): %v", err)
		return false
	}
	log.Printf("[indexer] entity count %d -> %d, incrementally rebuilt entity-name vector index", known, count)
	return true
}

type InjectedContext struct {
	Entities      []Entity   `json:"entities"`
	Relations     []Relation `json:"relations"`
	Summary       string     `json:"summary"`
	TokenEstimate int        `json:"token_estimate"`

	// Scenes 是本轮识别出的当前场景；SceneRelations 是被钉在这些场景上的
	// 记忆（带 relation_type 与原句）。两者都进注入文本——场景记忆是
	// **带条件的规则**，只给实体名等于没召回。
	Scenes         []string      `json:"scenes,omitempty"`
	SceneRelations []Relation    `json:"scene_relations,omitempty"`
	SceneBlocks    []MemoryBlock `json:"scene_blocks,omitempty"`
}

// BuildContext 不带场景的召回（保持既有行为：词法 + 实体名向量）。
func (idx *Indexer) BuildContext(userInput string) *InjectedContext {
	return idx.BuildContextInScene(userInput, nil)
}

// BuildContextInScene 在词法/向量召回之上叠加**场景召回**。
//
// 两条路正交且都要保留：
//   - 词法/向量：话题相关（「上次那个 bug 怎么修的」）
//   - 场景：条件相关（「在 QQ 上回消息」→ 不要 Markdown）
//
// 场景路不参与相似度打分、也不受关键词为空的影响：只要场面重现就该取回。
func (idx *Indexer) BuildContextInScene(userInput string, scenes []string) *InjectedContext {
	if idx.db == nil {
		return &InjectedContext{Summary: ""}
	}

	input := CleanText(userInput)

	// 0. 索引保鲜：运行中新增的实体不该等到下一个心跳才可被召回
	idx.syncIfStale()

	// 1. 向量搜索：从实体名向量索引中找到相关实体
	vectorEntities := idx.vectorSearchEntities(input)

	// 2. 关键词搜索：已有逻辑
	keywords := ExtractKeywords(input)
	if len(keywords) == 0 && len(vectorEntities) == 0 {
		keywords = []string{userInput}
	}

	// 合并关键词和向量找到的实体名
	seedNames := make([]string, 0, len(vectorEntities))
	for _, e := range vectorEntities {
		seedNames = append(seedNames, e.Name)
	}
	allKeywords := append(keywords, seedNames...)

	result, err := idx.db.Recall(allKeywords, nil, 2, "")
	if err != nil || result == nil {
		result = &RecallResult{}
	}

	// 3. 场景召回：当前场面钉住的记忆
	sceneRecall, err := idx.db.RecallByScene(scenes, maxSceneRecallRelations)
	if err != nil {
		sceneRecall = nil
	}
	sceneEntityNames := make(map[string]bool)
	if sceneRecall != nil {
		for _, e := range sceneRecall.Entities {
			sceneEntityNames[e.Name] = true
		}
	}

	// 过滤已被工具调用显式召回的实体，避免重复注入；场景实体已在场景块
	// 里给过，也不在索引里再占位。
	idx.mu.RLock()
	filtered := make([]Entity, 0, len(result.Entities))
	for _, e := range result.Entities {
		if idx.recalled[e.Name] || sceneEntityNames[e.Name] {
			continue
		}
		filtered = append(filtered, e)
	}
	idx.mu.RUnlock()

	ctx := &InjectedContext{
		Entities:  filtered,
		Relations: nil,
	}
	if sceneRecall != nil {
		// ★ 场景召回按**块向量余弦**排序并设下限（2026-10-27 现场）。
		//
		// RecallByScene 的排序是「场景权重 → 时间倒序」，与本轮 query 无关：
		// 老图库里 `rt 标签 / rtchild / inputch` 这类历史事实常年霸榜，
		// 每轮都灌同一批无关三元组（实测 1833 字符里九成是它们），
		// 真正刚聊的内容一句没有 —— 模型"忘事"就是这样产生的。
		//
		// 块里本就存着向量，所以相关性只是一次余弦比较（不建额外索引）。
		// 措辞零字面重合但语义相近的场景记忆仍然会被召回（既有判据要求）。
		ranked := idx.rankSceneRelationsByVector(input, sceneRecall.Relations)
		ctx.Scenes = sceneRecall.Scenes
		ctx.SceneRelations = ranked
		ctx.SceneBlocks = sceneRecall.Blocks
	}

	if len(filtered) > 0 {
		summary := buildIndexSummary(filtered)
		ctx.Summary = summary
		ctx.TokenEstimate = estimateTokens(summary) + len(filtered)*8
	} else {
		ctx.Summary = ""
	}

	return ctx
}

// vectorSearchEntities 在实体名向量索引中搜索
func (idx *Indexer) vectorSearchEntities(query string) []Entity {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	if !idx.trained || idx.vec.Size() == 0 {
		return nil
	}

	queryVec := idx.veczer.Vectorize(query)
	results := idx.vec.Search(queryVec, 5)

	var entities []Entity
	for _, r := range results {
		if r.Meta != nil && r.Meta["type"] == "entity" {
			entities = append(entities, Entity{Name: r.Meta["name"]})
		}
	}
	return entities
}

func (idx *Indexer) BuildToolPrompt() string {
	return `## Stable memory tools

You have these tools for the long-term stable memory:

### memory_recall
Retrieve entities and relations related to keywords.
Parameters:
- query_intent: keywords, comma separated
- depth: traversal depth (default 2)
- sort: relevance (default) or recent (newest first).
  Use recent when asked "latest / current value" - the answer is usually the newest value
  superseding an older one.

### memory_commit
Write triples into stable memory.
Parameters:
- triples: [{"subject": "entity", "relation": "type", "object": "target entity",
            "sentence_text": "original sentence (optional)",
            "media_digests": ["media digest (optional)"],
            "scene": "scene key (optional)"}]
  With media_digests, the image/audio can be retrieved from this memory later.
  With scene, the memory hangs on that **scene**: when the scene recurs (e.g. another QQ
  message arrives) it is recalled without a literal match. Put conventions and rules here,
  e.g. the QQ reply format -> scene="chan:qq".
- scene: default scene key for this batch (optional; per-triple scene wins).

### memory_introspect
Show memory statistics.

### memory_purge
Delete or fix memories.
Parameters:
- criteria: {"subject_contains": "...", "relation_type": "..."}
- mode: "soft" | "supersede"

Usage: call the tool during reasoning; the kernel executes it and returns the result.`
}

func (idx *Indexer) FormatContext(ctx *InjectedContext) string {
	if ctx == nil || (len(ctx.Entities) == 0 && len(ctx.SceneRelations) == 0 && len(ctx.SceneBlocks) == 0) {
		return ""
	}

	var b strings.Builder

	// 场景记忆排在前面：它们是**带条件的规则**（在什么场面下该怎么做），
	// 对行为的约束强于“话题相关的实体名”。也正因为带有触发条件，
	// 它们的正确性不依赖本轮措辞是否命中了字面。
	if len(ctx.SceneRelations) > 0 {
		b.WriteString(fmt.Sprintf("【场景记忆 %s】\n", strings.Join(ctx.Scenes, ", ")))
		for i, rel := range ctx.SceneRelations {
			if i >= maxSceneRecallRelations {
				break
			}
			b.WriteString(fmt.Sprintf("- %s --%s--> %s", rel.SourceName, rel.RelationType, rel.TargetName))
			if rel.SentenceText != "" {
				b.WriteString("（")
				b.WriteString(truncateRunes(rel.SentenceText, sceneSentenceMaxRunes))
				b.WriteString("）")
			}
			b.WriteString("\n")
		}
		// 场景块：块是流水线里最细的子项目（一段转写、一张图的描述）。
		// 只给 id 没用，要给能判断「这是什么」的短文本。
		if len(ctx.SceneBlocks) > 0 {
			b.WriteString("场景素材: ")
			for i, blk := range ctx.SceneBlocks {
				if i >= maxSceneRecallBlocks {
					b.WriteString("…")
					break
				}
				if i > 0 {
					b.WriteString(" | ")
				}
				b.WriteString(string(blk.Modality))
				b.WriteString(" ")
				if blk.Text != "" {
					b.WriteString(truncateRunes(blk.Text, sceneSentenceMaxRunes))
				} else {
					b.WriteString(shortBlockDigest(blk.ID))
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("（以上是该场景下的既有约定，请照办）\n")
	}

	if len(ctx.Entities) == 0 {
		return strings.TrimRight(b.String(), "\n")
	}

	b.WriteString("【记忆索引】")

	if ctx.Summary != "" {
		b.WriteString(" ")
		b.WriteString(ctx.Summary)
	}

	b.WriteString(fmt.Sprintf(" 索引: "))
	for i, e := range ctx.Entities {
		if i >= 5 {
			b.WriteString("…")
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(e.Name)
		if e.Type != "Concept" {
			b.WriteString("(" + e.Type + ")")
		}
	}

	b.WriteString(" | 需更多细节请用 memory_recall 查询")
	return b.String()
}

// sceneRelevanceFloor 是场景关系的余弦下限。
//
// 0.20 的依据：多模态空间下同一段对话内的话题相关短语通常 ≥0.35，
// 而跨主题（rt 设计 vs 平板闲聊）实测多在 0.1 以下；0.20 夹在中间，
// 既不会把"换了个说法"的真记忆挡在门外，也不会让长期霸榜的旧事实混进来。
const sceneRelevanceFloor = 0.20

// rankSceneRelationsByVector 用 query 向量给场景关系打分并截断。
//
// 无稠密空间（未启用多模态）时退回原顺序：此时没有可用向量，
// 宁可少一层排序，也不能用字符串匹配冒充语义（既有判据明确要求
// 「措辞与记忆零字面重合时场景路必须兜住」）。
func (idx *Indexer) rankSceneRelationsByVector(query string, rels []Relation) []Relation {
	if len(rels) == 0 {
		return nil
	}
	idx.mu.RLock()
	ds := idx.dense
	idx.mu.RUnlock()
	if ds == nil || !ds.Loaded() || strings.TrimSpace(query) == "" {
		if len(rels) > maxSceneRecallRelations {
			return rels[:maxSceneRecallRelations]
		}
		return rels
	}
	qv, err := ds.VectorizeDense(query)
	if err != nil || len(qv) == 0 {
		if len(rels) > maxSceneRecallRelations {
			return rels[:maxSceneRecallRelations]
		}
		return rels
	}
	type scored struct {
		rel Relation
		sim float64
	}
	out := make([]scored, 0, len(rels))
	for _, rel := range rels {
		best := 0.0
		for _, v := range [][]float64{rel.SentenceVec, rel.SourceVec, rel.TargetVec} {
			if len(v) != len(qv) {
				continue
			}
			if s := denseCosine(qv, v); s > best {
				best = s
			}
		}
		if best >= sceneRelevanceFloor {
			out = append(out, scored{rel: rel, sim: best})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].sim > out[j].sim })
	if len(out) > maxSceneRecallRelations {
		out = out[:maxSceneRecallRelations]
	}
	kept := make([]Relation, 0, len(out))
	for _, e := range out {
		kept = append(kept, e.rel)
	}
	return kept
}

// denseCosine 计算两个等长向量的余弦相似度（零向量返回 0）。
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
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// maxSceneRecallRelations 是单次场景召回的关系上限。
//
// 场景是**每轮都要注入**的常驻内容：不封顶时一个宽场景（如 chan:qq）
// 会把它下面所有关系都推进 prompt，把 token 预算吃光。
const maxSceneRecallRelations = 8

// sceneSentenceMaxRunes 是场景关系后附原句的截断长度。
const sceneSentenceMaxRunes = 60

// maxSceneRecallBlocks 是场景块在注入文本里的条数上限（同为常驻内容，要封顶）。
const maxSceneRecallBlocks = 3

// shortBlockDigest 取 digest 前 12 位做展示（与 L3 里引用媒体的写法一致）。
// shortBlockDigest 把块 ID 截短，供提示词里指认媒体块。
//
// 参数曾是内容 digest；块自带负载后，指认单位改成**块 ID**（模型按块 ID
// 取回该块的负载，不再经内容寻址层中转）。
func shortBlockDigest(d string) string {
	if len(d) <= 12 {
		return d
	}
	return d[:12]
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func (idx *Indexer) GetToolDefinitions() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name": "memory_recall",
				"description": "检索图记忆。输入查询意图关键词，返回相关实体和关系。" +
					"问『某个具体东西是什么/是多少』用默认相关性排序；" +
					"问『最近/最新/现在是什么』必须传 sort=recent —— " +
					"运维场景里答案常是新值覆盖旧值，相关性排序分不出新旧。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query_intent": map[string]interface{}{
							"type":        "string",
							"description": "查询意图，支持逗号分隔多个关键词",
						},
						"depth": map[string]interface{}{
							"type":        "integer",
							"description": "遍历深度，默认2",
							"default":     2,
						},
						"sort": map[string]interface{}{
							"type": "string",
							"description": "排序方式。relevance=相关性（默认，问具体是什么/多少）；" +
								"recent=时间倒序最新在前（问最近/最新/现在）。",
							"enum": []string{"relevance", "recent"},
						},
					},
					"required": []string{"query_intent"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_commit",
				"description": "写入图记忆。将三元组列表写入长期记忆。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"triples": map[string]interface{}{
							"type":        "array",
							"description": "三元组列表",
							"items": map[string]interface{}{
								"type": "object",
								"properties": map[string]interface{}{
									"subject":  map[string]interface{}{"type": "string"},
									"relation": map[string]interface{}{"type": "string"},
									"object":   map[string]interface{}{"type": "string"},
									"sentence_text": map[string]interface{}{
										"type":        "string",
										"description": "可选：这条三元组的原始句子。填了才能日后从图谱回到原文。",
									},
									"media_digests": map[string]interface{}{
										"type":        "array",
										"description": "可选：这条记忆关联的媒体 digest（对话或 memory_recall 的「关联媒体」里显示的十六进制串，短的即可）。填了以后从这条记忆能取回原图/音频。",
										"items":       map[string]interface{}{"type": "string"},
									},
									"scene": map[string]interface{}{
										"type":        "string",
										"description": "可选：这条记忆所属的场景键（如 chan:qq、chan:qq/peer:group_123、tool:qq_get_message）。「在什么场合该怎么做」这类条件性约定/规则必须填：场面重现时它会被直接召回，与用户措辞无关。",
									},
								},
								"required": []string{"subject", "relation", "object"},
							},
						},
					},
					"required": []string{"triples"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "memory_introspect",
				"description": "查看图记忆统计信息：实体数量、关系数量、热点实体。",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
	}
}

func buildIndexSummary(entities []Entity) string {
	if len(entities) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("关联 %d 个记忆实体", len(entities)))

	// 「高频」必须真的按提及次数排。
	//
	// 此前取的是 entities[0..2]，也就是 Recall 的返回顺序（旧实现下等于建表
	// 顺序），却写着"高频"两个字：给模型的暗示是"这几条最重要"，实际是
	// "这几条建得最早"。名不副实的标签比没有标签更坏——它会稳定地误导。
	byMentions := make([]Entity, len(entities))
	copy(byMentions, entities)
	sort.SliceStable(byMentions, func(i, j int) bool {
		if byMentions[i].MentionCount != byMentions[j].MentionCount {
			return byMentions[i].MentionCount > byMentions[j].MentionCount
		}
		return byMentions[i].Name < byMentions[j].Name
	})

	topN := 3
	if len(byMentions) < topN {
		topN = len(byMentions)
	}
	b.WriteString("，高频：")
	for i := 0; i < topN; i++ {
		if i > 0 {
			b.WriteString("、")
		}
		b.WriteString(byMentions[i].Name)
	}

	return b.String()
}

func estimateTokens(s string) int {
	return len(s) / 2
}
