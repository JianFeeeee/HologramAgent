package sdk

// MemoryAPI provides access to the graph memory (entity-relation store).
type MemoryAPI interface {
	Recall(query []string, depth int) ([]Entity, []Relation, error)
	Commit(triples []Triple) error
	Introspect() (map[string]interface{}, error)
	MergeEntities(source, target string) (int, error)
	Purge(criteria map[string]string, mode string) (int, error)
}

// Entity represents a named entity in the knowledge graph.
type Entity struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	MentionCount int    `json:"mention_count"`
}

// Relation represents a relationship between two entities.
type Relation struct {
	SourceName   string  `json:"source_name"`
	TargetName   string  `json:"target_name"`
	RelationType string  `json:"relation_type"`
	Confidence   float64 `json:"confidence,omitempty"`
}

// Triple represents a subject-relation-object triple for the knowledge graph.
//
// SentenceText 是这条三元组的原句，会写进 sentences 表；媒体引用挂在句子上，
// 所以 MediaDigests 非空时内核会保证句子存在（不给就自动合成一句）。
type Triple struct {
	Subject      string   `json:"subject"`
	Relation     string   `json:"relation"`
	Object       string   `json:"object"`
	Confidence   float64  `json:"confidence,omitempty"`
	SubjectType  string   `json:"subject_type,omitempty"`
	ObjectType   string   `json:"object_type,omitempty"`
	SentenceText string   `json:"sentence_text,omitempty"`
	MediaDigests []string `json:"media_digests,omitempty"`
}

// TextMemoryAPI provides access to chronological text event storage.
type TextMemoryAPI interface {
	Append(evt TextEvent) error
}

// TextEvent represents a single text memory event.
// MediaAttachment 描述一份与记忆关联的媒体。
//
// 两个方向共用一个类型：
//   - 写入（InsertWithMedia）：给 Data + MIME 就是新内容；只给 Digest 则是引用已有内容。
//   - 读出（Query）：内核只填 Digest/MIME，**不回 Data**——
//     一次检索可能命中几十张图，把字节全塞回插件会把 ABI 消息撑爆。
//     需要字节时拿 Digest 单独取。
//
// 刻意没有 Description 字段：媒体不作为文本被索引，也不带任何生成的描述。
// 它只按自己的原生向量被检索与召回；附加文字请写在文档 / 三元组的文本里。
type MediaAttachment struct {
	Digest string `json:"digest,omitempty"`
	MIME   string `json:"mime,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Name   string `json:"name,omitempty"`
}

type TextEvent struct {
	Role        string            `json:"role"`
	Content     string            `json:"content"`
	Timestamp   int64             `json:"timestamp"`
	Channel     string            `json:"channel,omitempty"`
	Attachments []MediaAttachment `json:"attachments,omitempty"`
}

// DocMemoryAPI provides access to the document vector store.
type DocMemoryAPI interface {
	Query(text string, topK int) []*Doc
	Insert(doc *Doc) error
	// InsertWithMedia 写入文档并关联媒体。attachments 里带 Data 的会落进
	// 内容寻址存储（相同字节只存一份），只带 Digest 的直接引用已有内容。
	// 媒体成为文档直接持有的一等记忆块：文档向量会融合它们的原生向量，
	// 因此图片按自己的向量被召回，不依赖任何生成的描述文本。
	InsertWithMedia(doc *Doc, attachments []MediaAttachment) error
	Remove(id string)
	Stats() map[string]interface{}
}

// Doc represents a document in the document store.
//
// MediaDigests / Attachments 在 Query 返回时由内核填充（仅元数据，不带字节）。
type Doc struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Content      string            `json:"content"`
	Score        float64           `json:"score,omitempty"`
	MediaDigests []string          `json:"media_digests,omitempty"`
	Attachments  []MediaAttachment `json:"attachments,omitempty"`
}

// SocialAPI provides read-only access to the social graph (person profiles and relationships).
// External plugins can query person traits and social networks but cannot modify them.
type SocialAPI interface {
	GetPerson(name string) (*PersonProfile, error)
	GetTrait(name, trait string) (string, bool)
	GetRelations(name string) ([]SocialRelation, error)
	GetNetwork(name string, depth int) ([]*PersonProfile, error)
	ListPersons() ([]string, error)
}

// PersonProfile represents a person's complete profile (traits + social relations).
type PersonProfile struct {
	Name      string            `json:"name"`
	Traits    map[string]string `json:"traits,omitempty"`
	Relations []SocialRelation  `json:"relations,omitempty"`
}

// SocialRelation represents a social relationship between two persons.
type SocialRelation struct {
	Person   string `json:"person"`
	Relation string `json:"relation"`
}
