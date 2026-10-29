package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BlockModality 是一等记忆块的原生模态。
type BlockModality string

const (
	BlockText  BlockModality = "text"
	BlockImage BlockModality = "image"
	BlockVideo BlockModality = "video"
	BlockAudio BlockModality = "audio"
)

// PageKind 是「页」的类型：一页 = 一次完整往返。
//
// 用户裁定（2026-10-29）：
//
//	对 toolcall 来说，页 = toolcall 与其对应的 toolresult
//	对文本来说，    页 = llm 的 input 与其对应的 llm output
//
// 这两类是**渲染正确性的最小单位**：整页留下或整页剔除，
// 永远不会产生"有 call 没 result"的破帧。
type PageKind string

const (
	// PageLLMRound 是一次模型往返（input → output）。
	PageLLMRound PageKind = "llm_round"
	// PageToolRound 是一次工具往返（call → result），靠 CallID 配对。
	PageToolRound PageKind = "tool_round"
)

// BlockRole 是块在**页内**的角色。
//
// 每页拆成 request / response 两块（而不是合成一块）的理由：
// 用户的提问与模型的回答语义常常不同，合成一块后只能有一个向量，
// 检索时无法单独命中"用户问的"或"模型答的"。
type BlockRole string

const (
	// RoleRequest 是页的请求端：llm 的 input，或 tool 的 call（含参数）。
	RoleRequest BlockRole = "request"
	// RoleResponse 是页的响应端：llm 的 output，或 tool 的 result。
	RoleResponse BlockRole = "response"
)

// MemoryBlock 是 Context、Document、Graph 三层共同使用的记忆块值。
//
// 它不携带 Layer、Owner 或 RefCount：块当前由哪个层的容器持有，哪个层就是
// 唯一事实源。Context→Document→Graph 迁移的是这个值本身，不建立平行保活账本。
//
// ★★ 负载是 **union**：一个块要么是文本块（负载在 Text），要么是媒体块
// （负载在 Data，配 MIME/Width/Height）。两者是同一个 MemoryBlock 的两种取值，
// 不是「块 + 外挂字节层」。
//
// 为何不存在独立的字节存储：块已经是内容的载体（文本块自己带正文），媒体块
// 自当同理。把媒体内容拆到块外、只留一个 digest 指向外部 CAS，会凭空多出
// 一层需单独维护生命周期的东西（第二套向量、第二套召回路、引用账本、自己的
// db），并与「块是唯一记忆载体」直接冲突。内容随块同生同灭才是正解。
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

// MemoryBlockEdge 是 L3 中连接一等记忆节点的结构化语义边。
// source/target kind 当前允许 block、entity、sentence、document。
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

// IsRelationEdge 判断该边是否为**关系边**（而非结构边）。
func (e MemoryBlockEdge) IsRelationEdge() bool {
	return e.SessionID != "" || e.Status != "" || e.Confidence != 0
}

func validBlockModality(modality BlockModality) bool {
	return modality == BlockText || modality == BlockImage || modality == BlockVideo || modality == BlockAudio
}

