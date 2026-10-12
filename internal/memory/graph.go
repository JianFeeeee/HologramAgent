package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// maxKeywordEntities 是单个关键词能取回的实体上限。
//
// 无上限时，一个宽关键词（"QQ"）会命中上百个实体并逐个参与深度扩展，
// 把一次召回变成一次全表扫描。
const maxKeywordEntities = 50

// maxRecallEntities 是**一次召回全局**的实体上限（跨所有关键词）。
//
// ★ 为什么必须有它（2026-10-01 跑分实测）：maxKeywordEntities 是**每个关键词**
// 的上限，而 ExtractKeywords 会把一个问句切成多个词（实测
// 「metrics服务的端口是多少」→ [metrics, 服务, 端口]，其中「服务」「端口」
// 是无区分度的泛词）。于是 3 个关键词 × 50 = 最多 150 个实体被**平铺**进上下文，
// 实测 memory_recall 单次返回 193 个实体 / 13744 tokens = 工具预算的 436%，
// 模型被噪音淹没后转去 grep 知识库文件，还把「没检索到」当成「不存在」。
//
// 实验结论（internal/memory 内的三方案对比，见 benchmark 记录）：排序不是瓶颈
// —— 目标实体在三种方案里都排#1。缺的是**全局上限**。
const maxRecallEntities = 20

// maxAdjacentRelations 是深度扩展里**每层**读取的关系上限。
const maxAdjacentRelations = 200

// maxFullRecallEntities 是「无关键词全量读取」路径的实体上限。
// 该路径只服务于内部整备（Indexer.Sync / 实体合并检测），并非用户检索；
// 无上限时一张大图会被整表 read 进内存。超限时 GraphDB.Recall 会记日志。
const maxFullRecallEntities = 10000

type Entity struct {
	// MatchRank 是**实词关键词**上的最佳命中层级（0=完全相等, 1=前缀, 2=包含）。
	//
	// ★ 为什么实体要带这个字段（2026-10-01 跑分实测）：召回结果平铺给模型时，
	// 「为什么这条相关」此前完全不可见 —— 模型看到 193 个同格式的
	// 「-名称(提及N次)」，无从判断该信哪个，于是转去 grep 知识库文件，
	// 还把「没检索到」当成「不存在」。带上层级后最相关的几条一眼可辨。
	// 仅在关键词召回路径上填充；深度扩展产出与 seed 路径为 -1（未知）。
	MatchRank int `json:"match_rank,omitempty"`
	// Seq 是该实体最新一次被写入时的**句子 id**（sentences.id，自增单调）。
	//
	// ★ 为什么实体时间排序需要它（真实库实测）：entities.updated_at 来自
	// SQLite CURRENT_TIMESTAMP，只到**秒**，而一批记忆常在同一秒内批量写入。
	// 实测 ha-c 生产库 190 个实体里最多的一批 20 个实体共享同一秒。
	// sentences.id 是自增主键，严格单调，是唯一可靠的时序信号。
	// 0 表示该实体没有关联句子（合成/导入的实体），此时回退到 UpdatedAt。
	Seq          int64     `json:"seq,omitempty"`
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	MentionCount int       `json:"mention_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`

	// blockKey 是对应的**块 ID**（2026-10-04）。
	//
	// ★ 为什么 ID 不能直接用块 ID：Entity.ID 是 int64（JSON 与 SDK 契约），
	//   而块 ID 是内容派生的字符串（blk_ent_<hash>）—— 放不进去。
	//   所以 ID 退化为「本次召回内的序号」，块 ID 走这个新字段。
	//
	// ★ 为什么需要它：召回内部要用块 ID 做 map 键（跨关键词去重、
	//   深度扩展的前沿集合），而出口排序 sortRecallEntities 按
	//   Entity.ID 建索引 —— 两者需要一次映射。
	//
	// ★ 不导出到 JSON（omitempty + json:"-"）：它是内部索引键，
	//   不是对外契约的一部分 —— 对外应该是 block_id 而不是这个临时序号。
	blockKey string `json:"-"`
}

// IsLegacyRow 报告这个实体行是**旧表**（entities）来的，而不是块。
//
// ★ 存在的理由：读侧切块之后，External Recall 回来的东西既可能是块
//
//	（blockKey 非空）也可能是旧表行（blockKey 为空）。
//	而判据常需要区分「块」与「旧表残留」——
//	两者 Name/Type 可能完全一样，光看内容分不出来。
//
// ★ 只读，不进 JSON。
func (e Entity) IsLegacyRow() bool { return e.blockKey == "" }

type Relation struct {
	ID       int64 `json:"id"`
	SourceID int64 `json:"source_id"`
	TargetID int64 `json:"target_id"`
	// ★★ 块体系的端点 ID（2026-10-04）
	//
	// 旧 SourceID/TargetID 是 relations 表的行号，退场后无效。
	// 块体系里端点是 memory_blocks.id（字符串，内容派生的稳定 ID）。
	// 两个字段并存：RecallByScene 已走块侧，旧字段留给旧表读方；
	// 旧表退场时删掉 SourceID/TargetID 即可。
	SourceBlockID string    `json:"source_block_id,omitempty"`
	TargetBlockID string    `json:"target_block_id,omitempty"`
	SourceName    string    `json:"source_name"`
	TargetName    string    `json:"target_name"`
	RelationType  string    `json:"relation_type"`
	Confidence    float64   `json:"confidence"`
	Status        string    `json:"status"`
	SessionID     string    `json:"session_id"`
	TurnID        int       `json:"turn_id"`
	CreatedAt     time.Time `json:"created_at"`
	DateBucket    string    `json:"date_bucket"`
	SentenceID    int64     `json:"sentence_id,omitempty"`   // FK → sentences.id（退场后不用）
	SentenceText  string    `json:"sentence_text,omitempty"` // 原句全文（走原句块回溯）
	// SentenceBlockID 是原句块的 ID（blk_src_<hash>），供场景引用与去重。
	SentenceBlockID string `json:"sentence_block_id,omitempty"`
}

type Triple struct {
	Subject      string  `json:"subject"`
	Relation     string  `json:"relation"`
	Object       string  `json:"object"`
	Confidence   float64 `json:"confidence,omitempty"`
	SubjectType  string  `json:"subject_type,omitempty"`
	ObjectType   string  `json:"object_type,omitempty"`
	SentenceText string  `json:"sentence_text,omitempty"` // 原始句子文本，Commit时写入sentences表
	// MediaDigests 是该三元组显式携带的媒体 digest（完整或前缀）。
	// 媒体不再靠正文 marker 反解：结构化字段直接给出归属，
	// 由调用方（core）把它变成 L3 一等块并与句子建立结构边。
	MediaDigests []string `json:"media_digests,omitempty"`
	// Scene 是这条记忆所属的**场景键**（可空）。写完后该三元组的两个实体
	// 与这条关系都会被挂到这个场景上，场景重现时按场景召回。
	// 约定见 memory.NormalizeSceneKey：`chan:qq`、`chan:qq/peer:group_123`。
	Scene string `json:"scene,omitempty"`
	// Scenes 是同一批多场景挂载（可空）：主动**声明**的场景（如 chan:qq）
	// 与被动**涌现**出来的场景（如 auto:chan:qq+peer_group:1027）可以同时挂。
	//
	// 为什么要两条都挂：声明路是"我知道这是哪个场面"，稳定、可读、可兜底；
	// 涌现路是"这轮看起来像哪个场面"，细粒度、不需要任何人声明。
	// 只挂一条的代价：只挂声明则细粒度唤起丢失，只挂涌现则首次交互（场景
	// 还没长出来）没有兜底。
	Scenes []string `json:"scenes,omitempty"`
}

type GraphDB struct {
	db     *sql.DB
	mu     sync.RWMutex
	dbPath string

	// onAccess 是**图读写钩子**（可为 nil），由 hookMu 保护。
	//
	// ❗为什么钩子必须用**独立的**锁：读写钩子在 commit/Recall 这类方法的同步
	// 路径上触发，而那些方法已持 g.mu。若钩子也去取 g.mu，就是**自死锁**
	// （Go 的 sync.RWMutex 不可重入）。
	//
	// ❗为什么埋在图数据库而不是各调用方：图读写散在 26 个方法与 180+ 个
	// db 调用点上，让每个调用方自己上报必然漏发。而**图数据库自己就知道
	// 它碰了哪些块 ID** —— 这正是星图需要的（它此前靠工具名猜节点，实测命中 0）。
	//
	// 钩子只上报**事实**（op / 块 ID / 工具名），不关心消费者是谁；
	// 内核把它接到 events.Bus，CLI 工具不接（nil 即静默跳过）。
	hookMu   sync.RWMutex
	onAccess AccessHook
}

// AccessEvent 描述一次图读写。
//
// 用结构体而非多个参数：字段后续可能增加（如关系 ID），
// 而签名一旦发布就不好改（调用点数量多）。
//
// ❗要求：实现**必须非阻塞**。钩子虽在 GraphDB 的锁外调用，
// 但仍在业务调用的同步路径上 —— 慢钩子会直接拖慢记忆读写。
// 发布到事件总线是内存操作 + 各订阅者回调，实际开销极小。
type AccessEvent struct {
	// Op 是本次操作类型：recall / commit / merge / purge / delete。
	Op string
	// Blocks 是本次实际读到的块 ID（recall）或写入的块 ID（commit）。
	Blocks []string
	// Created 是本次**新建**的块 ID（驱动星图「从小变大」生长动画）。
	Created []string
	// Removed 是被删除/取代而消失的块 ID（驱动粒子消散动画）。
	Removed []string
	// Merged 是合入对（from → to），驱动「两节点消失又出现」。
	Merged []MergePair
	// Tool 是触发本次访问的工具名（可空）。
	Tool string
}

// MergePair 是一次合入的源与目标块 ID。
type MergePair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// AccessHook 是钩子函数类型。
type AccessHook func(ev AccessEvent)

// SetAccessHook 安装读写钩子（传 nil 卸载）。
//
// 与 GraphDB 的构造解耦：NewGraphDB 的调用点有 7 处（含 4 个独立 CLI 工具），
// 而只有内核进程需要上报 —— 加构造参数会逼所有调用点改签名。
func (g *GraphDB) SetAccessHook(h AccessHook) {
	g.hookMu.Lock()
	g.onAccess = h
	g.hookMu.Unlock()
}

// emitAccess 上报一次图访问。调用方**必须已释放 g.mu**（见 onAccess 的注释）。
//
// 钩子为 nil 时零开销（一次指针比较），因此 CLI 工具与测试无需任何改动。
func (g *GraphDB) emitAccess(ev AccessEvent) {
	g.hookMu.RLock()
	h := g.onAccess
	g.hookMu.RUnlock()
	if h == nil {
		return
	}
	// 空事件不上报：星图收到空数组只会白白重算一次。
	if len(ev.Blocks) == 0 && len(ev.Created) == 0 && len(ev.Removed) == 0 && len(ev.Merged) == 0 {
		return
	}
	h(ev)
}

func NewGraphDB(dbPath string) (*GraphDB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open graph db: %w", err)
	}

	g := &GraphDB{db: db, dbPath: dbPath}
	if err := g.initSchema(); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return g, nil
}

// OpenGraphDBReadOnly 以**受限句柄**打开图库：可读、可恢复 WAL，但**一切写入被拒**。
//
// 这是"子 agent 改不了主记忆"的**结构性**保证（设计 docs/zh/resident-subagent-design.md
// §5）：不是靠调用方自觉不写，而是把写入在 SQLite 这一层就关掉
// （`PRAGMA query_only=1` —— 任何 INSERT/UPDATE/DELETE 都会直接报错）。
//
// 为什么不用 DSN 的 `mode=ro`：只读模式的连接在 WAL 库上无法自行恢复 -wal，
// 而主库在父 agent 手里是持续写入的。query_only 让连接保持正常打开能力，
// 同时**只堵写**，语义正是我们要的。
//
// 注意：不建表、不迁移 —— 受限句柄假定库已存在（由父 agent 建好）。
func OpenGraphDBReadOnly(dbPath string) (*GraphDB, error) {
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open graph db (readonly): %w", err)
	}
	if _, err := db.Exec("PRAGMA query_only=1"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("set query_only: %w", err)
	}
	return &GraphDB{db: db, dbPath: dbPath}, nil
}

