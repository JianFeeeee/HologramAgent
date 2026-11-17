// Package model 是内核**共享值类型**的叶子包（纯数据，零业务依赖）。
//
// ## 为什么它必须单独存在（用户裁定 2026-11-17）
//
// 目标架构要求 `internal/kernelapi` 只依赖 stdlib + 纯类型。而接口方法集
// 必然收发记忆/文档/知识的值类型（`Triple`、`MemoryBlock`、`ColdDoc`…）。
// 若这些 DTO 仍声明在 `internal/memory` / `internal/memory/coldcontext` /
// `internal/knowledge`，接口包就必须 import **实现包** —— 那正是本次要消灭的
// 形态（「解耦的目的是让其他模块在不 import 实现的前提下通过 import 接口
// 进行调用」）。
//
// 所以：**DTO 声明搬到这里**（唯一声明点），实现包反过来 import 它，
// 并通过类型别名（`type Triple = model.Triple`）保持既有调用点不变。
//
// ## 命名
//
// 包名 `model`（导入路径 `internal/kernelapi/model`）：它是内核接口层的
// 共享模型，不是某个模块的私产。
//
// ## 边界（违反即破坏解耦）
//
//   - 本包**只能** import 零依赖的叶子包（`internal/tfidf` 的 `Vector` 是
//     `map[string]float64`，它自己不 import 任何 internal 包 —— 用它而不是
//     另造一份，是为了让 `ColdDoc.Vector` 的类型与实现侧逐字一致）。
//   - 本包**只放数据**：不许有业务逻辑。原先挂在 DTO 上的方法
//     （`SituationFeature.Key/Weight`、`ColdDoc.HasEvents`、
//     `ColdEvent.InOut/InOutWithReasoning`）已按用户裁定
//     （«funcs_in_memory»）改为**实现包里的包级函数 / 方法保留在实现包**：
//     私有字段依赖（`Entity.BlockKey`）与业务归一化（场景键、权重表）
//     本来就是实现侧知识，不该是"值类型"的一部分。
package model

import (
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/tfidf"
)

type BlockModality string

type PageKind string

type BlockRole string