// PutMemoryBlocks 将完成 L2→L3 迁移的块写成 GraphDB 原生节点。
// 调用方只有在本事务成功后才能从 Document 删除这些块。
func (g *GraphDB) PutMemoryBlocks(blocks []MemoryBlock) error {
	if len(blocks) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, block := range blocks {
		if block.ID == "" {
			return fmt.Errorf("memory block id is required")
		}
		if !validBlockModality(block.Modality) {
			return fmt.Errorf("memory block %s has invalid modality %q", block.ID, block.Modality)
		}
		vectorJSON, err := json.Marshal(block.Vector)
		if err != nil {
			return fmt.Errorf("marshal memory block %s vector: %w", block.ID, err)
		}
		now := time.Now()
		if block.CreatedAt.IsZero() {
			block.CreatedAt = now
		}
		_, err = tx.Exec(`INSERT INTO memory_blocks (
			id, modality, text_content, data, mime, size, width, height,
			vector, fingerprint, source, tool, scene, created_at, updated_at,
			frame_ts, page_seq, page_kind, block_role, call_id, inactive,
			last_recalled_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			modality = excluded.modality,
			text_content = excluded.text_content,
			-- ★ 负载空值不覆盖：轻量读取路径（BlockByIDPrefix 等 lean 列 SELECT）
			--   取回的块不带 data/text，重新写回时若直接覆盖会把已存负载抹空。
			data = CASE WHEN excluded.data IS NULL OR length(excluded.data) = 0
				THEN memory_blocks.data ELSE excluded.data END,
			mime = excluded.mime,
			size = excluded.size,
			width = excluded.width,
			height = excluded.height,
			vector = excluded.vector,
			fingerprint = excluded.fingerprint,
			source = excluded.source,
			tool = excluded.tool,
			-- 场景只在本次给了值时才覆盖：块可能先被写入、后被归档路径补挂场景，
			-- 反过来「已挂场景的块被一次无场景的重写抹掉」是不可接受的静默降级。
			scene = CASE WHEN excluded.scene != '' THEN excluded.scene ELSE memory_blocks.scene END,
			-- ★ semantic_type 同样「空值不覆盖」：
			--   后写的三元组往往不带类型（Triple.SubjectType 可选），
			--   若让它覆盖已有类型，一次不带标注的写入就会抹掉语义类别。
			semantic_type = CASE WHEN excluded.semantic_type != '' THEN excluded.semantic_type ELSE memory_blocks.semantic_type END,
			-- ★ 段页式字段一律「空值不覆盖」：轻量读取路径（lean 列 SELECT）
			--   取回的块不带这些字段，写回时若直接覆盖会抹掉页身份。
			--   页身份一旦被抹，块就再也归不回原来的页 —— 渲染会破配对。
			frame_ts = CASE WHEN excluded.frame_ts IS NOT NULL AND excluded.frame_ts != ''
				THEN excluded.frame_ts ELSE memory_blocks.frame_ts END,
			page_seq = CASE WHEN excluded.page_seq > 0 THEN excluded.page_seq ELSE memory_blocks.page_seq END,
			page_kind = CASE WHEN excluded.page_kind != '' THEN excluded.page_kind ELSE memory_blocks.page_kind END,
			block_role = CASE WHEN excluded.block_role != '' THEN excluded.block_role ELSE memory_blocks.block_role END,
			call_id = CASE WHEN excluded.call_id != '' THEN excluded.call_id ELSE memory_blocks.call_id END,
			-- ★ inactive 是「标注」，所以要能被**显式改回 false**（回读后恢复渲染）。
			--   因此它不能走「空值不覆盖」——false 是合法值，必须照写。
			inactive = excluded.inactive,
			-- ★ 召回时间只**前进不后退**（P6）。
			--
			-- 轻量读取路径拿回的块不带 last_recalled_at（零值），
			-- 若直接覆盖就把"曾经被召回过"抹成"从未被召回" ——
			-- 于是这块活数据在回收时被误判为死块删掉。
			-- 保留两者的较大值：NULL/零值不会覆盖已有时间。
			last_recalled_at = CASE
				WHEN excluded.last_recalled_at IS NULL THEN memory_blocks.last_recalled_at
				WHEN memory_blocks.last_recalled_at IS NULL THEN excluded.last_recalled_at
				WHEN excluded.last_recalled_at > memory_blocks.last_recalled_at
					THEN excluded.last_recalled_at
				ELSE memory_blocks.last_recalled_at END,
			updated_at = excluded.updated_at`,
			block.ID, block.Modality, block.Text, block.Data, block.MIME,
			block.Size, block.Width, block.Height, string(vectorJSON), block.Fingerprint,
			block.Source, block.Tool, block.Scene, block.CreatedAt, now,
			frameTSArg(block.FrameTS), block.PageSeq, string(block.PageKind),
			string(block.BlockRole), block.CallID, block.Inactive,
			frameTSArg(block.LastRecalledAt))
		if err != nil {
			return fmt.Errorf("put memory block %s: %w", block.ID, err)
		}
		// 场景引用与块同事务：块写进去了、引用丢了，这个块在场景里就永远取不回。
		if block.Scene != "" {
			if err := tagSceneRefTx(tx, block.Scene, "block", 0, block.ID, 1.0); err != nil {
				return fmt.Errorf("tag scene for block %s: %w", block.ID, err)
			}
		}
	}
	return tx.Commit()
}