func (g *GraphDB) initSchema() error {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// scene_features 是场面指纹的特征集合：场景 = 一组反复共现的可观察特征，
	// 相似度按加权 Jaccard 算（权重由特征种类决定，chan/peer 最强）。
	// situation_evidence 记录一次性指纹的足迹：同类指纹重复出现到
	// minSceneEvidence 次才长出场景。
	schemas := []string{
		`CREATE TABLE IF NOT EXISTS entities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			type TEXT DEFAULT 'Concept',
			mention_count INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sentences (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			text TEXT UNIQUE NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			status TEXT DEFAULT 'active',
			session_id TEXT,
			turn_id INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			date_bucket TEXT,
			sentence_id INTEGER DEFAULT 0,
			FOREIGN KEY (source_id) REFERENCES entities(id),
			FOREIGN KEY (target_id) REFERENCES entities(id),
			UNIQUE(source_id, target_id, relation_type, session_id)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_blocks (
			id TEXT PRIMARY KEY,
			modality TEXT NOT NULL,
			text_content TEXT DEFAULT '',
			-- data 是媒体块的负载（union 的另一半）：文本块用 text_content，
			-- 媒体块用 data。字节随块同生同灭，块外不存在独立的字节存储层。
			data BLOB,
			mime TEXT DEFAULT '',
			size INTEGER DEFAULT 0,
			width INTEGER DEFAULT 0,
			height INTEGER DEFAULT 0,
			vector TEXT DEFAULT '',
			fingerprint TEXT DEFAULT '',
			source TEXT DEFAULT '',
			tool TEXT DEFAULT '',
			scene TEXT DEFAULT '',
			-- semantic_type 是实体的语义类别（Person / Animal / Concept），
			-- 对应旧 entities.type。2026-10-04 加入：social 层靠它
			-- 区分人物与特质，检索层靠它区分「谁」与「什么」。
			semantic_type TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS memory_block_edges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_kind TEXT NOT NULL,
			source_id TEXT NOT NULL,
			target_kind TEXT NOT NULL,
			target_id TEXT NOT NULL,
			edge_type TEXT NOT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
			-- ★ 无 UNIQUE 约束：边是独立单位，同一对节点间可并存多条同类边。
			--   结构边的幂等由 AddMemoryBlockEdge 的先查后写保证。
		)`,
		`CREATE TABLE IF NOT EXISTS documents (
			id         TEXT PRIMARY KEY,
			summary    TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		// 场景引用：给「记忆节点」再赋一层**触发条件**。
		//
		// 为什么需要它：词法/向量召回都靠「字面或语义相似」，而带条件的规则
		// （「回 QQ 消息不要用 Markdown」「老大消息优先」）在措辞不重合时根本
		// 召不回来。场景是这类记忆的**索引键**：节点记住自己「属于哪个场面」，
		// 场面重现（又来一条 QQ 消息）时直接按场景取回，不靠字面命中。
		`CREATE TABLE IF NOT EXISTS scenes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT UNIQUE NOT NULL,
			-- strength 是场景被重现的次数：场景不是被声明出来的，是被反复遇到
			-- 长出来的（见 EnterScene / minSceneEvidence）。
			strength INTEGER DEFAULT 1,
			-- origin 区分两条路：'declared' 是主动声明出来的（键由人/插件给，
			-- 召走精确+前缀匹配），'emergent' 是被动涌现的（召走相似度）。
			-- 两者刻意**分开**参与匹配：若让声明场景也吸收整轮指纹，
			-- 它会在相似度上压过一切，被动路就再也长不出更细的场面了。
			origin TEXT NOT NULL DEFAULT 'emergent',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS scene_features (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scene_id INTEGER NOT NULL,
			feature TEXT NOT NULL,
			weight REAL DEFAULT 1.0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(scene_id, feature)
		)`,
		`CREATE TABLE IF NOT EXISTS situation_evidence (
			label TEXT PRIMARY KEY,
			count INTEGER DEFAULT 1,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_features_scene ON scene_features(scene_id)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_features_feature ON scene_features(feature)`,
		// ref_id 的解释由 kind 决定（relation / entity）。这里不用外键：
		// 节点可能先于引用被清理（PurgeNoise/PurgeOrphans），悬空引用由
		// 读取侧的 JOIN 自然过滤掉，而级联删除会把清理变成一个跨表事务。
		`CREATE TABLE IF NOT EXISTS scene_refs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scene_id INTEGER NOT NULL,
			kind TEXT NOT NULL,
			ref_id INTEGER NOT NULL,
			-- ref_text 承载非数值主键的节点 id（块/文档的 id 是字符串），
			-- 数值型节点（relation/entity）为空串。
			ref_text TEXT NOT NULL DEFAULT '',
			weight REAL DEFAULT 1.0,
			-- decayed_at 是半衰期衰减的计时起点：每个引用至多每 halfLife
			-- 衰减一次（见 DecaySceneRefs）。重复写入/强化会把它刷成当前时刻。
			decayed_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(scene_id, kind, ref_id, ref_text)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_refs_scene ON scene_refs(scene_id, kind)`,
		`CREATE INDEX IF NOT EXISTS idx_scene_refs_ref ON scene_refs(kind, ref_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_blocks_modality ON memory_blocks(modality)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_source ON memory_block_edges(source_kind, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_target ON memory_block_edges(target_kind, target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_entity_type ON entities(type)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_source ON relations(source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_target ON relations(target_id)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_type ON relations(relation_type)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_status ON relations(status)`,
		`CREATE INDEX IF NOT EXISTS idx_relation_session ON relations(session_id)`,
	}

	for _, s := range schemas {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("schema exec: %w", err)
		}
	}

	// 迁移1：兼容旧版 sentence_ref 列（已有表则忽略）
	tx.Exec(`ALTER TABLE relations ADD COLUMN sentence_ref TEXT DEFAULT ''`)
	// 迁移2：为新表添加 sentence_id 列（必须放在索引创建之前，否则旧表无此列导致索引创建失败）
	tx.Exec(`ALTER TABLE relations ADD COLUMN sentence_id INTEGER DEFAULT 0`)
	// 迁移4：记忆块加场景列（旧表已存在时 CREATE TABLE IF NOT EXISTS 不会补列）
	tx.Exec(`ALTER TABLE memory_blocks ADD COLUMN scene TEXT DEFAULT ''`)
	// 迁移6：旧 scenes 表加 strength 列（涌现侧的强度计数）
	tx.Exec(`ALTER TABLE scenes ADD COLUMN strength INTEGER DEFAULT 1`)
	// ★ 迁移7：记忆块加语义类型列（2026-10-04）
	//
	// ★ 为什么必须补：Triple.SubjectType / ObjectType 此前**只写旧表
	//   entities.type**，块侧完全没有 —— 于是 Commit 标注的
	//   「这是 Person / 那是 Animal」在块体系里直接丢失。
	//
	//   判据 TestGraphCommit_CarriesAllFields 当场抓到：
	//   实体类型 = ("block","block")，期望 (Person,Animal)。
	//
	//   ★ 命名用 semantic_type 而不是 type：
	//   `type` 在 SQLite 里合法但与很多工具的保留字冲突
	//   （且 memory_blocks 已有 modality 列表达「模态」，语义类型是另一回事）。
	tx.Exec(`ALTER TABLE memory_blocks ADD COLUMN semantic_type TEXT DEFAULT ''`)
	// ★★★ 迁移9：媒体负载内联进块（2026-10-11）
	//
	// 淘汰独立的字节层（media.Store/CAS）：媒体内容成为块自己的负载，
	// 与文本块的 text_content 对称——一个块要么带文本、要么带媒体字节。
	// 旧库的 payload_digest 列保留（迁移脚本据此回填 data），但不参与新写入。
	tx.Exec(`ALTER TABLE memory_blocks ADD COLUMN data BLOB`)

	// ★★★ 迁移8：memory_block_edges 升格为「边是独立单位」
	//
	// 补五列（对应旧 relations 表的同名列）：
	//
	//	confidence   置信度 —— 「值覆盖」维度靠它判断哪条更可信
	//	session_id   来源会话 —— Recall 的 sessionFilter 依赖它
	//	turn_id      来源轮次
	//	status       active/deleted/merged —— 软删除与仲裁依赖它
	//	merged_into  源块被并进哪个块（历史边留在源块上）
	//
	// ★ 建表语句里的 UNIQUE(source_kind,source_id,target_kind,target_id,
	//   edge_type) **必须去掉** —— 它让同一对节点间只能存一条同类型边，
	//   而「边是独立单位」要求可并存多条（不同 session/confidence 是
	//   不同的事实）。实测那正是「报告 980、实际 959」的根因。
	//
	//   SQLite 不能直接删 UNIQUE 约束 ⇒ 见下方 ensureRelationEdgeUniqueness。
	//   结构边（contains 等）靠 AddMemoryBlockEdge 自己的
	//   「先查后写」保持幂等，不依赖 DB 约束。
	for _, m := range []struct{ table, col, decl string }{
		{"memory_block_edges", "confidence", "REAL DEFAULT 0"},
		{"memory_block_edges", "session_id", "TEXT DEFAULT ''"},
		{"memory_block_edges", "turn_id", "INTEGER DEFAULT 0"},
		{"memory_block_edges", "status", "TEXT DEFAULT ''"},
		{"memory_block_edges", "merged_into", "TEXT DEFAULT ''"},
	} {
		if !columnExists(tx, m.table, m.col) {
			tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
				m.table, m.col, m.decl))
		}
	}
	ensureRelationEdgeUniqueness(tx)
	// 迁移7：旧 scenes 表加 origin。既有行都是声明/存量引导来的（建表时还没有
	// 涌现机制），标成 declared；新建的涌现场景在 createSceneLocked 里写 emergent。
	if !columnExists(tx, "scenes", "origin") {
		tx.Exec(`ALTER TABLE scenes ADD COLUMN origin TEXT NOT NULL DEFAULT 'emergent'`)
		tx.Exec(`UPDATE scenes SET origin = 'declared'`)
	}
	// 迁移5：场景引用加 ref_text（块/文档的 id 是字符串）。
	//
	// 不能只 `ALTER TABLE ADD COLUMN`：REF_TEXT 同时参与唯一约束
	// （scene_id, kind, ref_id, ref_text），而 ALTER 改不了已有约束。旧约束
	// (scene_id, kind, ref_id) 会让「同一场景下的第 2 个块」直接冲突——
	// 表现是块写不进场景、且只在有多个块时才出现。
	// 因此按需整表重建（表小、操作幂等）：判定依据是 ref_text 列是否存在。
	if !columnExists(tx, "scene_refs", "ref_text") {
		migrate := []string{
			`CREATE TABLE scene_refs_new (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				scene_id INTEGER NOT NULL,
				kind TEXT NOT NULL,
				ref_id INTEGER NOT NULL,
				ref_text TEXT NOT NULL DEFAULT '',
				weight REAL DEFAULT 1.0,
				created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
				UNIQUE(scene_id, kind, ref_id, ref_text)
			)`,
			`INSERT INTO scene_refs_new (id, scene_id, kind, ref_id, ref_text, weight, created_at)
			 SELECT id, scene_id, kind, ref_id, '', weight, created_at FROM scene_refs`,
			`DROP TABLE scene_refs`,
			`ALTER TABLE scene_refs_new RENAME TO scene_refs`,
			`CREATE INDEX IF NOT EXISTS idx_scene_refs_scene ON scene_refs(scene_id, kind)`,
			`CREATE INDEX IF NOT EXISTS idx_scene_refs_ref ON scene_refs(kind, ref_id)`,
		}
		for _, m := range migrate {
			if _, err := tx.Exec(m); err != nil {
				return fmt.Errorf("migrate scene_refs: %w", err)
			}
		}
	}
	// 迁移8：scene_refs 加 decayed_at（半衰期衰减的计时起点）。
	// ALTER 不接受非常量默认值，先加可空列再用 created_at 回填，
	// 于是既有引用的「上一次衰减」就定在它被写入的时刻，不会被立即清掉。
	if !columnExists(tx, "scene_refs", "decayed_at") {
		tx.Exec(`ALTER TABLE scene_refs ADD COLUMN decayed_at TIMESTAMP`)
		tx.Exec(`UPDATE scene_refs SET decayed_at = created_at WHERE decayed_at IS NULL`)
	}
	// 冗余索引清理：entities.name 与 sentences.text 上的 UNIQUE 已隐含等价索引
	// （sqlite_autoindex_*），再建一个同列索引只增加写放大，查询不会用到。
	tx.Exec(`DROP INDEX IF EXISTS idx_entity_name`)
	tx.Exec(`DROP INDEX IF EXISTS idx_sentences_text`)
	// 迁移3：将现有 sentence_ref 数据迁移到 sentences 表
	tx.Exec(`INSERT OR IGNORE INTO sentences (text) SELECT DISTINCT sentence_ref FROM relations WHERE sentence_ref != ''`)
	tx.Exec(`UPDATE relations SET sentence_id = (SELECT id FROM sentences WHERE text = relations.sentence_ref) WHERE sentence_ref != ''`)

	// sentence_id 索引在迁移后创建，避免旧表缺少该列时失败
	tx.Exec(`CREATE INDEX IF NOT EXISTS idx_relation_sentence ON relations(sentence_id)`)

	// 迁移4：为旧版 relations 表（无复合唯一约束）重建表以去重。
	// 旧表由 2026-07 之前的版本创建，缺少 UNIQUE(source_id, target_id, relation_type, session_id)，
	// 生产库累积了海量重复关系。这里检查 sqlite_master 中已建表的 DDL，
	// 若不含该约束则走"新建带约束表 → INSERT OR IGNORE 拷贝去重 → 换名"的官方 12 步迁移。
	if err := g.migrateRelationUnique(tx); err != nil {
		return fmt.Errorf("migrate relations unique: %w", err)
	}

	return tx.Commit()
}

// migrateRelationUnique 检测 relations 表是否带复合唯一约束，缺失则重建去重。
// 必须在 initSchema 的同一个事务内调用（外键/索引均已存在时需先禁用外键再换名）。
func (g *GraphDB) migrateRelationUnique(tx *sql.Tx) error {
	var ddl string
	err := tx.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'relations'`).Scan(&ddl)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil // 表都不存在，无从迁移
		}
		return err
	}
	if strings.Contains(ddl, "UNIQUE") {
		return nil // 已是新 schema
	}

	stmt := []string{
		`ALTER TABLE relations RENAME TO relations_old`,
		`CREATE TABLE relations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_id INTEGER NOT NULL,
			target_id INTEGER NOT NULL,
			relation_type TEXT NOT NULL,
			confidence REAL DEFAULT 1.0,
			status TEXT DEFAULT 'active',
			session_id TEXT,
			turn_id INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			date_bucket TEXT,
			sentence_id INTEGER DEFAULT 0,
			sentence_ref TEXT DEFAULT '',
			FOREIGN KEY (source_id) REFERENCES entities(id),
			FOREIGN KEY (target_id) REFERENCES entities(id),
			UNIQUE(source_id, target_id, relation_type, session_id)
		)`,
		`INSERT OR IGNORE INTO relations (id, source_id, target_id, relation_type, confidence, status, session_id, turn_id, created_at, updated_at, date_bucket, sentence_id, sentence_ref)
		 SELECT id, source_id, target_id, relation_type, confidence, status, session_id, turn_id, created_at, updated_at, date_bucket, sentence_id, sentence_ref FROM relations_old`,
		`DROP TABLE relations_old`,
	}
	for _, s := range stmt {
		if _, err := tx.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// Path 返回本库的存储路径（父 agent 用它为驻留子打开**受限句柄**）。
func (g *GraphDB) Path() string { return g.dbPath }

// Commit 把三元组写入图库，返回通过实体名校验并写入/刷新的实体数与**新建**的关系数。
//
// 两个计数的语义刻意不同，因为上游只用它们判断「有没有东西写进去」：
// 实体计数含已存在实体的 mention_count 刷新（见 upsertEntity），
// 关系计数只统计真正新建的关系（已存在则仅刷新 confidence）。
func (g *GraphDB) Commit(triples []Triple, sessionID string, turnID int) (int, int, error) {
	_, ec, rc, err := g.commit(triples, sessionID, turnID, false)
	return ec, rc, err
}

// CommitWithMedia 与 Commit 相同，但额外返回每条句子文本对应的 sentences.id。
//
// 为何单独开一个方法而不改 Commit 的签名：Commit 有十个非测试调用点
// 加二十多个测试调用点，为了一个多数调用方都不需要的返回值去改全部签名
// 不划算。这里让 Commit 内部转调，两者共享同一份落库逻辑。
//
// 返回的 map 只包含本次真正写入了 sentences 表的句子。调用方据此把媒体
// 变成 L3 一等块，并以 sentence --contains--> block 边与句子相连；
// 关系行本身不持有媒体。
// ★ 返回的 map 现在是「原句文本 → **原句块 ID**」（原为 sentences 表行号）。
//
// 变更原因：sentences 表退场后行号不再存在，而媒体桥
// （graphmedia.go commitTriplesWithMedia）要靠它挂
// 「原句块 --contains--> 媒体块」这条边。
//
// ⇒ 媒体桥那条边要同步改：source_kind 从 "sentence" 变成 "block"。
func (g *GraphDB) CommitWithMedia(triples []Triple, sessionID string, turnID int) (map[string]string, int, int, error) {
	return g.commit(triples, sessionID, turnID, true)
}

func (g *GraphDB) commit(triples []Triple, sessionID string, turnID int, trackSentences bool) (map[string]string, int, int, error) {
	sentenceIDs, ec, rc, written, created, err := g.commitLocked(triples, sessionID, turnID, trackSentences)
	// ★ 上报必须在 commitLocked **返回之后**（即 g.mu 已释放）。
	//   在 commitLocked 内部（持锁时）调钩子会自死锁 —— 钩子的订阅者
	//   会回调到内存系统，而那些路径要取同一把 g.mu。
	//
	// 只上报成功的写入：失败的 commit 什么都没落库，报出去会让星图
	// 高亮一批不存在的块。
	if err == nil {
		g.emitAccess(AccessEvent{Op: "commit", Blocks: written, Created: created})
	}
	return sentenceIDs, ec, rc, err
}

// commitLocked 是 commit 的实体（持 g.mu）。
//
// 拆出原因：外部上报需要在**锁释放后**发生，而 defer g.mu.Unlock()
// 只能在函数返回时生效 —— 把两者拆成两层是最直接的解法。
func (g *GraphDB) commitLocked(triples []Triple, sessionID string, turnID int, trackSentences bool) (map[string]string, int, int, []string, []string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	var sentenceIDs map[string]string
	if trackSentences {
		sentenceIDs = make(map[string]string)
	}

	// writtenIDs / createdIDs 收集本次写入与新建的块 ID，供**锁释放后**
	// 通过 emitAccess 上报给星图（不能在持锁时调钩子，见 GraphDB.onAccess）。
	var writtenIDs []string
	var createdIDs []string

	tx, err := g.db.Begin()
	if err != nil {
		return nil, 0, 0, nil, nil, err
	}
	defer tx.Rollback()

	entitiesCreated := 0
	relationsCreated := 0
	// dateBucket 原供旧表 relations.date_bucket —— 旧表停写后已无去处。
	dateBucket := time.Now().Format("2006-01-02")
	_ = dateBucket

	for _, t := range triples {
		if t.Subject == "" || t.Relation == "" || t.Object == "" {
			continue
		}
		if !validEntityName(t.Subject) || !validEntityName(t.Object) {
			continue
		}

		// ★ 块化写入：三元组 → 「主语块 --关系--> 宾语块」
		//
		// 为什么在 commit 里做（而不是让调用方各自块化）：
		// Commit 有四条调用路径（媒体桥 / memory_commit 工具 / 驻留子 /
		// 记忆整理流水线），实测只有 memory_commit 工具在活跃写库。
		// 若各路径自己块化，必然出现「有的路径写块、有的不写」——
		// 那正是「旧表在长大」（跑分实测 entities 188 → 381）的机制。
		//
		// ★ 块 ID 由内容派生 ⇒ 重复提交同一三元组得到同一个块，天然幂等。
		//
		// ★ 旧表写入**暂时保留**：Recall 等 55 处调用方仍读它们
		//   （见 docs/zh/legacy-table-retirement.md），先删会打断在线读取。
		//   退场顺序是「读方先切块 → 再停写旧表 → 最后删表」。
		// ★ 原句块 ID：在原句分支里赋值，场景挂载与 contains 边要用。
		//   （声明提前到循环开头，否则它只在 SentenceText != "" 的分支里可见）
		var sentenceBlockID string

		srcBlockID, dstBlockID, edgeID, tripletCreated, err := putTripleBlocksTx(tx, t, sessionID, turnID)
		if err != nil {
			return nil, 0, 0, nil, nil, fmt.Errorf("block triple %s/%s: %w",
				t.Subject, t.Relation, err)
		}
		writtenIDs = append(writtenIDs, srcBlockID, dstBlockID)
		createdIDs = append(createdIDs, tripletCreated...)

		subjType := t.SubjectType
		if subjType == "" {
			subjType = "Concept"
		}
		objType := t.ObjectType
		if objType == "" {
			objType = "Concept"
		}
		confidence := t.Confidence
		if confidence == 0 {
			confidence = 1.0
		}

		// ★★ 旧表 entities 双写已停（2026-10-04）
		//
		// 读方已全部切块，而 entities.type / mention_count 已无处可取
		// （块侧改用 semantic_type）。
		//
		// ★ 代价要说清楚：**entitiesCreated 现在恒为 0，没有意义了**。
		//   Commit 的返回签名 (entityCount, relationCount, error) 是
		//   SDK 契约（9 处调用方），改签名会波及一大片 ——
		//   所以保留位置并置 0，而不是改签名。
		//   依赖它做判断的调用方（若存在）应改用块侧计数，
		//   已记入 docs/zh/legacy-table-retirement.md 的待办。
		_ = subjType
		_ = objType

		// ★★ 旧表 entities 的**读取**也已停（2026-10-04）
		//
		// 这两条 SELECT 只为拿旧表行号（sourceID/targetID），
		// 而它们唯一的用途是写旧表 relations。旧表不写之后，
		// 查询本身成了纯开销 —— 且在旧表被删后会直接报错。
		//
		// ★ 注意它们**必须**先于句块写入被摘掉：
		//   旧表退场后 `SELECT id FROM entities WHERE name=?`
		//   会返回 no rows ⇒ Commit 直接失败 ⇒ **记忆完全写不进去**。
		//   这是「停双写」时最容易漏的一环：读也要停。

		// 写入/查找句子（★ 现在只建块，不写 sentences 表）
		if t.SentenceText != "" {
			// ★ 原句块：sentences 表退场后，原句由块承载。
			//
			// 它必须在**任何分支之外**建 —— 否则「sentence 行插入失败」
			// 会连带丢掉原句块，而那句原句本身是有效记忆。
			// 判据 zz_blockcommit_test.go 的②盯的就是这条。
			// ★ 同样留空时间：原句块的时序由迁移/蒸馏显式指定，
			//   这里写 now 会覆盖既有值（实测抹平了迁移块的时序）。
			//
			// ★ 用返回值区分「新建」与「更新」：只有真正新建的块才驱动星图的
			//   「生长」动画。该句被后续引用时走 UPDATE，不算新块——
			//   若也播生长，同一句话每次被提及都会再长一次，看着像新记忆。
			createdSentenceBlock, err := putBlockTx(tx, NewSentenceBlock(t.SentenceText,
				time.Time{}, time.Time{}))
			if err != nil {
				return nil, 0, 0, nil, nil, fmt.Errorf("put sentence block: %w", err)
			}
			if createdSentenceBlock {
				createdIDs = append(createdIDs, SentenceBlockID(t.SentenceText))
			}
			sentenceBlockID = SentenceBlockID(t.SentenceText)
			if sentenceIDs != nil {
				// ★ 返回原句块 ID 而非 sentences 行号（见 CommitWithMedia 注释）
				sentenceIDs[t.SentenceText] = sentenceBlockID
			}

			// ★★ 旧表 sentences 双写已停（2026-10-04）
			//
			// 原句改由**原句块**承载（上方 putBlockTx），
			// 场景引用已改用块 ID（13c3292），不再需要 sentences.id。
			// 保留旧表写入只会让它持续增长，退场就永远看不到完成信号。
		}

		// ★★ 旧表 relations 双写已停（2026-10-04）
		//
		// 关系改由**关系边**承载，场景引用已改用 edge ID。
		// 读方已全部切块（scene / Purge / RecallSorted / Introspect /
		// MergeBlocks / social / sdk / core），旧表只增不减会让
		// ① 退场永远看不到完成信号 ② 迁移报告的「旧表还剩多少」永不归零。
		//
		// ★ 判断依据是**行为**不是语句：判据 Test停双写_旧表不再增长
		//   逐表比对 Commit 前后的行数。
		relationsCreated = 0

		// 场景引用：写完关系立即把「关系 + 两端块」挂到**每个**场景上
		// （主动声明的 + 被动涌现的）。同一事务内完成，避免出现「关系写进去了
		// 但场景引用丢了」——那会让这条记忆在后来的场景里永远召不回来，且无声无息。
		//
		// ★★ 端点已改为**块**（2026-10-04）
		//
		// 旧实现传 relationID（relations.id）与两个 entityID，
		// 于是 Commit 每写一条关系就在 scene_refs 里生成
		// kind='relation' + kind='entity' 的引用 ——
		// 而 entities/relations 退场后这些引用全部悬空，
		// 且场景式记忆会指向不存在的对象。
		//
		// 现在传块 ID：两端 kind='block'，关系边本身 kind='edge'。
		if srcBlockID != "" || dstBlockID != "" {
			// ★★ 原句回溯：原句块 --contains--> 关系边
			//
			// 旧表靠 relations.sentence_id 外键取原句。sentences 退场后
			// 这个外键没有了 —— 而**带条件的规则本体长在句子里**，
			// 只给关系名等于没召回（判据「场景召回必须带原句」盯的就是这条）。
			//
			// ★ 方向：原句块 --contains--> 关系边。
			//   从原句出发能顺着 contains 找到关系，从关系能逆向找到原句
			//   （BFS 是双向的）。反向（原句块→关系）才是对的：
			//   「这句里说了什么」是块→边的方向。
			//
			// ★ 含时间才能做时序仲裁。
			if sentenceBlockID != "" && edgeID != 0 {
				if err := addContainsEdgeTx(tx, sentenceBlockID, edgeID,
					confidence); err != nil {
					return nil, 0, 0, nil, nil, fmt.Errorf("contains %s->%d: %w",
						sentenceBlockID, edgeID, err)
				}
			}
			for _, sc := range effectiveScenes(t) {
				if err := tagSceneTripleTx(tx, sc, srcBlockID, dstBlockID,
					edgeID, confidence); err != nil {
					return nil, 0, 0, nil, nil, err
				}
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, 0, 0, nil, nil, err
	}

	return sentenceIDs, entitiesCreated, relationsCreated, writtenIDs, createdIDs, nil
}

func validEntityName(name string) bool {
	if name == "" {
		return false
	}
	r := []rune(name)
	if len(r) < 2 || len(r) > 50 {
		return false
	}
	// 纯数字串是合法实体值：端口/分机/编号这类属性值的本体就是数字。
	// 此前把它拒掉导致 commit 静默丢弃（0 写入假成功回执，实测
	// 2026-10-01 跑分：metrics 端口 8328 / 分机 4379→4324 都因此丢失）。
	// 但仍禁纯标点/空白串。
	hasLetterOrDigit := false
	for _, ch := range r {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '\u4e00' && ch <= '\u9fff') || ch == '-' || ch == '_' || (ch >= '0' && ch <= '9') {
			hasLetterOrDigit = true
		}
	}
	return hasLetterOrDigit
}

// upsertEntity 写入/刷新一个实体，返回 1 表示该实体**通过名校验并被写入或刷新**，
// 0 表示名校验未通过。
//
// 注意返回值语义不是「新建数」：`ON CONFLICT DO UPDATE` 在更新时
// RowsAffected 同样为 1，所以返回值等于「通过校验的 upsert 次数」。
// 调用方（Commit）把它当「写入了几个实体」用，不是「新建了几个」。
func (g *GraphDB) upsertEntity(tx *sql.Tx, name string, entityType string) (int, error) {
	if !validEntityName(name) {
		return 0, nil
	}
	result, err := tx.Exec(
		`INSERT INTO entities (name, type) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		 	mention_count = mention_count + 1,
		 	updated_at = CURRENT_TIMESTAMP`,
		name, entityType,
	)
	if err != nil {
		return 0, err
	}
	rows, _ := result.RowsAffected()
	if rows > 0 {
		return 1, nil
	}
	return 0, nil
}

type RecallResult struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

// SortMode 决定召回结果的呈现顺序。
type SortMode string

const (
	// SortRelevance：按「实词命中层级 → 提及次数 → 名字长度」排序。
	// 回答「某个具体东西是什么/是多少」用这个 —— 实词精确命中最可信。
	SortRelevance SortMode = "relevance"
	// SortRecent：按更新时间倒序，同时间按提及次数。
	// 回答「最近/最新/现在是什么」用这个 —— 运维场景里答案常常是
	// 「新值覆盖旧值」（实测 v4 的 overwrite 组就是考这个），
	// 相关性排序会把旧的同名实体排在新值前面。
	SortRecent SortMode = "recent"
)

// ParseSortMode 解析排序模式；空或无法识别时返回默认（相关性）。
//
// 刻意不接受任何"看起来像"的字符串：这里只服务显式入参，
// 认错会让调用方以为自己按时间排序了、实际拿到相关性顺序。
func ParseSortMode(s string) SortMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "recent", "time", "newest":
		return SortRecent
	default:
		return SortRelevance
	}
}

// Recall 保持原签名（相关性排序），委托给 RecallSorted。
func (g *GraphDB) Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*RecallResult, error) {
	// ★ 不传 fingerprint：Recall 是**库层**接口（social / indexer / SDK 都用），
	//   它不知道当前向量空间是什么 —— 那是 core 层的关注点。
	//   需要空间隔离的路径（memory_recall 工具）走 RecallSorted 并显式传。
	return g.RecallSorted(keywords, seedEntities, depth, sessionFilter, "", SortRelevance)
}

// RecallSorted 是可指定呈现顺序的召回。
//
// 截断在**出口**做（深度扩展之后），不是深度扩展之前 —— 扩展会从种子实体
// 带出新的邻居实体，扩展前截断会让总量再次越界（实测 47 > 20）。
// ★ fingerprint 是**向量空间隔离键**（2026-10-04）：
//
//	非空时只返回 fingerprint 匹配的块。
//
//	RecallSorted 是**纯词法**召回（不碰向量），但它不能因此绕过隔离 ——
//	它是 recallByBlocks 失败后的兜底路，一旦 fallback 到它，
//	「换向量空间后旧块不能污染结果」这条约束就被绕过了。
//
//	判据 TestMemoryRecall_指纹不匹配的块被跳过 当场抓到这一点：
//	块路因无空间被跳过 → 走兜底 → 兜底把旧空间的块原样返回。
func (g *GraphDB) RecallSorted(keywords []string, seedEntities []string, depth int, sessionFilter, fingerprint string, mode SortMode) (*RecallResult, error) {
	result, err := g.recallSortedLocked(keywords, seedEntities, depth, sessionFilter, fingerprint, mode)
	// ★ 上报必须在锁释放**之后**（见 GraphDB.onAccess 的死锁说明）。
	//   读事件驱动星图的「被查询提亮」：这些块就是本次召回真正取回的。
	if err == nil && result != nil {
		seen := make(map[string]struct{}, len(result.Entities))
		ids := make([]string, 0, len(result.Entities))
		for _, e := range result.Entities {
			// 只报有块键的实体：旧实现下部分实体来自非块路径（无 blockKey），
			// 报出去星图也找不到对应节点。
			if e.blockKey == "" {
				continue
			}
			if _, dup := seen[e.blockKey]; dup {
				continue
			}
			seen[e.blockKey] = struct{}{}
			ids = append(ids, e.blockKey)
		}
		g.emitAccess(AccessEvent{Op: "recall", Blocks: ids})
	}
	return result, err
}

// recallSortedLocked 是 RecallSorted 的实体（持 g.mu）。
func (g *GraphDB) recallSortedLocked(keywords []string, seedEntities []string, depth int, sessionFilter, fingerprint string, mode SortMode) (*RecallResult, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	result := &RecallResult{}

	if len(keywords) == 0 && len(seedEntities) == 0 {
		// 全量读取仅用于内部整备（Indexer.Sync / 实体合并检测），
		// 必须加限额：无 LIMIT 时大图会被整表读进内存。
		// ★★★ 改走块/边体系（2026-10-04）
		//
		// 旧实现读 entities + relations。旧表退场后这里返回空 ——
		// 而这条路径是 Indexer.Sync 的数据源（建向量索引）与
		// 实体合并检测的唯一入口。它空了，整备就是空转：
		// 索引里一条向量都没有，而没有任何报错。
		//
		// 块侧口径：按 created_at **升序**（旧的是 mention_count DESC 的逆序，
		// 但全量读取只用于建索引，顺序不影响正确性；命中上限时
		// 优先取最早的，因为它们更可能是稳定的基础事实）。
		rows, err := g.db.Query(
			`SELECT `+blockColumns+` FROM memory_blocks
			 WHERE text_content != '' AND (? = '' OR fingerprint = ?)
			 ORDER BY created_at ASC, id ASC LIMIT ?`,
			fingerprint, fingerprint, maxFullRecallEntities,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()

		var seq int64
		for rows.Next() {
			b, err := scanBlockRow(rows)
			if err != nil {
				return nil, err
			}
			seq++
			result.Entities = append(result.Entities, Entity{
				ID:        seq, // 占位：块 ID 是字符串，Entity.ID 是 int64
				Name:      b.Text,
				Type:      blockSemanticType(b),
				CreatedAt: b.CreatedAt,
				UpdatedAt: b.UpdatedAt,
			})
		}
		if len(result.Entities) >= maxFullRecallEntities {
			log.Printf("[graph] full recall 命中实体上限 %d，可能有实体未纳入", maxFullRecallEntities)
		}

		relRows, err := g.db.Query(
			`SELECT e.id, e.source_id, e.target_id, sb.text_content, tb.text_content,
					e.edge_type, COALESCE(e.confidence, 0), COALESCE(e.status, ''),
					COALESCE(e.session_id, ''), COALESCE(e.turn_id, 0), e.created_at,
					COALESCE((SELECT b.text_content FROM memory_block_edges c
					          JOIN memory_blocks b ON b.id = c.source_id
					          WHERE c.target_kind = 'edge' AND c.target_id = e.id
					            AND c.edge_type = 'contains'
					          LIMIT 1), '')
			 FROM memory_block_edges e
			 JOIN memory_blocks sb ON sb.id = e.source_id
			 JOIN memory_blocks tb ON tb.id = e.target_id
			 WHERE e.source_kind = 'block' AND e.target_kind = 'block'
				  AND COALESCE(e.status, '') = 'active'
			 ORDER BY e.created_at DESC LIMIT 30`,
		)
		if err != nil {
			return nil, err
		}
		defer relRows.Close()
		for relRows.Next() {
			var rel Relation
			if err := relRows.Scan(&rel.ID, &rel.SourceBlockID, &rel.TargetBlockID,
				&rel.SourceName, &rel.TargetName, &rel.RelationType,
				&rel.Confidence, &rel.Status, &rel.SessionID,
				&rel.TurnID, &rel.CreatedAt, &rel.SentenceText); err != nil {
				return nil, err
			}
			result.Relations = append(result.Relations, rel)
		}

		return result, nil
	}

	// ══════════════════════════════════════════════════════════
	//  第二分支：关键词 / 种子实体 —— 已改块/边体系（2026-10-04）
	//
	//  ★ 这一整块是「int64 实体 ID → 旧表查询」的闭环：
	//      查 entities 拿 ID → 用 ID 查 relations → 再用关系两端 ID 查 entities。
	//    块体系里端点是**字符串块 ID**，所以 entityIDs 换成 map[string]bool，
	//    三处查询全部重写。
	//
	//  ★ 它服务的调用方（全部经由 db.Recall）：
	//      social（人物/特质/关系）  ← 本次改动的直接受害者
	//      indexer（全量，已在前一分支改完）
	//      light_memory（透传）
	//      toolcall（旧路兜底）
	//
	//  ★★ 这次改动的实测后果（social 3 个测试当场变红）：
	//      写侧（Purge）已切块、读侧（这里）还没切 ⇒ 软删的边在旧表里
	//      仍是 active ⇒ 召回照样返回它。**混合态比全旧态更危险**，
	//      因为它看起来是「部分成功」。
	// ══════════════════════════════════════════════════════════

	// entityIDs 的键从 int64 变成**块 ID 字符串**。
	var bScratch MemoryBlock
	var vecJSON string
	var sceneNull sql.NullString

	entityIDs := make(map[string]bool)
	// entityRank 记「三层精确度」：完全相等 > 前缀命中 > 包含命中。
	entityRank := make(map[string]int)
	seq := 0

	for _, kw := range keywords {
		if kw == "" {
			continue
		}
		// ★ mention_count 已无处可取（旧表才有），用 created_at 兜底排序：
		//   先出现的块更可能是稳定的基础事实。
		// ★ rank 放在 SELECT 的**第一列**，而不是最后。
		//
		//   之前放在 blockColumns 之后 ⇒ scanBlockRow 只吃 15 列，
		//   多出来的那列无人扫 ⇒ 「expected 16 destination arguments in Scan,
		//   not 15」。这类错只在真跑 SQL 时暴露，编译期完全看不出来。
		rows, err := g.db.Query(
			`SELECT CASE
			          WHEN LOWER(text_content) = LOWER(?) THEN 0
			          WHEN LOWER(text_content) LIKE LOWER(?) || '%' THEN 1
			          ELSE 2 END, `+blockColumns+`
			 FROM memory_blocks
			 WHERE text_content != '' AND LOWER(text_content) LIKE ?
			   AND (? = '' OR fingerprint = ?)
			 ORDER BY CASE
			         WHEN LOWER(text_content) = LOWER(?) THEN 0
			         WHEN LOWER(text_content) LIKE LOWER(?) || '%' THEN 1
			         ELSE 2 END,
			       created_at DESC, LENGTH(text_content) ASC
			 LIMIT ?`,
			kw, kw, "%"+strings.ToLower(kw)+"%",
			fingerprint, fingerprint, kw, kw, maxKeywordEntities,
		)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var rank int
			// rank 是第 1 列，先扫掉，剩下 15 列正好对上 scanBlockRow。
			if err := rows.Scan(&rank, &bScratch.ID, &bScratch.Modality,
				&bScratch.Text, &bScratch.MIME,
				&bScratch.Size, &bScratch.Width, &bScratch.Height, &vecJSON,
				&bScratch.Fingerprint, &bScratch.Source, &bScratch.Tool,
				&sceneNull, &bScratch.SemanticType,
				&bScratch.CreatedAt, &bScratch.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			b := bScratch
			seq++
			e := Entity{
				ID:        int64(seq),
				Name:      b.Text,
				Type:      blockSemanticType(b),
				CreatedAt: b.CreatedAt,
				UpdatedAt: b.UpdatedAt,
			}
			if prev, ok := entityRank[b.ID]; !ok || rank < prev {
				entityRank[b.ID] = rank
			}
			if !entityIDs[b.ID] {
				entityIDs[b.ID] = true
				e.blockKey = b.ID
				result.Entities = append(result.Entities, e)
			}
		}
		rows.Close()
	}

	// 种子实体：按名**精确**命中（不区分大小写，与旧实现一致）。
	for _, se := range seedEntities {
		if se == "" {
			continue
		}
		rows, err := g.db.Query(
			`SELECT `+blockColumns+` FROM memory_blocks
			 WHERE text_content = ? AND (? = '' OR fingerprint = ?) LIMIT 1`,
			se, fingerprint, fingerprint)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			b, err := scanBlockRow(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			if !entityIDs[b.ID] {
				seq++
				entityIDs[b.ID] = true
				entityRank[b.ID] = 0
				result.Entities = append(result.Entities, Entity{
					ID:        int64(seq),
					Name:      b.Text,
					Type:      blockSemanticType(b),
					CreatedAt: b.CreatedAt,
					UpdatedAt: b.UpdatedAt,
					blockKey:  b.ID,
				})
			}
		}
		rows.Close()
	}

	if len(entityIDs) == 0 {
		return result, nil
	}

	// ★ 全局上限：跨关键词累加后按「三层精确度」截断。
	//
	// 单个关键词的 LIMIT(maxKeywordEntities) 管不住总量 —— 多个关键词各召回
	// 一批会累加，实测 3 个关键词就能堆到 193 个实体（13744 tokens）。
	// 这里按三层精确度（完全相等 > 前缀命中 > 包含命中）跨关键词归并后截断。

	seenRel := make(map[int64]bool)

	// ★★ 深度扩展
	//
	// 旧实现每层用「已累积的 entityIDs」查邻接关系 ——
	// ★★ 那个设计是错的：entityIDs 累积的是**实体**，
	//    而一层扩展的正确语义是「上一层新发现的实体的邻居」。
	//    用累积集会让第 2 层把第 0 层见过的实体再查一遍，
	//    于是深度形同虚设（判据 TestRecallSorted_深度扩展 抓到：
	//    depth=2 拿不到「丙」，而甲→乙→丙→丁 是 3 跳链）。
	//
	// 现在：frontier = 上一层新发现的块；每层只从 frontier 扩展。
	// prevFrontier 是本层的扩展起点；每轮结束时被 newIDs 替换。
	prevFrontier := entityIDs
	for depthLevel := 0; depthLevel < depth; depthLevel++ {
		if len(prevFrontier) == 0 {
			break
		}
		// ★ 起点集合必须是「**本层之前**新发现的块」，不是 entityIDs 全集。
		//
		// 我先写成 entityIDs（累积集），那等于「每层从所有已知点重新扩一遍」——
		// 深度形同虚设，而且第一层就把 2 跳邻居全拉进来了。
		//
		// prevFrontier 由上一轮的 newIDs 赋值；depthLevel==0 时用种子命中集。
		frontier := prevFrontier

		ids := make([]interface{}, 0, len(frontier))
		for id := range frontier {
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			break
		}

		ph := placeholders(len(ids))
		relQuery := fmt.Sprintf(
			`SELECT e.id, e.source_id, e.target_id, sb.text_content, tb.text_content,
			        e.edge_type, COALESCE(e.confidence,0), COALESCE(e.status,''),
			        COALESCE(e.session_id,''), COALESCE(e.turn_id,0), e.created_at,
			        COALESCE((SELECT b.text_content FROM memory_block_edges c
			                  JOIN memory_blocks b ON b.id = c.source_id
			                  WHERE c.target_kind='edge' AND c.target_id = e.id
			                    AND c.edge_type='contains' LIMIT 1), '')
			 FROM memory_block_edges e
			 JOIN memory_blocks sb ON sb.id = e.source_id
			 JOIN memory_blocks tb ON tb.id = e.target_id
			 WHERE (e.source_id IN (%s) OR e.target_id IN (%s))
			   AND e.source_kind='block' AND e.target_kind='block'
			   AND e.edge_type != 'contains'
			   AND COALESCE(e.status,'') = 'active'`, ph, ph)
		args := append(append([]interface{}{}, ids...), ids...)
		if sessionFilter != "" {
			relQuery += " AND COALESCE(e.session_id,'') = ?"
			args = append(args, sessionFilter)
		}
		// ★★ 排序口径必须交给 sortRecallRelations，不能在 SQL 里预设（2026-10-04）
		//
		//   我写了 `ORDER BY e.created_at DESC`，看起来无害 ——
		//   实际上它让**两种 SortMode 的结果完全相同**：
		//   sortRecallRelations 对 SortRelevance 直接 return（不排序），
		//   于是「相关性」模式实际拿到的是「时间倒序」。
		//
		//   判据 TestRecall_时间倒序_变异_相关性模式顺序不同 当场抓住：
		//   它是**变异自证** —— 断言「相关性模式下顺序必须不同于时间模式」，
		//   一旦两者相同就说明上一个测试的判据不可信。
		//
		// ★★ SortRelevance 需要自己的顺序依据（2026-10-04）
		//
		//   sortRecallRelations 对 SortRelevance 直接 return —— 前提是
		//   「SQL 已按种子实体的相关性排过」。
		//
		// ★ 那个前提在我改写时不成立：无 ORDER BY 时 SQLite 按 rowid 走，
		//   而 rowid = 插入顺序 = 时间顺序 ⇒
		//   相关性模式实际拿到的是时间倒序，两种 SortMode 完全同序。
		//
		//   判据（变异自证）当场抓住：它断言「相关性模式下顺序必须不同于
		//   时间模式」，两者一相同就说明上一个测试的判据不可信。
		//
		//   相关性的可用信号只有 confidence（边自带），用它兜底：
		//   同分时保持插入序，于是「高置信在前」可区分于「新的在前」。
		if mode != SortRecent {
			relQuery += " ORDER BY e.confidence DESC, e.id ASC"
		} else {
			relQuery += " ORDER BY e.created_at DESC, e.turn_id DESC, e.id DESC"
		}

		relRows, err := g.db.Query(relQuery, args...)
		if err != nil {
			return nil, err
		}

		newIDs := make(map[string]bool)
		for relRows.Next() {
			var rel Relation
			if err := relRows.Scan(&rel.ID, &rel.SourceBlockID, &rel.TargetBlockID,
				&rel.SourceName, &rel.TargetName, &rel.RelationType,
				&rel.Confidence, &rel.Status, &rel.SessionID,
				&rel.TurnID, &rel.CreatedAt, &rel.SentenceText); err != nil {
				relRows.Close()
				return nil, err
			}
			if !seenRel[rel.ID] {
				seenRel[rel.ID] = true
				result.Relations = append(result.Relations, rel)
			}
			if !entityIDs[rel.SourceBlockID] {
				newIDs[rel.SourceBlockID] = true
			}
			if !entityIDs[rel.TargetBlockID] {
				newIDs[rel.TargetBlockID] = true
			}
		}
		relRows.Close()

		if len(newIDs) == 0 {
			break
		}
		// ★ 下一层从这批新块出发 —— 这是「深度」真正生效的地方。
		prevFrontier = newIDs

		// 取出新实体的块内容 —— 块 ID 就是主键，直接查一次。
		ids2 := make([]interface{}, 0, len(newIDs))
		for id := range newIDs {
			ids2 = append(ids2, id)
		}
		eRows, err := g.db.Query(
			fmt.Sprintf(`SELECT `+blockColumns+` FROM memory_blocks WHERE id IN (%s)`,
				placeholders(len(ids2))),
			ids2...,
		)
		if err != nil {
			return nil, err
		}
		for eRows.Next() {
			b, err := scanBlockRow(eRows)
			if err != nil {
				eRows.Close()
				return nil, err
			}
			if !entityIDs[b.ID] {
				seq++
				entityIDs[b.ID] = true
				entityRank[b.ID] = 3 // 深度扩展来的，精度最低
				result.Entities = append(result.Entities, Entity{
					ID:        int64(seq),
					Name:      b.Text,
					Type:      blockSemanticType(b),
					CreatedAt: b.CreatedAt,
					UpdatedAt: b.UpdatedAt,
					blockKey:  b.ID,
				})
			}
		}
		eRows.Close()
	}

	// ★ 出口排序 + 全局截断（在深度扩展之后）。
	//
	// 深度扩展会从种子实体带出新的邻居实体，所以上限必须在出口施加。
	// ★ entityRank 的键要从块 ID 映射回 Entity.ID：
	//   sortRecallEntities 按 ents[i].ID 建索引，而 Entity.ID 现在是
	//   「本次召回内的序号」（块 ID 是字符串，放不进 int64）。
	//   序号在本次调用内唯一，所以映射是一一对应的。
	rankBySeq := make(map[int64]int, len(result.Entities))
	for i := range result.Entities {
		if r, ok := entityRank[result.Entities[i].blockKey]; ok {
			rankBySeq[result.Entities[i].ID] = r
		}
	}
	sortRecallEntities(result.Entities, rankBySeq, keywords, mode)
	if len(result.Entities) > maxRecallEntities {
		dropped := len(result.Entities) - maxRecallEntities
		result.Entities = result.Entities[:maxRecallEntities]
		log.Printf("[graph] recall: 截断 %d 个实体（全局上限 %d）", dropped, maxRecallEntities)
	}
	sortRecallRelations(result.Relations, mode)

	return result, nil
}

// ExportTriples 导出库中的**活跃**三元组（供父 agent 在回收阶段收割子的 temp）。
//
// 设计 docs/zh/resident-subagent-design.md §9（回收 = 父读 temp → 选记录 → 合入 main）。
// 只导出 `status='active'` 的关系，并把实体名一并带出（Relation 已含 SourceName/TargetName），
// 于是合入侧可以直接复用 Commit —— 它按实体名 upsert、按
// (source, relation, target) 幂等，因此"重复收割"不会造成重复条目。
//
// limit <= 0 表示不限量。
func (g *GraphDB) ExportTriples(limit int) ([]Triple, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	// ★★★ 改走块/边体系（2026-10-04）
	//
	// 原实现读 relations JOIN entities。旧表停双写后这两张表**不再增长**，
	// 而子 agent 的 temp 库恰恰是靠 Commit 写入的 ——
	// ⇒ ExportTriples 返回空 ⇒ 回收（ReclaimResident）合入 0 条
	// ⇒ **驻留子 agent 的记忆回收功能静默失效**。
	//
	// 判据：TestResident_ContextFullAndDispositions ④
	// 「回收应把选中的 temp 记录合入主记忆」当场变红。
	//
	// ★ 这也说明「零调用点的读方」不等于「不重要的读方」：
	//   ExportTriples 只被 resident.go 用，而 resident 是一条完整产品线。
	q := `SELECT sb.text_content, e.edge_type, tb.text_content,
	             COALESCE(e.confidence, 0),
	             COALESCE((SELECT b.text_content FROM memory_block_edges c
	                         JOIN memory_blocks b ON b.id = c.source_id
	                         WHERE c.target_kind = 'edge' AND c.target_id = e.id
	                           AND c.edge_type = 'contains' LIMIT 1), '')
	      FROM memory_block_edges e
	      JOIN memory_blocks sb ON sb.id = e.source_id
	      JOIN memory_blocks tb ON tb.id = e.target_id
	     WHERE e.source_kind = 'block' AND e.target_kind = 'block'
	       AND e.edge_type != 'contains'
	       AND COALESCE(e.status, '') = 'active'
	     ORDER BY e.id`
	args := []interface{}{}
	if limit > 0 {
		q += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := g.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Triple
	for rows.Next() {
		var tr Triple
		if err := rows.Scan(&tr.Subject, &tr.Relation, &tr.Object,
			&tr.Confidence, &tr.SentenceText); err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, rows.Err()
}

func (g *GraphDB) Purge(criteria map[string]string, mode string) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// ★★★ 改走块/边体系（2026-10-04）
	//
	// 旧实现查 entities 拿 id、拼 relations 的 where、删/软删 relations，
	// 最后 purgeStaleSceneRefsLocked。旧表退场后这条路全断 ——
	// 而 Purge 有 9 处调用方（webui / healthcheck / SDK / toolcall×2 /
	// social×2 / lua / proc），它是**记忆删除的主路径**：
	// 用户说「忘掉这条」而它删不动，等于没有删除功能。
	//
	// 块体系里的等价物：
	//
	//	subject_contains → 按块文本 LIKE 找源块 ID
	//	target_contains  → 按块文本 LIKE 找目标块 ID
	//	relation_type    → edge_type
	//	session_id       → session_id
	//
	// ★ 端点是**块 ID 字符串**，所以 IN 列表的参数类型从 int64 变 string。
	// ★ status 口径：软删写 'deleted'；硬删直接 DELETE。
	//   两者都紧跟 purgeStaleSceneRefsLocked —— 否则场景里会挂一条
	//   永远召不回的幽灵，而 SceneStats 还照样把它算进去。

	if _, ok := criteria["subject_contains"]; !ok {
		if _, ok := criteria["target_contains"]; !ok {
			if _, ok := criteria["relation_type"]; !ok {
				if _, ok := criteria["session_id"]; !ok {
					return 0, fmt.Errorf("no criteria provided")
				}
			}
		}
	}

	// 收集要匹配的端点块 ID（可能同时来自 subject 与 target）
	var endpointConds []string
	var endpointArgs []interface{}

	for _, spec := range []struct{ key, col string }{
		{"subject_contains", "source_id"},
		{"target_contains", "target_id"},
	} {
		v, ok := criteria[spec.key]
		if !ok {
			continue
		}
		blocks, err := g.blocksByTextLikeTx(v)
		if err != nil {
			return 0, err
		}
		if len(blocks) == 0 {
			// ★ 匹配不到块 ⇒ 没有任何边可删。
			//   返回 0 而不是构造恒假条件 —— 后者会让「删 0 条」
			//   与「条件写错了」在返回值上无法区分。
			return 0, nil
		}
		ids := make([]interface{}, 0, len(blocks))
		for _, b := range blocks {
			ids = append(ids, b.ID)
		}
		endpointConds = append(endpointConds,
			fmt.Sprintf("%s IN (%s)", spec.col, placeholders(len(ids))))
		endpointArgs = append(endpointArgs, ids...)
	}

	conds := []string{"source_kind = 'block'", "target_kind = 'block'"}
	args := []interface{}{}
	conds = append(conds, endpointConds...)
	args = append(args, endpointArgs...)

	if v, ok := criteria["relation_type"]; ok {
		conds = append(conds, "edge_type = ?")
		args = append(args, v)
	}
	if v, ok := criteria["session_id"]; ok {
		conds = append(conds, "COALESCE(session_id, '') = ?")
		args = append(args, v)
	}

	// ★ 只有「仅按 session/relation_type 删」时不限定 status；
	//   带端点条件时也限定 active —— 否则重复调用会反复命中已软删的行，
	//   计数虚高而实际什么都没删。
	if _, hasEndpoint := criteria["subject_contains"]; hasEndpoint {
		conds = append(conds, "COALESCE(status, '') != 'deleted'")
	} else if _, hasEndpoint := criteria["target_contains"]; hasEndpoint {
		conds = append(conds, "COALESCE(status, '') != 'deleted'")
	}

	where := strings.Join(conds, " AND ")

	if mode == "hard" {
		result, err := g.db.Exec(
			fmt.Sprintf(`DELETE FROM memory_block_edges WHERE %s`, where), args...)
		if err != nil {
			return 0, err
		}
		n, _ := result.RowsAffected()

		// ★ 这里**不再**顺手全局删孤儿块。
		//
		// 与旧实现同一条纪律：孤儿清理是独立意图，
		// 混进删除路径会让「删一条边」清掉全图的孤块。
		//
		// 边没了，它的场景引用必须跟着对齐：残留引用会让场景看着很大、
		// 召回却是空的（SceneStats 也跟着说谎）。
		g.purgeStaleSceneRefsLocked()

		return int(n), nil
	}

	// ★ 不能写 updated_at：memory_block_edges **没有**这一列
	//   （升格成独立边时只加了 confidence/session/turn/status/merged_into）。
	//   旧 relations 表有，所以这个字段是照抄过来的 ——
	//   一旦旧表删掉、这里不改，Purge 软删会直接报 no such column。
	result, err := g.db.Exec(
		fmt.Sprintf(`UPDATE memory_block_edges SET status = 'deleted' WHERE %s`, where),
		args...,
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	// 软删除也要摘掉场景引用：RecallByScene 只返回 status='active'，
	// 留着引用只会在场景里挂一条永远召不回的幽灵。
	g.purgeStaleSceneRefsLocked()
	return int(n), nil
}

// blocksByTextLikeTx 按文本子串取块（调用方已持锁）。
//
// 与 BlockTextsLike 同语义，供**已持锁**的删除路径复用 ——
// 持锁路径不能调 BlockTextsLike（它自己要拿锁）。
func (g *GraphDB) blocksByTextLikeTx(fragment string) ([]MemoryBlock, error) {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil, nil
	}
	rows, err := g.db.Query(
		`SELECT `+blockColumns+` FROM memory_blocks
		 WHERE text_content LIKE ? ESCAPE '\' ORDER BY id`,
		"%"+escapeLike(fragment)+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryBlock
	for rows.Next() {
		b, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (g *GraphDB) GraphData() (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(`SELECT id, name, type, mention_count, created_at, updated_at FROM entities ORDER BY mention_count DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type graphEntity struct {
		ID           int64     `json:"id"`
		Name         string    `json:"name"`
		Type         string    `json:"type"`
		MentionCount int       `json:"mention_count"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
	}
	var entities []graphEntity
	for rows.Next() {
		var e graphEntity
		if err := rows.Scan(&e.ID, &e.Name, &e.Type, &e.MentionCount, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		entities = append(entities, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rrows, err := g.db.Query(`SELECT r.id, r.source_id, r.target_id, r.relation_type, r.confidence, r.status, r.created_at, COALESCE(r.sentence_id, 0), COALESCE(s.text, '') FROM relations r LEFT JOIN sentences s ON r.sentence_id = s.id WHERE r.status = 'active' ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()

	type graphRelation struct {
		ID           int64     `json:"id"`
		SourceID     int64     `json:"source_id"`
		TargetID     int64     `json:"target_id"`
		RelationType string    `json:"relation_type"`
		Confidence   float64   `json:"confidence"`
		Status       string    `json:"status"`
		CreatedAt    time.Time `json:"created_at"`
		SentenceID   int64     `json:"sentence_id,omitempty"`
		SentenceText string    `json:"sentence_text,omitempty"`
	}
	var relations []graphRelation
	for rrows.Next() {
		var r graphRelation
		if err := rrows.Scan(&r.ID, &r.SourceID, &r.TargetID, &r.RelationType, &r.Confidence, &r.Status, &r.CreatedAt, &r.SentenceID, &r.SentenceText); err != nil {
			return nil, err
		}
		relations = append(relations, r)
	}
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	brows, err := g.db.Query(`SELECT ` + blockColumns + `
		FROM memory_blocks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	var blocks []MemoryBlock
	for brows.Next() {
		var block MemoryBlock
		var vectorJSON string
		// ★ 这条 Scan 历史上就少扫了 scene（SELECT 用 blockConstants 改了，
		//   Scan 却没跟上）—— 与 semantic_type 是同一类错，
		//   报错形如「expected 16 destination arguments in Scan, not 14」。
		if err := brows.Scan(&block.ID, &block.Modality, &block.Text,
			&block.MIME, &block.Size, &block.Width, &block.Height, &vectorJSON,
			&block.Fingerprint, &block.Source, &block.Tool, &block.Scene,
			&block.SemanticType, &block.CreatedAt,
			&block.UpdatedAt); err != nil {
			return nil, err
		}
		if vectorJSON != "" && vectorJSON != "null" {
			if err := json.Unmarshal([]byte(vectorJSON), &block.Vector); err != nil {
				return nil, fmt.Errorf("decode memory block %s vector: %w", block.ID, err)
			}
		}
		blocks = append(blocks, block)
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}

	berows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
		edge_type, created_at FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer berows.Close()
	var blockEdges []MemoryBlockEdge
	for berows.Next() {
		var edge MemoryBlockEdge
		if err := berows.Scan(&edge.ID, &edge.SourceKind, &edge.SourceID,
			&edge.TargetKind, &edge.TargetID, &edge.Type, &edge.CreatedAt); err != nil {
			return nil, err
		}
		blockEdges = append(blockEdges, edge)
	}
	if err := berows.Err(); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"nodes":              entities,
		"edges":              relations,
		"memory_blocks":      blocks,
		"memory_block_edges": blockEdges,
	}, nil
}

// CountSentences 返回 sentences 表的行数（只读）。
//
// ★ 用途：方案 A 的退场判据
// ------------------------
// 蒸馏已不写该表（WritePayload 改用原句块承载原句），
// 而迁移仍会写它（MigrateLegacyTextEntities 建 sentence→block 边）。
// 所以「sentences 是否清零」是判断退场是否完成的可观察信号 ——
// 给它一个明确的只读接口，而不是让调用方猜 SQL 或翻 schema。
func (g *GraphDB) CountSentences() (int, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	err := g.db.QueryRow(`SELECT COUNT(*) FROM sentences`).Scan(&n)
	return n, err
}

func (g *GraphDB) Introspect() (map[string]interface{}, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var entityCount, relationCount int
	// ★★★ entity_count 必须数**块**而不是旧表 entities（2026-10-05）
	//
	// 旧表停双写 ⇒ 这句恒为 0 ⇒ Introspect 报告「库里没有记忆」
	// 而实际有几千条。实测（隔离实例，380 块）工具回：
	//
	//	记忆统计: map[entity_count:0 memory_hotspots:[map[count:15 name:order-gw ...]]
	//
	// ★ 同一个输出里 entity_count=0 而 hotspots 有真实度数 ——
	//   **半个旧表半个块表**，模型看到的是自相矛盾的统计，
	//   于是会得出「记忆是空的」而放弃写入。
	//
	// ★ 口径对齐下面两处（relation_count / hotspots 都已数块）。
	//
	// ★ 为什么不数全表块：块表含**原句块**（blk_src_*）与媒体块，
	//   它们不是「实体」。entity_count 的语义是端点块数
	//   （即三元组的主语/宾语），所以排除原句块与 contains 结构边。
	g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks
		 WHERE text_content != ''
		   AND COALESCE(modality,'') = 'text'`).Scan(&entityCount)
	// ★ relation_count 保持**活跃数**语义（status='active'）。
	//
	// 第一版为对齐迁移口径改成数全表（因为 MigrateLegacyTextEntities
	// 按 `ORDER BY id` 遍历全表、不按 status 过滤，而报告只数 active，
	// 导致「预计产出 块边 966」实际写入 980）。
	//
	// ★ 但那个修法是错的：relation_count 是**通用统计字段**，
	//   Purge("soft") 会把 status 置为 'deleted'，而 TestPurgeSoft
	//   断言软删除后 relation_count == 0。改成数全表后软删除失效，
	//   该测试失败 —— 它在我这次改动之前是绿的。
	//
	// 正确修法：**两处口径分开**。
	//   - Introspect 的 relation_count = 活跃数（语义不变，各调用方依赖）
	//   - 迁移报告要的是「会转换多少条」= 全表数，另开一个字段
	// ★★★ 改数块/边（2026-10-04）
	//
	// 旧表退场后这两句恒为 0 ⇒ Introspect 报告「库里没有记忆」
	// 而实际有几千条。它是 healthcheck 与 WebUI 状态页的数据源 ——
	// **报告说谎比报错更坏**（没人会去查）。
	//
	// ★ 口径对齐（沿用此前定的语义分离）：
	//   relation_count  = 活跃关系边（status='active'，不含 contains 结构边）
	//   relations_total = 全部关系边（含 soft-deleted）
	//
	// ★ 排除 contains：那是结构边（原句块 → 关系边），
	//   计数它会让「关系数」随原句数量翻倍。
	g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_block_edges
		 WHERE source_kind='block' AND target_kind='block'
		   AND edge_type != 'contains' AND COALESCE(status,'')='active'`).Scan(&relationCount)
	// relations_total 是**全表**关系数（不过滤 status）——
	// 迁移报告用它，因为 MigrateLegacyTextEntities 会转换全表。
	var relationsTotal int
	g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_block_edges
		 WHERE source_kind='block' AND target_kind='block'
		   AND edge_type != 'contains'`).Scan(&relationsTotal)

	// ★★ hotspots 改数**块**（2026-10-04）
	//
	// 旧实现读 entities 并按 mention_count 排序 ——
	// 而旧表双写已停，那张表不再增长 ⇒ hotspots 永远是迁移时的快照，
	// 而真实热点（哪些块被引用最多）已经变了。
	//
	// ★ 排序口径换成**关系边度数**：一个块被越多关系边指向，
	//   它在图里越重要 —— 这正是 hotspots 想回答的问题。
	//   mention_count 已无处可取（旧表专有）。
	//
	// ★★★ 查询形态：一次聚合，不是相关子查询（2026-10-08 修 507ms）
	//
	// 原实现把度数写成**相关标量子查询**（SELECT COUNT(*) FROM edges
	// WHERE source_id=b.id OR target_id=b.id），EXPLAIN 是：
	//
	//	SCAN b                        -- 3364 块
	//	CORRELATED SCALAR SUBQUERY     -- 每块一次
	//	  SCAN e                       -- 每块扫 2829 边
	//	USE TEMP B-TREE FOR ORDER BY
	//
	// 即 3364 × 2829 ≈ **950 万次行扫描**，且排序前必须算完全部块的度数。
	// 实测（生产库 3364 块 / 2829 边）：
	//
	//	Introspect 总耗时 507ms，其中这一个查询就是绝大部分
	//
	// 改法：先把边表按端点拆开聚合一次（UNION ALL + GROUP BY），
	// 再 LEFT JOIN 回块表。语义完全等价（都是用同一套 status 过滤数度），
	// 但边表只扫**两遍**而不是 3364 遍。
	//
	// 实测同一份数据：**507ms → 11ms**（46×），且新旧查询输出逐字节一致。
	//
	// 为何不建索引就了事：`source_id=b.id OR target_id=b.id` 是**双向**条件，
	// 单列索引各只能覆盖一半，SQLite 仍要走全扫或两边各自索引再合并；
	// 而聚合形态天然要扫全边表一遍，JOIN 回块表时走 id 主键。
	hotspots := []map[string]interface{}{}
	rows, err := g.db.Query(
		`SELECT b.text_content,
		        COALESCE(d.deg, 0) AS deg,
		        COALESCE(b.semantic_type, '') AS typ
		 FROM memory_blocks b
		 LEFT JOIN (
		     SELECT id, COUNT(*) AS deg FROM (
		         SELECT source_id AS id FROM memory_block_edges
		          WHERE COALESCE(status,'') != 'deleted'
		         UNION ALL
		         SELECT target_id AS id FROM memory_block_edges
		          WHERE COALESCE(status,'') != 'deleted'
		     ) GROUP BY id
		 ) d ON d.id = b.id
		 WHERE b.text_content != ''
		 ORDER BY deg DESC, b.created_at ASC
		 LIMIT 10`,
	)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, etype string
			var count int
			if err := rows.Scan(&name, &count, &etype); err == nil {
				hotspots = append(hotspots, map[string]interface{}{
					"name": name, "count": count, "type": etype,
				})
			}
		}
	}

	return map[string]interface{}{
		"entity_count":   entityCount,
		"relation_count": relationCount,
		// relations_total 不过滤 status。★ 迁移报告必须用它 ——
		// MigrateLegacyTextEntities 转换全表，用 active 会少报。
		"relations_total": relationsTotal,
		"memory_hotspots": hotspots,
	}, nil
}

// RecentRelations 返回最近的关系边（不含实体），供只关心关系的调用方使用。
//
// ★ 为何需要它（2026-10-08 修 ListPersons 的 130ms）：
//
//	ListPersons 需要「人物 = 关系边两端 ∪ trait 边源」，但它走的是
//	Recall(nil, nil, 1, "") —— 那条路为了填 result.Entities 会加载
//
// t	**全部块（blockColumns 含稠密向量）**。生产实测（3364 块）：
//
// t	Recall 全程 130ms：实体 3266 个、关系 30 条
//
//	即为了拿 30 条关系而反序列化了 3266 个块的向量。而 ListPersons
//	**只看 result.Relations**，那些实体完全是白付的。
//
// 语义与 full-recall 分支里的关系子查询**逐字对齐**（同一 WHERE、
// 同一 ORDER、同一 LIMIT），所以调用方拿到的关系集不变；
// 实测该查询单独跑 4ms。
//
// 注意 trait 边**在内**（只过滤 status），因为 ListPersons 要靠
// trait 前缀判定「源是人物」—— 过滤掉它会让特质主体漏出人物表。
func (g *GraphDB) RecentRelations(limit int) ([]Relation, error) {
	if limit <= 0 {
		limit = 30
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(
		`SELECT e.id, e.source_id, e.target_id, sb.text_content, tb.text_content,
				e.edge_type, COALESCE(e.confidence, 0), COALESCE(e.status, ''),
				COALESCE(e.session_id, ''), COALESCE(e.turn_id, 0), e.created_at,
				COALESCE((SELECT b.text_content FROM memory_block_edges c
				          JOIN memory_blocks b ON b.id = c.source_id
				          WHERE c.target_kind = 'edge' AND c.target_id = e.id
				            AND c.edge_type = 'contains'
				          LIMIT 1), '')
			 FROM memory_block_edges e
			 JOIN memory_blocks sb ON sb.id = e.source_id
			 JOIN memory_blocks tb ON tb.id = e.target_id
			 WHERE e.source_kind = 'block' AND e.target_kind = 'block'
				  AND COALESCE(e.status, '') = 'active'
			 ORDER BY e.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rels []Relation
	for rows.Next() {
		var rel Relation
		if err := rows.Scan(&rel.ID, &rel.SourceBlockID, &rel.TargetBlockID,
			&rel.SourceName, &rel.TargetName, &rel.RelationType,
			&rel.Confidence, &rel.Status, &rel.SessionID,
			&rel.TurnID, &rel.CreatedAt, &rel.SentenceText); err != nil {
			return nil, err
		}
		rels = append(rels, rel)
	}
	return rels, rows.Err()
}

// MergeEntities 合并两个实体：将 sourceName 的所有信息合并到 targetName
// 1. sourceName 的所有关系重新指向 targetName
// 2. targetName 的 mention_count 增加 sourceName 的计数
// 3. sourceName 彻底删除（不再残留 @merged_ 实体）
// 返回 (关系的重定向数, error)
func (g *GraphDB) MergeEntities(sourceName, targetName string) (int, error) {
	// ★ 委托给 MergeBlocks（2026-10-04）
	//
	// 旧实现在这里直接操作 entities/relations（85 行），块侧完全不动。
	// 后果：改名后「张先生」的块还叫「张先生」，召回照样命中它 ——
	// 判据 TestMergeEntities 就是这么红的（「should be merged and hidden」）。
	//
	// ★ 对外签名保持不变（source, target string）→ (int, error)：
	//   它是 SDK 公开接口（sdk/memory.go 的 Memory 接口），
	//   有 5 个调用方（sdk / toolcall / lua / proc / mocksdk）。
	//   改名会连带改 SDK 契约，那是另一次变更。
	//
	// ★ 返回值语义也保持：重定向的边数。
	return g.MergeBlocks(sourceName, targetName)
}

// ★★★ 改走块/边体系（2026-10-04，删块口径由用户明定）
//
// 旧实现三句 SQL 全在 entities/relations 上：
//
//	SELECT id FROM entities WHERE name = ?
//	DELETE FROM relations WHERE source_id = ? OR target_id = ?
//	DELETE FROM entities WHERE id = ?
//
// 而旧表在停双写（c2bf963）后**不再增长** —— 于是这个函数是**纯空转**：
//
//	memory_blocks      Δ0
//	memory_block_edges Δ0
//
// ★ 危害不是「删不干净」，是**谎报**：工具层回「已彻底删除实体…及其所有关联关系」，
//
//	模型据此认为内容已消失（不再提及、或重新写入），
//	而 40+ 条关联边还在，后续召回继续命中它。
//	这比留残迹严重一级：残迹只是脏，谎报会让模型的行为跟着错。
//
// ★ 删块口径（用户明定「删除块」）：按**块文本精确匹配**定位，
//
//	删掉这些块 + 它们的全部关联边 + 指向它们的 contains 结构边，
//	再摘掉场景引用 —— 与 Purge 的收尾纪律一致
//	（否则场景里挂一条永远召不回的幽灵，而 SceneStats 照样把它算进去）。
//
// ★ 为什么按文本而不按 legacy entities.id：
//
//	旧表只是历史对照，不再是权威源；块才是活图谱的节点。
//	按 name 查 entities.id 会把「旧表里叫这个名字的行」
//	与「图里文本相同的块」当成两回事 —— 那正是本缺陷的成因。
func (g *GraphDB) DeleteEntity(name string) (DeleteResult, error) {
	res, removed, err := g.deleteEntityLocked(name)
	// ★ 上报必须在锁释放后（见 GraphDB.onAccess 的死锁说明）。
	//   删除事件驱动星图的「粒子消散」—— removed 就是真正被删掉的块。
	if err == nil {
		g.emitAccess(AccessEvent{Op: "delete", Removed: removed})
	}
	return res, err
}

// deleteEntityLocked 是 DeleteEntity 的实体（持 g.mu），额外返回被删的块 ID。
func (g *GraphDB) deleteEntityLocked(name string) (DeleteResult, []string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return DeleteResult{}, nil, err
	}
	defer tx.Rollback()

	name = strings.TrimSpace(name)
	if name == "" {
		return DeleteResult{}, nil, fmt.Errorf("name 不能为空")
	}

	// ★ 按块文本精确匹配（不是 LIKE）：DeleteEntity 的承诺是「删除这一个实体」，
	//   用子串会把「admin」连带「admin 8861、billing 8499」一起删掉。
	//   要批量按子串清理走 Purge(subject_contains=…)，两者语义本就不同。
	rows, err := tx.Query(
		`SELECT id FROM memory_blocks WHERE text_content = ? ORDER BY id`, name)
	if err != nil {
		return DeleteResult{}, nil, err
	}
	var blockIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return DeleteResult{}, nil, err
		}
		blockIDs = append(blockIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DeleteResult{}, nil, err
	}
	rows.Close()

	if len(blockIDs) == 0 {
		// ★ 明确报「找不到」，不静默返回成功。
		//
		//   旧实现在这里返回 "not found" 错误，是对的 —— 但它查的是旧表，
		//   于是「旧表有、块里没有」会报成功，而「块里有、旧表没有」会报错。
		//   两种都说不清真相。现在以块为准：块里没有就是没有。
		//
		// ★ 返回 0 而不是 error 也可以，但那样工具层只能回「已删除 0 个」——
		//   模型分不清「删了但本来就没有」和「条件写错了」，
		//   于是会重试或改口径乱猜。明确报错更有用。
		return DeleteResult{}, nil, fmt.Errorf("块 %q 不存在，删除未执行（可能已删除，或这个名字是关系文本而非端点块）", name)
	}

	ph := placeholders(len(blockIDs))
	args := make([]interface{}, len(blockIDs))
	for i, id := range blockIDs {
		args[i] = id
	}

	// ① 删全部关联边（关系边 + 指向这些块的 contains 结构边）。
	//
	// ★ 不区分 edge_type：承诺是「所有关联关系」，
	//   而 contains 边指向已删块就是悬空边 —— 留着会让
	//   sameSentenceOf / hasSameSentenceSibling 的分组落到不存在的块上。
	endpointCond := fmt.Sprintf(
		`(source_kind = 'block' AND source_id IN (%s)) OR (target_kind = 'block' AND target_id IN (%s))`,
		ph, ph)
	edgeArgs := append(append([]interface{}{}, args...), args...)
	edgeRes, err := tx.Exec(
		`DELETE FROM memory_block_edges WHERE `+endpointCond, edgeArgs...)
	if err != nil {
		return DeleteResult{}, nil, err
	}
	edgesDeleted, _ := edgeRes.RowsAffected()

	// ② 删块本身。
	blockRes, err := tx.Exec(
		`DELETE FROM memory_blocks WHERE id IN (`+ph+`)`, args...)
	if err != nil {
		return DeleteResult{}, nil, err
	}
	blocksDeleted, _ := blockRes.RowsAffected()

	// ③ 旧表同步删（若那一行还在）。
	//
	//   不是本函数的主职责 —— 旧表已冻结、不再增长，
	//   留着它只是不让「块已删、旧表还在」这种对不上的状态继续存在。
	//   查不到就跳过：旧表**不是权威源**，它的缺行不代表删除失败。
	//
	// ★★★ 2026-10-05 复核修正（生产库副本实测暴露）
	//
	//   原写法：
	//
	//	DELETE FROM relations WHERE id IN (SELECT id FROM entities WHERE name = ?)
	//	DELETE FROM entities  WHERE name = ?
	//
	//   第一句是**跨表误用 id**：子查询返回的是 **entities.id**，
	//   而外层匹配的是 **relations.id**。两者同名不同表。
	//   生产实测：实体 201 被 relation 101 引用（201≠101），
	//   于是 relation 101 根本没被删，紧接着
	//   DELETE FROM entities 因 relations 的外键（source_id/target_id → entities.id）
	//   而报 `FOREIGN KEY constraint failed` —— **整个删除事务回滚**。
	//
	//   ⇒ DeleteEntity 在任何「旧表里存在、且被关系引用」的实体上直接失败。
	//     判据抓不到：delete_entity_blocks_test.go 只用 Commit/putBlocks 建**块**，
	//     旧表是空的 ⇒ 那条路径不可达（这是「测不到」而非「没问题」）。
	//
	//   正确写法：按**外键列**删，而不是拿 id 去撞另一个表的 id。
	if _, err := tx.Exec(
		`DELETE FROM relations WHERE source_id IN (SELECT id FROM entities WHERE name = ?)
		    OR target_id IN (SELECT id FROM entities WHERE name = ?)`, name, name); err != nil {
		return DeleteResult{}, nil, err
	}
	if _, err := tx.Exec(`DELETE FROM entities WHERE name = ?`, name); err != nil {
		return DeleteResult{}, nil, err
	}

	if err := tx.Commit(); err != nil {
		return DeleteResult{}, nil, err
	}

	// ④ 摘场景引用。必须在 Commit 之后 —— purgeStaleSceneRefsLocked 走 g.db，
	//   而这里的事务还没提交。
	g.purgeStaleSceneRefsLocked()

	return DeleteResult{Blocks: int(blocksDeleted), Edges: int(edgesDeleted)}, blockIDs, nil
}

// DeleteResult 报告一次删除实际删掉了什么。
//
// ★ 不返回单一数字，是因为「删了 1 个块」和「删了 0 个块 40 条边」
//
//	是**两种不同的事实**，压成一个 int 就会让工具层只能说谎
//	—— 那正是本缺陷的成因（回「已彻底删除…及其所有关联关系」而实际 Δ0）。
type DeleteResult struct {
	Blocks int `json:"blocks"`
	Edges  int `json:"edges"`
}

// Archive 把 days 天前的**关系边**标记为 archived。
//
// ★★★ 改走块/边体系（2026-10-04）
//
// 原实现只 UPDATE relations 表 —— 而旧表在停双写后**不再增长**，
// 所以归档对块体系完全无效：关系边永远不会被归档，
// 于是「按时间衰减记忆」这个能力静默失效了。
//
// ★ 它当时零调用方（只有测试），所以这个缺陷从未被发现。
//
//	★★ 停双写是把它照出来的原因：旧表冻结 ⇒ 任何只碰旧表的
//	  维护操作都变成空转。
//
// ★ archived 与 deleted 的区别：archived 保留在库里（可回溯），
//
//	但不参与召回（读路径按 status='active' 过滤）。
func (g *GraphDB) Archive(days int) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	result, err := g.db.Exec(
		`UPDATE memory_block_edges
		 SET status = 'archived'
		 WHERE COALESCE(status, '') = 'active'
		   AND edge_type != 'contains'
		   AND created_at < datetime('now', ?)`,
		fmt.Sprintf("-%d days", days),
	)
	if err != nil {
		return 0, err
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// ClearSentenceID 解除「关系 → 原句」的引用（LLM 复审后不再信任那句话）。
//
// ★★★ 改走块/边体系（2026-10-04）
//
// 原实现 `UPDATE relations SET sentence_id = 0 WHERE id = ?` ——
// 而调用方（internal/agent/core/distill.go）传的 rel.ID 来自
// RecallSorted 的返回，那里 Relation.ID 现在是**关系边 ID**。
//
// ⇒ ★★ 也就是说：它一直在清理**错误的行**。
//
//	旧表那一行的 sentence_id 纹丝不动，而边侧什么都没发生。
//	而这**没有任何报错** —— UPDATE 影响 0 行也是成功。
//
// ★ 正确做法：删掉「关系边 --contains--> 原句块」这条结构边。
//
//	sentence_id=0 的语义是「这条关系不再挂在那句话上」，
//	而在块体系里那个挂接就是 contains 边。
//
// ★ 幂等：边不存在时返回 nil（复审流程会重复调用）。
func (g *GraphDB) ClearSentenceID(edgeID int64) error {
	if edgeID == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.Exec(
		`DELETE FROM memory_block_edges
		 WHERE target_kind = 'edge' AND target_id = ? AND edge_type = 'contains'`,
		edgeID)
	return err
}

// CleanupOrphanedSentences 删除既无关系引用、也无媒体块边的句子，返回删除数。
//
// 两个条件都必须看：旧媒体实体被迁移成原生块后，那些句子可能只靠
// sentence --contains--> block 存活，若只看 relations 引用就会被误删，
// 连带把块边变成悬空引用。
func (g *GraphDB) CleanupOrphanedSentences() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cleanupOrphanedSentencesLocked()
}

// cleanupOrphanedSentencesLocked 是 CleanupOrphanedSentences 的加锁内联版，
// 供已在写锁内的调用方（PurgeNoise）复用，避免自锁死。
func (g *GraphDB) cleanupOrphanedSentencesLocked() (int, error) {
	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// 先清掉指向将被删除句子的块边，避免留下悬空端点。
	if _, err := tx.Exec(`DELETE FROM memory_block_edges
		WHERE source_kind = 'sentence' AND source_id NOT IN (
			SELECT CAST(id AS TEXT) FROM sentences
			WHERE id IN (SELECT DISTINCT sentence_id FROM relations WHERE sentence_id != 0)
			   OR id IN (SELECT CAST(source_id AS INTEGER) FROM memory_block_edges WHERE source_kind = 'sentence')
		)`); err != nil {
		return 0, err
	}

	res, err := tx.Exec(`DELETE FROM sentences WHERE id NOT IN (
			SELECT DISTINCT sentence_id FROM relations WHERE sentence_id != 0
		) AND id NOT IN (
			SELECT CAST(source_id AS INTEGER) FROM memory_block_edges WHERE source_kind = 'sentence'
		)`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

func (g *GraphDB) Close() error {
	return g.db.Close()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// FindRelations 按实体名与关系类型**精确**查找活跃关系（带原句）。
//
// 为什么需要精确查找：memory_edit 走的是「按包含匹配 Purge + 写入新三元组」，
// 中间那一步会把旧关系的附加信息（置信度、场景、原句）一起丢掉。
// 编辑前先精确取回这条关系，才能把这些信息带过去。
func (g *GraphDB) FindRelations(subject, relationType, object string) ([]Relation, error) {
	// ★★★ 改走块/边体系（2026-10-04）
	//
	// 原实现 JOIN relations + entities + sentences。旧表停双写后
	// 这条查询返回空 ⇒ 精确查找功能失效（判据 TestFindRelationsAndScenesOfRelation）。
	//
	// 按**块文本**精确匹配两端块 —— 语义与旧表的 name = ? 一致。
	rows, err := g.db.Query(
		`SELECT e.id, e.source_id, e.target_id, sb.text_content, tb.text_content,
		        e.edge_type, COALESCE(e.confidence,0), COALESCE(e.status,''),
		        COALESCE(e.session_id,''), COALESCE(e.turn_id,0), e.created_at,
		        COALESCE((SELECT b.text_content FROM memory_block_edges c
		                  JOIN memory_blocks b ON b.id = c.source_id
		                  WHERE c.target_kind='edge' AND c.target_id = e.id
		                    AND c.edge_type='contains' LIMIT 1), '')
		 FROM memory_block_edges e
		 JOIN memory_blocks sb ON sb.id = e.source_id
		 JOIN memory_blocks tb ON tb.id = e.target_id
		 WHERE e.source_kind='block' AND e.target_kind='block'
		   AND e.edge_type != 'contains'
		   AND COALESCE(e.status,'') = 'active'
		   AND sb.text_content = ? AND e.edge_type = ? AND tb.text_content = ?`,
		subject, relationType, object)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Relation
	for rows.Next() {
		var rel Relation
		if err := rows.Scan(&rel.ID, &rel.SourceBlockID, &rel.TargetBlockID,
			&rel.SourceName, &rel.TargetName, &rel.RelationType,
			&rel.Confidence, &rel.Status, &rel.SessionID,
			&rel.TurnID, &rel.CreatedAt, &rel.SentenceText); err != nil {
			return nil, err
		}
		if rel.DateBucket == "" && !rel.CreatedAt.IsZero() {
			rel.DateBucket = rel.CreatedAt.Format("2006-01-02")
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

// columnExists 判断表里是否已有某列（SQLite 的 ALTER 无法改约束，只能按列探测后重建）。
func columnExists(tx *sql.Tx, table, column string) bool {
	rows, err := tx.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// effectiveScenes 合并一条三元组的场景键（Scenes 多值 + Scene 单值），去重且保序。
//
// 单值 Scene 保留是为了兼容既有调用方与 memory_commit 的模型参数；
// 多值 Scenes 是"声明 + 涌现"两条路同时挂载的载体。
func effectiveScenes(t Triple) []string {
	if len(t.Scenes) == 0 && t.Scene == "" {
		return nil
	}
	seen := make(map[string]bool, len(t.Scenes)+1)
	out := make([]string, 0, len(t.Scenes)+1)
	add := func(k string) {
		k = NormalizeSceneKey(k)
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, k)
	}
	for _, k := range t.Scenes {
		add(k)
	}
	add(t.Scene)
	return out
}

// sortRecallEntities 按模式给实体排序（原地），并回填 MatchRank。
//
// 相关性模式的三层键（从强到弱）：
//  1. 实词命中层级：完全相等(0) > 前缀(1) > 包含(2)。**最关键的一层**——
//     问 metrics 时「metrics服务端口8328」是前缀命中，而「metrics服务端口」类
//     实体只是包含命中，前者必须在前。
//  2. 提及次数降序：提及多的更可能是常用实体。
//  3. 名字长度升序：短名更可能是实体本身而非长描述（沿用 SQL 原口径）。
//
// 时间模式：UpdatedAt 倒序 → 提及次数降序 → 名字长度升序。
// 运维场景里「现在的值」通常是最新的那次写入（实测 v4 overwrite 组
// 就是「新分机覆盖旧分机」），相关性排序会把旧值排在新值前面。
//
// 稳定排序保证同层内顺序可复现，不会两次调用结果跳动。
func sortRecallEntities(ents []Entity, rank map[int64]int, keywords []string, mode SortMode) {
	spec := make(map[int64]int, len(ents))
	for i := range ents {
		// 先算实词层级：只命中泛词时退回整体 rank（仍应召回，只是靠后）。
		spec[ents[i].ID] = bestSpecificRank(rank[ents[i].ID], ents[i].Name, keywords)
	}
	// 回填 MatchRank，供上层展示「为什么这条相关」。
	for i := range ents {
		ents[i].MatchRank = spec[ents[i].ID]
	}

	sort.SliceStable(ents, func(i, j int) bool {
		if mode == SortRecent {
			// Seq 优先：严格单调，不受秒级精度影响。
			if ents[i].Seq != ents[j].Seq {
				return ents[i].Seq > ents[j].Seq
			}
			if !ents[i].UpdatedAt.Equal(ents[j].UpdatedAt) {
				return ents[i].UpdatedAt.After(ents[j].UpdatedAt)
			}
		} else if spec[ents[i].ID] != spec[ents[j].ID] {
			return spec[ents[i].ID] < spec[ents[j].ID]
		}
		if ents[i].MentionCount != ents[j].MentionCount {
			return ents[i].MentionCount > ents[j].MentionCount
		}
		return len(ents[i].Name) < len(ents[j].Name)
	})
}

// sortRecallRelations 按同一模式给关系排序（原地）。
//
// 时间模式：CreatedAt 倒序 → TurnID 倒序 → ID 倒序。
//
// ★ 为什么必须带 TurnID/ID 两级兜底（实测抓到的坑）：SQLite 的 CURRENT_TIMESTAMP
// **只到秒**。同一秒内写入的两次覆盖（实测 turn 1 写 4379、turn 2 写 4324
// 落在同一秒），CreatedAt 完全相等，稳定排序会保留原顺序 —— 而原顺序来自
// `ORDER BY confidence DESC`，置信度相同时退化到 rowid 升序，也就是**旧值在前**。
// 这与「记忆只能按毫秒时间戳排序」是同一类问题（SQLite 秒精度）。
// TurnID 在一次会话内单调递增，RelationID 是自增主键，两者都严格单调，
// 因此即使同秒也能分出先后。
//
// 相关性模式下关系**保持召回顺序**（源头已按种子实体的相关性排过），
// 刻意不打乱 —— 邻接扩展的顺序本身带语义（从种子实体出发由近及远）。
func sortRecallRelations(rels []Relation, mode SortMode) {
	if mode != SortRecent {
		return
	}
	sort.SliceStable(rels, func(i, j int) bool {
		a, b := rels[i], rels[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		if a.TurnID != b.TurnID {
			return a.TurnID > b.TurnID
		}
		return a.ID > b.ID
	})
}

// rankEntity 按「精确度层级 → 提及次数 → 名字长度」给实体排序（原地）。
//
// 为什么需要跨关键词归并（2026-10-01 跑分实测）：SQL 里已经算出了三层精确度
// （完全相等 > 前缀命中 > 包含命中），但那只是**单个关键词内**的排序。
// ExtractKeywords 会把问句切成多个词（实测「metrics服务的端口是多少」
// → [metrics, 服务, 端口]），各关键词的结果被依次 append —— 精确度层级
// 在拼接过程中被冲淡，第一个泛词召回的噪音会排在精确命中之前。
//
// 排序键（从强到弱）：
//  1. rank：完全相等(0) > 前缀(1) > 包含(2)。**这是最关键的一层**——
//     问 metrics 时，「metrics服务端口8328」是前缀命中，而「metrics服务端口」
//     类实体是包含命中，前者必须在前。
//  2. mention_count 降序：提及多的更可能是常用实体。
//  3. 名字长度升序：短名更可能是实体本身而非长描述（沿用 SQL 的口径）。
//
// 稳定排序：rank 相同的实体保持原顺序（SQL 已在各关键词内排过），
// 避免同层内因合并而随机跳动 —— 那会让「第一次调用结果」与「第二次」不一致。
func rankEntity(ents []Entity, rank map[int64]int, keywords []string) {
	// specificity 为每个实体算「最有区分度的那个关键词」上的命中层级。
	//
	// ★ 为什么要区分「泛词」与「实词」（实测抓到）：关键词里有「端口」「服务」
	// 这类泛词，实体名恰好就叫「端口」时它对泛词是**完全相等命中（rank=0）**，
	// 于是把真正的答案「metrics服务端口8328」（前缀命中 rank=1）压到了第二。
	// 「完全相等」只在**实词**上才算强信号。
	spec := make(map[int64]int, len(ents))
	for _, e := range ents {
		spec[e.ID] = bestSpecificRank(rank[e.ID], e.Name, keywords)
	}
	sort.SliceStable(ents, func(i, j int) bool {
		ri, rj := spec[ents[i].ID], spec[ents[j].ID]
		if ri != rj {
			return ri < rj
		}
		if ents[i].MentionCount != ents[j].MentionCount {
			return ents[i].MentionCount > ents[j].MentionCount
		}
		return len(ents[i].Name) < len(ents[j].Name)
	})
}

// rankOf 取实体的精确度层级；未记录时按最差处理（包含命中）。
func rankOf(rank map[int64]int, e Entity) int {
	if r, ok := rank[e.ID]; ok {
		return r
	}
	return 2
}

// genericKeywords 是区分不出实体的泛词：对它们做「完全相等」匹配没有意义。
//
// 判据：出现在大量实体名里的短词。实测「端口」「服务」这类词在 order-gw
// 的实体表里遍布（每个「xx服务端口」都含它们），而「metrics」只出现一次。
// 用固定表而非动态统计，是为了召回路径可预测、不引入额外查询；
// 代价是新领域需要补这张表（补漏了也只是排序略差，不会召回不到）。
var genericKeywords = map[string]bool{
	"端口": true, "服务": true, "版本": true, "接口": true, "任务": true,
	"状态": true, "类型": true, "时间": true, "配置": true, "数量": true,
	"名称": true, "结果": true, "内容": true, "问题": true, "记录": true,
	"数据": true, "信息": true, "文件": true, "系统": true, "功能": true,
}

// bestSpecificRank 返回实体在**实词**关键词上的最佳命中层级。
//
// 若实体只命中泛词（全都不算实词），退回用整体 rank —— 它仍然该被召回，
// 只是排在命中实词的实体之后。
func bestSpecificRank(rank int, name string, keywords []string) int {
	best := -1
	lower := strings.ToLower(name)
	for _, kw := range keywords {
		if genericKeywords[strings.ToLower(kw)] {
			continue
		}
		lkw := strings.ToLower(kw)
		var r int
		switch {
		case lower == lkw:
			r = 0
		case strings.HasPrefix(lower, lkw):
			r = 1
		default:
			r = 2
		}
		if best < 0 || r < best {
			best = r
		}
	}
	if best < 0 {
		return rank // 只命中泛词
	}
	return best
}