type MemoryBlock struct {
	ID       string        `json:"id"`
	Modality BlockModality `json:"modality"`
	// Text 是文本块（Modality=text）的负载。
	Text string `json:"text,omitempty"`
	// Data 是媒体块（Modality=image/video/audio）的负载：内容的原始字节。
	//
	// 落盘形态随所在层而定：L0/L1 的块内联在 JSON 里（Data 序列化为 base64），
	// L3 的块落 SQLite 的 BLOB 列。无论哪层，字节都不脱离块单独存在。
	Data        []byte    `json:"data,omitempty"`
	MIME        string    `json:"mime,omitempty"`
	Size        int64     `json:"size,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Vector      []float64 `json:"vector,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Source      string    `json:"source,omitempty"`
	Tool        string    `json:"tool,omitempty"`
	// Scene 是这个块所属的场景键（可空）。
	//
	// 块是记忆流水线里最细的「子项目」：一段转写、一张图的描述、一份附件。
	// 场景要贯穿到流水线底，就得从块开始——否则「QQ 那场对话里发过来的那张图」
	// 在场面重现时永远拿不回来。块进 L3 时按 Scene 挂 scene_refs(kind='block')，
	// 场景召回即可把它取回（见 GraphDB.RecallByScene）。
	Scene string `json:"scene,omitempty"`
	// SemanticType 是实体的语义类别（Person / Animal / Concept / Topic…），
	// 对应旧 entities.type 与 Triple.SubjectType/ObjectType。
	//
	// ★ 2026-10-04 加入。此前 Triple 标注的类型只写旧表，块侧完全没有 ——
	//   判据 TestGraphCommit_CarriesAllFields 抓到：
	//   Commit 标注的 (Person, Animal) 读回来是 ("block","block")。
	SemanticType string `json:"semantic_type,omitempty"`

	// ─────────── 段页式身份（2026-10-29）───────────
	//
	// ## 为什么需要这三个字段
	//
	// 设计约束（用户裁定）：**淘汰/渲染的最小单位必须是"移除后上下文仍渲染正确"**。
	// 而"渲染正确"的硬性要求是 OpenAI 协议那条：assistant(tool_calls)
	// 后面必须紧跟配对齐全的 tool 消息。所以最小单位不是单块，而是**页**。
	//
	//	段 Frame = 一次完整任务（一个 FrameTS）
	//	  └─ 页 Page = 一次完整往返
	//	       ├─ llm_round  : llm_input  ──→ llm_output
	//	       └─ tool_round : tool_call  ──配对(CallID)──→ tool_result
	//
	// ## 为什么用时间戳而不是自增帧号
	//
	// 用户裁定：帧号**直接用时间戳**，不另造自增编号。理由：
	//   · 时间戳在 L1/L2/L3 三层天然一致，不需要额外的编号分配器
	//   · 跨进程/跨层迁移时无需协调（自增号需要中心分配，重启即冲突）
	//   · 天然可排序（渲染时序直接按它排）
	//
	// ## 为什么不跨帧
	//
	// 同一 FrameTS 的块不管走到哪层，**整批待在同一层**。原因不是洁癖：
	// 帧是最小可渲染单元，拆开就会出现"有 call 没 result"的破帧。
	// 判据见 §C7。
	//
	// ★ 旧数据（FrameTS 为零值）视为"独立帧"，各自成帧、不参与配对校验。
	//   这是迁移的兼容路径：不能因为它们没有帧号就拒绝写入或丢弃。
	FrameTS time.Time `json:"frame_ts,omitempty"`
	// PageSeq 是帧内的**页序号**（从 1 开始，连续不减）。
	//
	// 与 FrameTS 一起构成页的身份：`(FrameTS, PageSeq)`。
	// 同一页内的块（request/response，或 call/result）靠它归组。
	PageSeq int `json:"page_seq,omitempty"`
	// PageKind 标记这一页是哪种往返。
	PageKind PageKind `json:"page_kind,omitempty"`
	// BlockRole 是块在页内的角色（request 端 / response 端）。
	BlockRole BlockRole `json:"block_role,omitempty"`
	// CallID 是 tool_round 的**配对键**：tool_call 块与它的 tool_result 块
	// 必须同 CallID。llm_round 的页此字段为空。
	//
	// ★ 为什么不靠 (FrameTS, PageSeq) 配对：并行工具调用时，
	//   一页里有 N 个 call（assistant 一次声明多个），它们共用 PageSeq
	//   但各自配对不同的 result —— 必须有 CallID 才分得清。
	CallID string `json:"call_id,omitempty"`
	// Inactive 标记该页**不参与渲染**（但仍可回读、仍在原层）。
	//
	// 用户裁定：段超预算时按相似度**剔除**低分页 —— 实际是标注 inactive，
	// 不渲染为消息。数据不删、层不迁、随时可读回，因此不是"静默丢失"。
	// 渲染时必须在原位置留下提示行，让模型知道"这里省略了内容"。
	Inactive bool `json:"inactive,omitempty"`

	// LastRecalledAt 是最近一次被召回的时间（P6 回收判定）。
	//
	// ★ 为什么用「被召回」而不是「被创建」判死活：
	//   判断一块记忆有没有用，看它有没有被想起来，不是什么时候写进来的。
	//   旧块若一直命中查询，它是活的 —— 不能因为"老"就回收。
	//
	// 零值 = 从未被召回（回收候选）；非零 = 至少用过一次（按时间窗判定）。
	// 两者刻意分开：从不被召回 与 很久没被召回 是不同的病。
	LastRecalledAt time.Time `json:"last_recalled_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MemoryBlockEdge struct {
	ID         int64     `json:"id"`
	SourceKind string    `json:"source_kind"`
	SourceID   string    `json:"source_id"`
	TargetKind string    `json:"target_kind"`
	TargetID   string    `json:"target_id"`
	Type       string    `json:"type"`
	CreatedAt  time.Time `json:"created_at"`

	// ★ 以下是「边作为独立单位」需要的属性列。
	//
	// 结构边（contains 等）不填这些；关系边填。
	// 旧 relations 表有同名列 —— 块化后属性挂在**边**上，
	// 而不是挂在一张独立的 relations 表上（那才是「边不是独立单位」的根源）。
	Confidence float64 `json:"confidence,omitempty"`
	SessionID  string  `json:"session_id,omitempty"`
	TurnID     int     `json:"turn_id,omitempty"`
	// Status 是 EdgeActive / EdgeDeleted / EdgeMerged。
	// 空字符串表示结构边（无状态语义）。
	Status string `json:"status,omitempty"`
	// MergedInto 在 EdgeMerged 时指向目标块：源块被并进哪里，
	// 历史边留在源块上不重定向。
	MergedInto string `json:"merged_into,omitempty"`
}

type BlockRecallQuery struct {
	Vector []float64
	// Fingerprint 是发起方所用向量空间的指纹。与库中块不一致的块会被
	// **跳过**（不是报错）：库里可能同时存在两个空间的块（迁移期），
	// 跳过才是正确的——拿新空间的查询向量去比旧空间的块向量，余弦值
	// 没有任何意义，而且不会报错，只是"结果看起来还行但全是错的"。
	Fingerprint string
	TopK        int
	// MinScore 低于此分数的候选被丢弃（0 表示不设下限）。
	MinScore float64
	// Modality 非空时只召回该模态的块（空 = 不限）。
	//
	// ★ 为何需要它（实测回归）：CAS 时代视觉路查 `FROM media`（只含媒体），
	//   结构上不可能被文本块挤掉。块 union 化后视觉路改用本接口（全局
	//   top-K，含数千文本块），而实测同一个 chineseclip 空间里：
	//       查询「图片」→ 图像块 max cosine 0.377，文本块 max 1.000
	//       查询「照片」→ 图像块 max 0.385，文本块 max 0.935
	//   图片的绝对相似度天然低于字面相同的文本，per=200 时视觉路返回
	//   1000 条里**图像块 0 条** —— 视觉路被彻底饿死。
	//   所以“取全局 top-K 再过滤掉文本块”是错的取法：过滤发生在截断之后。
	//   必须在 SQL 层就限定模态，让 top-K 在目标模态内部竞争。
	Modality BlockModality
	// ExcludeText 为 true 时排除文本块（等价 Modality 取非文本全集，
	// 用于“媒体块”语义；模态可能有 image/video/audio 多种）。
	ExcludeText bool
	// SkipCentroid 关闭中心化校正，直接用原始向量算余弦。
	//
	// 默认是要校正的：实测 chineseclip 的块向量均值范数 0.9374，
	// 93.7% 的能量在同一个方向上，导致同属性不同值的分离度只有 0.0095
	// （去均值后 0.0689，提升 7.3 倍）。而中心是**按需加载**的 ——
	// 库里没建过中心、或中心已失效（块数变化 >20%）时自动跳过校正，
	// 所以这个开关只在「明确知道中心存在却想绕开」时才需要。
	SkipCentroid bool
}

type BlockHit struct {
	Block MemoryBlock `json:"block"`
	Score float64     `json:"score"`
}

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
	BlockKey string `json:"-"`
}

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
	// SourceVec / TargetVec / SentenceVec 是端点块与原句块的**稠密向量**。
	//
	// 为什么带在关系上：场景召回是"每轮都要跑的常驻注入"，排序必须与本轮
	// query 相关（块里本来就存着向量，相关性就是一次余弦比较）。
	// 此前 RecallByScene 只按「场景权重 → 时间倒序」排，老图库里的历史高频
	// 三元组常年霸榜，每轮灌同一批无关内容（实测 90% token 是这些）。
	//
	// 不落 JSON / 不入库：它们只用于本次比较，块向量本身仍在 memory_blocks 里。
	SourceVec   []float64 `json:"-"`
	TargetVec   []float64 `json:"-"`
	SentenceVec []float64 `json:"-"`
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
	// 约定见 NormalizeSceneKey：`chan:qq`、`chan:qq/peer:group_123`。
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

type RecallResult struct {
	Entities  []Entity   `json:"entities"`
	Relations []Relation `json:"relations"`
}

type SortMode string

type DeleteResult struct {
	Blocks int `json:"blocks"`
	Edges  int `json:"edges"`
}

type SituationFeature struct {
	Kind  string
	Value string
}

type Situation struct {
	Features []SituationFeature
}

type TurnScene struct {
	// Primary 是本轮写记忆时的**首选**场景：优先用涌现出来的（细粒度、
	// 与措辞无关），没有（首次出现、场景还没长出来）时退到第一个声明场景。
	Primary string
	// Keys 是声明场景 + 涌现场景的全集（去重保序），供召回并集使用。
	Keys []string
	// Emergent 标记 Primary 是否来自涌现。
	Emergent bool
	// DeclaredCreated 是本次**新建**的声明场景（此前不存在，因声明而成立）。
	DeclaredCreated []string
}

type ColdDoc struct {
	ID      string      `json:"id"`
	Summary string      `json:"summary"`
	Content string      `json:"content"` // 渲染视图，不是原文
	Events  []ColdEvent `json:"events,omitempty"`

	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Source      string            `json:"source"`
	Meta        map[string]string `json:"meta,omitempty"`
	AccessCount int               `json:"access_count"`
	LastAccess  time.Time         `json:"last_access"`

	// ArchiveAttempts 是 L2→L3 归档失败的累计次数。
	//
	// ★ 为什么必须持久化（2026-11-15）：归档失败此前只是「保留待下一轮」，
	// 没有计数、没有退避。生产实测 50 个文档每小时重试一次，从 2026-11-11
	// 连跑 3 天共 2950 次 —— 输入完全相同 ⇒ 必然失败的无限循环。
	// 计数进不了内存以外的任何东西 ⇒ 重启后清零，无限继续。
	ArchiveAttempts  int    `json:"archive_attempts,omitempty"`
	ArchiveLastError string `json:"archive_last_error,omitempty"`

	// Blocks 是本文档持有的**全部**块（事件里的文本块 + 媒体块去重后的并集）。
	//
	// ★ 为什么还留一个扁平列表而不是只看 Events：
	//   · 向量检索／跨模态融合要能不遍历事件就拿到所有块；
	//   · L2→L3 蒸馏按块迁移；
	//   · 存量文档（无 Events）只有这一个字段有块。
	//
	// ★ 它是**视图**，不是事实源：块的真实归属在 Events[i].Blocks 里。
	//   两者由 ContextToColdContext 同一处产出，不得分别修改。
	Blocks []MemoryBlock `json:"blocks,omitempty"`

	Vector   tfidf.Vector `json:"vector,omitempty"`    // TF-IDF 稀疏向量（fallback 时持久化）
	DenseVec []float64    `json:"dense_vec,omitempty"` // 多模态稠密向量（主路径）
	DenseFP  string       `json:"dense_fp,omitempty"`  // DenseVec 所属统一空间指纹，变化时触发重算
}

type ColdEvent struct {
	Timestamp   time.Time        `json:"ts"`
	Source      string           `json:"source"`
	ToolsUsed   []string         `json:"tools_used,omitempty"`
	ToolResults []ToolResultItem `json:"tool_results,omitempty"`

	// NoMemory 继承自 L1 的 ContextEvent.NoMemory（声明源：插件注入的
	// payload.no_memory 或 ChannelDef.NoMemory）。
	//
	// ★ 为什么必须一路带到 L2（2026-11-15）：
	// 声明说的是「不参与记忆计算」，而 **L3 蒸馏（coldDocToTriples）会
	// 拿 L2 的原文重新跑一遍三元组抽取**。若声明在 L2 边界断了，
	// 同一段内容就在第一次蒸馏被挡、第二次蒸馏被放行 —— 声明等于失效。
	//
	// 生产后果（实测）：`context_archived` 文档以终端日志/ADB 截图回执为正文，
	// NLP 抽出「、」「（」「，」当主语，被 validEntityName 全数拒掉 ⇒
	// 块数不涨 ⇒ 文档被永久扣在 L2，每小时重试一次，连跑 3 天 2950 次。
	NoMemory bool `json:"no_memory,omitempty"`
	// ToolCalls 是每次工具调用的完整记录（含参数），与 ToolResults 靠 CallID 配对。
	ToolCalls []ToolCallItem `json:"tool_calls,omitempty"`

	// Blocks 是本事件持有的块，**正文就在里面**。
	//
	// 顺序约定（复原时按此顺序回填 ContextEvent）：
	//   Input 文本块 → Response 文本块 → 媒体块…
	// 由 docBlocksForEntry 保证；回填由 blocksToInOut 对称解读。
	Blocks []MemoryBlock `json:"blocks,omitempty"`
}

type ColdHit struct {
	// Doc 是命中的冷上下文条目。
	//
	// 字段名保持 Doc：它是**字段**不是类型，改名会波及所有 h.Doc 调用点，
	// 而与"类型叫什么"无关。
	Doc   *ColdDoc
	Score float64
}

type ToolResultItem struct {
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

type ToolCallItem struct {
	CallID       string                 `json:"call_id,omitempty"`
	Name         string                 `json:"name"`
	Arguments    map[string]interface{} `json:"arguments,omitempty"`
	RawArguments string                 `json:"raw_arguments,omitempty"`
}

type ContextEntry struct {
	Timestamp time.Time
	Source    string
	Content   string
	Response  string
	// NoMemory 是 L1 事件上的同一声明，原样带到 L2（见 ColdEvent.NoMemory）。
	NoMemory bool
	// ReasoningContent 是本轮模型的思考内容（thinking 模式的思维链）。
	//
	// L2 必须存它（2026-11-11 用户裁定）：决策依据、排除掉的选项、
	// "先查 A 再查 B"的因果链常常只在思考里。只存 Response 会导致
	// 召回后只能答出"做了什么"，答不出"为何这么做"。
	ReasoningContent string
	// ToolCalls 是工具调用的完整记录（含参数），归档时原样带到 L2。
	ToolCalls   []ToolCallItem
	ToolsUsed   []string
	ToolResults []ToolResultItem
	Blocks      []MemoryBlock // 一等记忆块随事件一起迁移到文档
}

// TextEvent 是文本记忆（internal/memory/text）的一条原始 I/O 记录。
//
// ★ 名字带前缀：本包同时承载事件总线口径的 Event，两个值类型不能同名。
type TextEvent struct {
	Timestamp int64    `json:"ts"`
	Source    string   `json:"source"`
	Input     string   `json:"input"`
	Response  string   `json:"response,omitempty"`
	ToolsUsed []string `json:"tools_used,omitempty"`
	AgentID   string   `json:"agent_id,omitempty"`
}

// PromptSection 是插件声明的一段系统提示词（渲染期只读）。
//
// ★ 名字带前缀：本包已有一个 Section（若将来有别的段类型，同名会撞）。
type PromptSection struct {
	// Section 是**全局唯一**的段名，约定 `<插件名>.<名字>`（如 "skillmgr.skills"）。
	//
	// ★ 强制前缀约定（与 security 的 Profile.Name 同型）：拼错能被立刻发现，
	// 且插件之间不可能互相覆盖。
	Section string

	// Owner 是声明它的插件名。必须与 Section 的前缀一致。
	Owner string

	// Order 决定渲染顺序（小的在前）。同 Order 按 Section 字典序，保证稳定。
	Order int

	// Content 是正文。**空串表示移除该段**（运行时撤销声明）。
	Content string

	// Volatile 表示内容会频繁变化（如技能索引随 skill 增删变化）。
	//
	// 仅用于诊断输出（状态面能看到哪些段每轮都在变），不参与渲染决策。
	// 存在的理由：技能索引是本类声明里唯一"随时可能变"的段，
	// 出问题时需要能一眼区分"这段是静态说明"还是"这段每轮重算"。
	Volatile bool
}

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

type IndexItem struct {
	Name    string             `json:"name"`
	Preview string             `json:"preview"` // 前 200 字摘要
	Tags    []string           `json:"tags"`
	Vector  map[string]float64 `json:"vector"` // TF-IDF 特征向量（top-N 特征）
	Size    int                `json:"size"`   // 内容总字节数
}

type TreeIndex struct {
	Name     string                `json:"name"`
	Children map[string]*TreeIndex `json:"children,omitempty"`
	Items    []IndexItem           `json:"items,omitempty"` // 此节点下的知识条目（含向量）
}

type ImportOptions struct {
	// Dir 是源目录，**必须是绝对路径**。
	//
	// 为什么强制绝对：agent 调工具时 cwd 不受控（内核 daemon 的 cwd 是
	// 工作目录，不是 agent 心智模型里的那个"项目目录"）。相对路径会
	// 静默导到错误的地方，而用户以为导的是他指定的目录。
	Dir string

	// Category 是**前缀叠加**在源目录结构之上的分类路径。
	//
	// 选"叠加"而非"替换"：替换会丢掉源目录本身的层级信息
	//（导入 ~/docs/go/x.md 配 category=tech 得到 tech/x，
	//  而"go" 这一层正是这份资料最有价值的组织信息）。
	Category string

	// DryRun 只统计不落盘。
	DryRun bool

	// MaxItems 限制本次导入条目数，0 用默认值。
	// 防呆：agent 误传 "/" 或整个家目录时不会一次把盘灌满。
	MaxItems int

	// IncludeMedia 是否把图片/音视频一并导入（进媒体库、按 digest 引用）。
	IncludeMedia bool
}

type ImportStats struct {
	// Imported 成功写入的条目数。
	Imported int
	// Skipped 因同名冲突跳过的条目数（**不覆盖已有知识**）。
	Skipped int
	// Failed 失败的条目数。
	Failed int
	// Media 是导入的媒体文件数。
	Media int
	// Truncated 表示因 MaxItems 被截断（agent 据此知道"没导完"）。
	Truncated bool
	// Names 是本次导入的知识名（dry_run 与成功路径都给，便于 agent 复核）。
	Names []string
	// Errors 记前若干条失败原因（不全量：失败成百上千时返回上万行没意义）。
	Errors []string
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

type FusedHit struct {
	BlockID   string
	Text      string
	Score     float64 // 统一分数
	VectorHit float64 // 向量路分数（0 = 未命中）
	SymbolHit float64 // 符号路分数（0 = 未命中）
	// ExactHit 表示「查询里的精确数字串/版本串命中了本块」。
	//
	// ★ 这是**布尔信号**不是强度信号：它的价值在于「存在与否」。
	//   混进加权和会被低权重稀释（实测：sym=1.0×0.3=0.30
	//   输给 vec=0.62 + sym=0.33 → 0.534），所以单独一路置顶。
	ExactHit bool
	Why      []string // 可读的归因，给调试与模型解释用
}

// Vectorizer 是文本向量化能力（稀疏词向量，TF-IDF/倒排检索用）。
//
// 与 MultimodalEmbedder 刻意区分：后者是稠密共享空间，用于媒体块的余弦召回。
// MultimodalEmbedder 是稠密多模态编码器接口（内嵌 ONNX 或外部 HTTP 服务均可）。
type MultimodalEmbedder interface {
	VectorizeDense(text string) ([]float64, error)
	EmbedImageDense(img []byte, mime string) ([]float64, error)
	Fingerprint() string
	Dim() int
	Loaded() bool
	Close()
}

type Vectorizer interface {
	Vectorize(text string) Vector
	EmbedImage(img []byte, mime string) (Vector, error)
}

// Vector 是带权特征映射：feature → weight（即 internal/tfidf.Vector 的别名）。
type Vector = tfidf.Vector

type AbstainReason struct {
	Query   string
	Ratio   float64
	Matched []string
	Notice  string
}

type ArbitrationResult struct {
	// Kept 是仲裁后保留的块（按时间升序）。
	Kept []BlockHit
	// Superseded 是被取代的块（按时间降序，最早的在前）。
	Superseded []BlockHit
	// Status 是仲裁结论，用于日志与调试。
	Status string
}

type PersonProfile struct {
	Name      string            `json:"name"`
	Traits    map[string]string `json:"traits,omitempty"`
	Relations []SocialRelation  `json:"relations,omitempty"`
}

type SocialRelation struct {
	Person   string `json:"person"`
	Relation string `json:"relation"` // 关系类型：朋友/家人/同事/邻居/...
}

// ───────────────────────── 事件总线口径 ─────────────────────────

// EventType 是事件类型（字符串，允许插件自定义）。
type EventType string

// Event 是总线上的一条事件。
type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

// Publisher 是发布/订阅的主体身份（Name 空 = 内核内部）。
type Publisher struct {
	Name string
}

// ───────────────────────── 安全闸门口径 ─────────────────────────

// Direction 是数据流方向。
type Direction string

const (
	DirectionOutbound Direction = "outbound"
	DirectionInbound  Direction = "inbound"
)

// Inspection 是一次过境点的描述（审计与策略定位用）。
type Inspection struct {
	Direction Direction
	Stage     string
	Tool      string
	Channel   string
	AgentID   string
	Source    string
}

// Decision 是策略判定结论。
type Decision struct {
	Allowed bool
	Reason  string
}

// FindingKind 是内容审查命中的类别。
type FindingKind string

// Finding 是一条内容审查命中（不含原文，只记规则与位置）。
type Finding struct {
	Kind   FindingKind `json:"kind"`
	Where  string      `json:"where"`
	Rule   string      `json:"rule,omitempty"`
	Offset int         `json:"offset,omitempty"`
	Length int         `json:"length,omitempty"`
	At     string      `json:"at,omitempty"`
}

// RoundInput 是一轮输入的策略声明（通道/说话人/档位）。
type RoundInput struct {
	Channel string
	UserID  string
	GroupID string
	Profile string
	AgentID string
	IsChild bool
	Source  string
}

// Evt 是一次明文→别名的替换记录（变量库）。
type Evt struct {
	Alias  string `json:"alias"`
	Kind   string `json:"kind"`
	Where  string `json:"where"`
	Len    int    `json:"len"`
	Merged bool   `json:"merged"`
	At     string `json:"at"`
}

// SecretInfo 是变量库里一条别名的元信息（不含明文）。
type SecretInfo struct {
	Alias  string `json:"alias"`
	Source string `json:"source"`
	Note   string `json:"note"`
	At     string `json:"at"`
}

// ───────────────────────── 变更追踪口径 ─────────────────────────

// TrackedChange 是 PreAction/PostAction 的窄口径（agent 侧只需要知道
// 「这次动作改了哪几个文件」，不需要 tracker 的完整变更记录）。
//
// ★ 之所以不把 tracker 的 ChangeSet 搬进本包：它经 internal/sdk 转发，
// 属于实现侧契约（含 Type/Size/Hash/Content 等回滚字段）。本包只放
// **接口必须收发**的值类型 —— 少放一个就少一处耦合。
type TrackedChange struct {
	Action string
	Files  []string
}

// Knowledge 是知识库里的一条条目（DTO 声明在 kernelapi/model）。
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
	Media []KnowledgeMediaRef `json:"-"`
}