// PutDocumentNode 在 L3 登记一个文档节点，作为 document --contains--> block
// 结构边的端点。文档正文已蒸馏为实体/关系，这里只保留身份与摘要。
func (g *GraphDB) PutDocumentNode(id, summary string) error {
	if id == "" {
		return fmt.Errorf("document node id is required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err := g.db.Exec(`INSERT INTO documents (id, summary) VALUES (?, ?)
		ON CONFLICT(id) DO UPDATE SET summary = excluded.summary`, id, summary)
	return err
}

// MemoryBlocks 查询 Graph 层实际持有的一等记忆节点。
// BlocksChangedSince 返回自 since 之后**创建或更新过**的块（供星图轻量轮询）。
//
// ★ 为何需要专方法，而不是复用 GraphData() 再过滤（2026-10-08 实测）：
//
//	/memory/graph/pulse 本应是**轻量**端点，原实现却先调 GraphData()
//	（全量 3190 块 + 2692 边）→ json.Marshal 约 3.3MB → 再反序列化 → 才按时间
//	过滤。实测它与全量端点**同价**（145ms vs 135ms），而它每 10s 被轮询一次。
//
// 这里直接用 SQL 在库侧过滤，响应体只含窗口内变动的块（几百字节～几 KB，
// 与全量差两个数量级）。
//
// limit 上限保护：窗口内也可能有大量块（如一次批量迁移），
// 而星图轮询只需知道“有新东西”，不需要全部。0 表示用默认上限。
func (g *GraphDB) BlocksChangedSince(since time.Time, limit int) ([]MemoryBlock, error) {
	if limit <= 0 {
		limit = 200
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT `+blockColumns+`
		FROM memory_blocks
		WHERE created_at >= ? OR updated_at >= ?
		ORDER BY updated_at DESC LIMIT ?`, since, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blocks []MemoryBlock
	for rows.Next() {
		block, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

func (g *GraphDB) MemoryBlocks() ([]MemoryBlock, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT ` + blockColumns + `
		FROM memory_blocks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blocks []MemoryBlock
	for rows.Next() {
		block, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

// scanBlockRow 读一行 memory_blocks。
//
// ★★ 列顺序与个数必须与**每个**调用它的 SELECT 完全一致
// -----------------------------------------------
// 共用扫描的代价是：SELECT 少列就整体读不出（不是某字段零值，而是
// 整行 Scan 失败 ⇒ 该路径静默返回空）。
//
// 实测踩过：MemoryBlocks() 的 SELECT 少了 scene 列（14 列），
// 而 block_recall.go / edge_entity.go 是 15 列，换成共用扫描后
// 三处一起读不出块 —— 症状是「召回返回未找到相关记忆」，
// 完全看不出是列数问题。
//
// ⇒ 改任何 memory_blocks 的 SELECT 时，**数一遍列**。
//
// ★ 提取成共用函数的原因
// ------------------
// BFSBlocks（edge_entity.go）也要读块。若它自己写一份 Scan，
// 将来加列时**两处都要改** —— 而我只加了 schema 列忘了扩读取端
// 已经犯过一次（属性静默变零值，被 TestEdge_承载关系属性 抓到）。
// 共用扫描让「加列漏改」不再可能。
// blockColumns 是 memory_blocks 的完整列清单，**必须与 scanBlockRow 的
// Scan 顺序逐列一致**。
//
// ★★★ 加列时的必读（2026-10-04 实测踩了 6 次）
//
// 加 semantic_type 那一次，**四处 SELECT 是手写列清单**（不是常量），
// 两处 Scan 也是手写 —— 于是：
//
//	「sql: expected 16 destination arguments in Scan, not 15」
//
// ★ 这个错**只在真跑 SQL 时暴露**，编译期完全无感；
//
//	而且它表现为「召回突然空了」而非「读失败」，
//	很容易被误判成召回逻辑坏了而查错方向。
//
// ⇒ 本文件的 SELECT 一律用 blockColumns，不要再手写。
// ⇒ 加列后必须做两件事：
//  1. `grep -rn 'FROM memory_blocks' --include='*.go'` 确认无手写清单
//  2. `grep -rn '&b.Scene,\|&scene,' --include='*.go'` 确认所有 Scan 已同步
//
// ★ 为什么要提成常量：2026-10-04 修过一次由此引发的生产 bug ——
//
//	MemoryBlocks() 的 SELECT 漏了 scene 列，而它与 scanBlockRow 共用，
//	结果媒体与召回测试大面积变红（列数不匹配 ⇒ 读失败）。
//
//	单靠“记得同步”不可靠；提成常量后新增查询直接复用。
//
// blockColumns 是 memory_blocks 的**元数据**列清单，供所有常规读取复用。
//
// ★ 为何**不含 data**：媒体负载动辄数 MB（生产实测：图均 495KB、最大 2.7MB），
// 而 RecallBlocks 要全表扫描有向量的块。若把 data 列进来，每次召回都会把
// 几十 MB 字节读进内存。块拥有自己的负载（同一行、同一生命周期），
// 但负载**按需加载**（见 BlockData）——这与被淘汰的 CAS 有本质区别：
// 那里字节归另一个存储层所有，这里归块自己。
const blockColumns = `id, modality, text_content, mime, size, width, height,
			vector, fingerprint, source, tool, scene, semantic_type, created_at, updated_at,
			frame_ts, page_seq, page_kind, block_role, call_id, inactive, last_recalled_at`

func scanBlockRow(rows *sql.Rows) (MemoryBlock, error) {
	var b MemoryBlock
	var vectorJSON string
	var scene sql.NullString
	// 段页式字段一律用 Null* 扫：存量行的 frame_ts 是 NULL、其余有 DEFAULT，
	// 直接扫进零值类型会在 NULL 上报错（sql: Scan error on column "frame_ts"）。
	var frameTS sql.NullTime
	var pageSeq sql.NullInt64
	var pageKind, blockRole, callID sql.NullString
	var inactive sql.NullInt64
	var lastRecalled sql.NullTime
	if err := rows.Scan(&b.ID, &b.Modality, &b.Text,
		&b.MIME, &b.Size, &b.Width, &b.Height, &vectorJSON,
		&b.Fingerprint, &b.Source, &b.Tool, &scene, &b.SemanticType,
		&b.CreatedAt, &b.UpdatedAt,
		&frameTS, &pageSeq, &pageKind, &blockRole, &callID, &inactive,
		&lastRecalled); err != nil {
		return b, err
	}
	if lastRecalled.Valid {
		b.LastRecalledAt = lastRecalled.Time
	}
	b.Scene = scene.String
	if frameTS.Valid {
		b.FrameTS = frameTS.Time
	}
	b.PageSeq = int(pageSeq.Int64)
	b.PageKind = PageKind(pageKind.String)
	b.BlockRole = BlockRole(blockRole.String)
	b.CallID = callID.String
	b.Inactive = inactive.Int64 != 0
	if vectorJSON != "" && vectorJSON != "null" {
		if err := json.Unmarshal([]byte(vectorJSON), &b.Vector); err != nil {
			return b, fmt.Errorf("decode memory block %s vector: %w", b.ID, err)
		}
	}
	return b, nil
}

func validGraphNodeKind(kind string) bool {
	return kind == "block" || kind == "entity" || kind == "sentence" || kind == "document"
}

func graphNodeExists(tx *sql.Tx, kind, id string) (bool, error) {
	var n int
	var err error
	switch kind {
	case "block":
		err = tx.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, id).Scan(&n)
	case "entity":
		err = tx.QueryRow(`SELECT COUNT(*) FROM entities WHERE CAST(id AS TEXT) = ?`, id).Scan(&n)
	case "sentence":
		err = tx.QueryRow(`SELECT COUNT(*) FROM sentences WHERE CAST(id AS TEXT) = ?`, id).Scan(&n)
	case "document":
		err = tx.QueryRow(`SELECT COUNT(*) FROM documents WHERE id = ?`, id).Scan(&n)
	default:
		return false, fmt.Errorf("invalid graph node kind %q", kind)
	}
	return n == 1, err
}

// AddMemoryBlockEdge 建立 contains、depicts、derived_from 等原生图边。
// 端点必须是真实 Graph 节点，不能用 owner 字符串伪装关系。
func (g *GraphDB) AddMemoryBlockEdge(sourceKind, sourceID, targetKind, targetID, edgeType string) error {
	if !validGraphNodeKind(sourceKind) || !validGraphNodeKind(targetKind) {
		return fmt.Errorf("invalid memory block edge kinds %q -> %q", sourceKind, targetKind)
	}
	if sourceID == "" || targetID == "" || edgeType == "" {
		return fmt.Errorf("memory block edge endpoints and type are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, endpoint := range []struct{ kind, id string }{{sourceKind, sourceID}, {targetKind, targetID}} {
		exists, err := graphNodeExists(tx, endpoint.kind, endpoint.id)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%s graph node %s does not exist", endpoint.kind, endpoint.id)
		}
	}
	// ★★ 结构边必须**显式查重** —— 不能再依赖 INSERT OR IGNORE
	//
	// 边表升格去掉了 UNIQUE(source_kind,source_id,target_kind,target_id,
	// edge_type)（因为它让「同一对节点并存多条同类关系边」不可能）。
	// 而 OR IGNORE 的去重**正是靠那个 UNIQUE 实现的** ——
	// 约束一去，重复调用就会插出两条一样的 contains 边。
	//
	// 实测后果：distill 幂等与迁移幂等两个判据当场红了。
	//
	// ⇒ 结构边（contains 等）在这里显式查；关系边走
	//   AddRelationBlockEdge，它**刻意**不查重（可并存多条）。
	var existing int
	err = tx.QueryRow(`SELECT COUNT(*) FROM memory_block_edges
		WHERE source_kind=? AND source_id=? AND target_kind=? AND target_id=?
		  AND edge_type=? AND COALESCE(session_id,'')=''`,
		sourceKind, sourceID, targetKind, targetID, edgeType).Scan(&existing)
	if err != nil {
		return err
	}
	if existing > 0 {
		return nil // 已存在：结构边幂等
	}

	if _, err = tx.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type)
		VALUES (?, ?, ?, ?, ?)`, sourceKind, sourceID, targetKind, targetID, edgeType); err != nil {
		return err
	}
	return tx.Commit()
}

func (g *GraphDB) MemoryBlockEdges() ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	// ★ 必须读全部 11 列 —— 只读旧 6 列会让关系边的属性
	//   （session_id / confidence / status）静默变成零值。
	//   这正是第一版加列时漏掉的地方：写了列但没扩读取端。
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
		edge_type, created_at, confidence, session_id, turn_id, status, merged_into
		FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []MemoryBlockEdge
	for rows.Next() {
		var edge MemoryBlockEdge
		var sess, status, merged sql.NullString
		var conf sql.NullFloat64
		var turn sql.NullInt64
		if err := rows.Scan(&edge.ID, &edge.SourceKind, &edge.SourceID, &edge.TargetKind,
			&edge.TargetID, &edge.Type, &edge.CreatedAt,
			&conf, &sess, &turn, &status, &merged); err != nil {
			return nil, err
		}
		edge.Confidence = conf.Float64
		edge.SessionID = sess.String
		edge.Status = status.String
		edge.MergedInto = merged.String
		if turn.Valid {
			edge.TurnID = int(turn.Int64)
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

// BlocksForNode 返回与某个图节点通过任意边相连的一等记忆块。
// 例：sentence --contains--> block；entity --depicts--> block。
func (g *GraphDB) BlocksForNode(nodeKind, nodeID string) ([]MemoryBlock, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT b.id, b.modality, b.text_content,
		b.mime, b.size, b.width, b.height, b.vector, b.fingerprint, b.source, b.tool,
		b.created_at, b.updated_at
		FROM memory_block_edges e
		JOIN memory_blocks b ON (
			(e.source_kind = 'block' AND e.source_id = b.id AND e.target_kind = ? AND e.target_id = ?)
			OR (e.target_kind = 'block' AND e.target_id = b.id AND e.source_kind = ? AND e.source_id = ?))
		ORDER BY b.created_at, b.id`, nodeKind, nodeID, nodeKind, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blocks []MemoryBlock
	for rows.Next() {
		var block MemoryBlock
		var vectorJSON string
		if err := rows.Scan(&block.ID, &block.Modality, &block.Text,
			&block.MIME, &block.Size, &block.Width, &block.Height, &vectorJSON,
			&block.Fingerprint, &block.Source, &block.Tool, &block.CreatedAt,
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
	return blocks, rows.Err()
}

// addContainsEdgeTx 在事务内建一条**结构边**（原句块 → 关系边）。
//
// ★ 为什么不能用 AddRelationBlockEdge：它刻意不查重（同对节点可并存多条
//
//	同类型关系边，这是 52e4596 的设计）。结构边语义相反 —— 同一句与同一条
//	关系之间只应有一条 contains，重复插入会让 BFS 的邻接表里出现重复项。
//
// ★ 为什么不能用 addBlockEdgeTx：它是迁移专用的 INSERT OR IGNORE 版本，
//
//	端点校验走的是旧表。
//
// ★ 端点允许是**边 ID**：原句块指向的是关系边（memory_block_edges.id），
//
//	所以 targetKind 用 'edge'。graphNodeExists 已支持该 kind。
//
// ★ 幂等：重复写入静默跳过（返回 nil），让重试安全。
func addContainsEdgeTx(tx *sql.Tx, sentenceBlockID string, edgeID int64, weight float64) error {
	if sentenceBlockID == "" || edgeID == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM memory_block_edges
		WHERE source_kind='block' AND source_id=?
		  AND target_kind='edge' AND target_id=?
		  AND edge_type='contains' AND COALESCE(session_id,'')=''`,
		sentenceBlockID, edgeID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type,
		 confidence, status)
		VALUES ('block', ?, 'edge', ?, 'contains', ?, 'active')`,
		sentenceBlockID, edgeID, weight)
	return err
}

// ═══════════════════════════════════════════════════════════════
//  读方切换用的薄包装（2026-10-04）
//
//  这三个函数存在的唯一理由：让 indexer / social / distill / Purge
//  能从旧表（entities/relations/sentences）迁到块体系。
//
//  ★ 它们**刻意不做语义加工** —— 只做「按名找块」「枚举块名」
//  「取某块的邻居边」这三件最朴素的事。
//    任何排序、过滤、仲裁、召回打分都该在调用方做，
//    否则同一个语义会散落在两处，然后各自漂移。
//
//  ★ 全部走 g.mu，所以调用方**不得**已持锁。
// ═══════════════════════════════════════════════════════════════

// BlockByText 按文本取块（精确匹配）。
//
// 用途对应旧表的 `WHERE name = ?` —— 旧表的 name 是实体名，
// 块侧的等价物是 text_content。
//
// ★ 为什么用 = 而不是 LIKE：旧表 name 是**规范化过的实体名**
//
//	（Commit 走 TripleBlockID(t.Subject) 这样的内容派生 ID），
//	精确匹配能命中；模糊匹配会把「值班室分机号」与「值班室分机号（旧）」
//	混为一谈，而那正是仲裁要区分的东西。
//
// ★ 不区分 modality：媒体块的 text_content 常为空，
//
//	空串匹配不到任何真实查询（调用方不会传空串查）。
func (g *GraphDB) BlockByText(text string) (*MemoryBlock, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	blocks, err := g.blocksByTextTx(text)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	// 多个块同文本时取**最早**的：TripleBlockID 是内容派生的，
	// 理论上一个文本一个块；重复只可能来自迁移的历史数据，
	// 此时取最早的（它的 created_at 来自源实体，语义上更接近原意）。
	b := blocks[0]
	return &b, nil
}

// blocksByTextTx 按文本取全部同文本块（调用方已持锁）。
func (g *GraphDB) blocksByTextTx(text string) ([]MemoryBlock, error) {
	rows, err := g.db.Query(
		`SELECT `+blockColumns+` FROM memory_blocks
		 WHERE text_content = ? ORDER BY created_at ASC, id ASC`, text)
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

// BlockTextsLike 返回文本匹配 LIKE 的块（大小写不敏感，%value% 包裹）。
//
// 用途对应旧表的 `WHERE LOWER(name) LIKE ?`。
// ★ 调用方自己拼 LIKE 还是传裸子串？**传裸子串** ——
//
//	包裹在这里做，避免每个调用方各写各的 %…% 然后漏掉转义。
//
// ★ 不做 N 限制：调用方要么自己限，要么要全量（建索引类场景）。
//
//	但 ⚠️ 全表 LIKE 在大库上是 O(n) —— 调用方需自行评估。
func (g *GraphDB) BlockTextsLike(fragment string) ([]MemoryBlock, error) {
	fragment = strings.TrimSpace(fragment)
	if fragment == "" {
		return nil, nil
	}
	pattern := "%" + escapeLike(fragment) + "%"
	g.mu.Lock()
	defer g.mu.Unlock()

	rows, err := g.db.Query(
		`SELECT `+blockColumns+` FROM memory_blocks
		 WHERE text_content LIKE ? ESCAPE '\'
		 ORDER BY created_at ASC, id ASC`, pattern)
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

// escapeLike 转义 LIKE 的元字符（% _ 与 ESCAPE 自身）。
//
// ★ 不转义的话，「50%」这类文本会变成通配符，
//
//	在 Purge 的删除路径上就是**误删**。
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// AllBlockTexts 返回全部块的文本（去重，按出现顺序）。
//
// 用途对应旧表的「全量实体名」—— 建向量索引（indexer）与
// 记忆整理（distill）都需要这个。
//
// ★ 去重：同名实体在旧表是**多行**（mention_count 不同），
//
//	而块侧的 TripleBlockID 是内容派生 ⇒ 同文本本应只有一个块。
//	去重让调用方不必处理重复，也顺带掩盖迁移期的历史重复。
//
// ★ 分批读取：全表取进内存在百万块级会炸。
//
//	batchSize<=0 时用默认批大小。
func (g *GraphDB) AllBlockTexts(batchSize int) ([]string, error) {
	if batchSize <= 0 {
		batchSize = 5000
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	var out []string
	seen := make(map[string]bool)
	var lastID string
	for {
		rows, err := g.db.Query(
			`SELECT id, text_content FROM memory_blocks
			 WHERE id > ? AND text_content != ''
			 ORDER BY id LIMIT ?`, lastID, batchSize)
		if err != nil {
			return nil, err
		}
		n := 0
		var nextID string
		for rows.Next() {
			var id, text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return nil, err
			}
			nextID = id
			n++
			if seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, text)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if n < batchSize {
			break
		}
		lastID = nextID
	}
	return out, nil
}

// NeighbourEdgesOfBlock 返回以该块为端点的全部 active 关系边及其对端块。
//
// 用途对应旧表的「按实体名找它的所有关系」—— social 层按人/物聚合
// 社交关系时用它。
//
// ★ 双向：块作为 source 或 target 都算。
// ★ 只取非 deleted 的边（与全部读路径的 status 口径一致）。
//
// ★ 返回顺序按创建时间 —— 稳定的输出对测试与展示都重要，
//
//	而 SQLite 不保证无 ORDER BY 的行序。
func (g *GraphDB) NeighbourEdgesOfBlock(blockID string) ([]BlockNeighbour, []MemoryBlock, error) {
	if blockID == "" {
		return nil, nil, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	rows, err := g.db.Query(
		`SELECT e.id, e.source_id, e.target_id, e.edge_type,
		        COALESCE(e.confidence, 0), COALESCE(e.session_id, ''),
		        COALESCE(e.turn_id, 0), COALESCE(e.status, ''),
		        COALESCE(e.created_at, ''),
		        COALESCE(other.id, ''), COALESCE(other.text_content, '')
		 FROM memory_block_edges e
		 LEFT JOIN memory_blocks other
		   ON other.id = CASE WHEN e.source_id = ? THEN e.target_id ELSE e.source_id END
		 WHERE (e.source_id = ? OR e.target_id = ?)
		   AND e.source_kind = 'block' AND e.target_kind = 'block'
		   AND COALESCE(e.status, '') != 'deleted'
		 ORDER BY e.created_at ASC, e.id ASC`, blockID, blockID, blockID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var edges []BlockNeighbour
	var peers []MemoryBlock
	seenPeer := make(map[string]bool)
	for rows.Next() {
		var nb BlockNeighbour
		var otherID, otherText, created string
		if err := rows.Scan(&nb.EdgeID, &nb.SourceID, &nb.TargetID, &nb.EdgeType,
			&nb.Confidence, &nb.SessionID, &nb.TurnID, &nb.Status, &created,
			&otherID, &otherText); err != nil {
			return nil, nil, err
		}
		nb.IsOutgoing = nb.SourceID == blockID
		nb.Peer = MemoryBlock{
			ID:        otherID,
			Modality:  BlockText,
			Text:      otherText,
			CreatedAt: parseLegacyTime(created),
			UpdatedAt: parseLegacyTime(created),
		}
		nb.RelationEdgeData = RelationEdgeData{
			Confidence: nb.Confidence,
			SessionID:  nb.SessionID,
			TurnID:     nb.TurnID,
			Status:     nb.Status,
		}
		edges = append(edges, nb)
		if otherText != "" && !seenPeer[otherID] {
			seenPeer[otherID] = true
			peers = append(peers, nb.Peer)
		}
	}
	return edges, peers, rows.Err()
}

// blockSemanticType 返回块对外呈现的语义类型。
//
// ★ 为什么要这个函数而不是直接用 b.SemanticType：
//
//	语义类型是**可选标注**，绝大多数块没被标注过。
//	旧 entities 表在缺省时写 "Concept"（graph.go 里 subjType 默认值），
//	所以读侧（social 的 ListPersons、SDK 的实体类型）期待一个具体字符串。
//	返回空串会让下游的 == "Person" 之类的比较全部落空 ——
//	那不是「未标注」，那像是「数据坏了」。
//
// ★ 取值优先级：
//  1. 块自带标注（Person / Animal / Concept…）—— 直接用
//  2. 未标注 → "block"（说清「这是一个图节点，且没标类型」）
//     ★ 不用 "Concept"：那是旧表的默认值，
//     拿来兜底会让「未标注」与「确实是概念」无法区分。
func blockSemanticType(b MemoryBlock) string {
	if t := strings.TrimSpace(b.SemanticType); t != "" {
		return t
	}
	return "block"
}

// MemoryBlockCount 返回当前块总数。
//
// ★ 用途：判断「一次写入是否真的落库了」
//
// 停旧表双写后，Commit 的两个返回值恒为 0（它们数的是旧表行数），
// 于是「有没有写进去」失去了信号 ——
// 而 archiveColdDocs 正需要它来决定文档能否删除
// （没写进去就删 = 丢数据）。
//
// ★ 为什么用块数而不是「本次新增块数」：
//
//	新增数需要事务内计数，而 MemoryBlockCount 是事务外的简单计数。
//	调用方在写入前后各取一次相减即可，误差只来自并发写入 ——
//	归档是后台低频任务，这个精度足够。
func (g *GraphDB) MemoryBlockCount() (int, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MemoryEdgeCount 返回关系边数（不含 contains 结构边）。
//
// ★ 为什么需要它（2026-10-05）
//
//	旧表停双写后 Commit 的 ec/rc 恒为 0（graph.go 里
//	relationsCreated = 0 是写死的），所以「写了多少条关系」
//	唯一的可信来源只能是**实测边数差值**。
//
//	不含 contains：那是结构边（原句块 → 字段块 / 关系边），
//	混进来会让「关系数」随原句数量翻倍 —— 与 Introspect 的
//	relation_count 口径保持一致。
func (g *GraphDB) MemoryEdgeCount() (int, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_block_edges
		 WHERE source_kind = 'block' AND target_kind = 'block'
		   AND edge_type != 'contains'`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// frameTSArg 把零值 FrameTS 写成 NULL 而不是 "0001-01-01"。
//
// 为什么必须区分：零值表示「旧数据/未分帧」，与真实时间戳不是一回事。
// 若写成 "0001-01-01"，迁移脚本会把它们当成一个真实的极早帧聚到一起，
// 于是所有存量块被错误地归入同一页。
func frameTSArg(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}
