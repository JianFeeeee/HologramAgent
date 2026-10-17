package sdk

import (
	"time"

	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// MemoryBlockView 是块的最小只读视图（供星图轮询端点）。
//
// ★ 不直接用 memory.MemoryBlock：那是内核内部类型，而本包是**插件边界**。
//
//	SDK 接口只暴露端点真正要用的字段（原来是先 GraphData 全量再转成
//	graphBlockView 做同样的事，只是绕了一圈）。
type MemoryBlockView struct {
	ID        string    `json:"id"`
	Modality  string    `json:"modality"`
	Text      string    `json:"text,omitempty"`
	HasData   bool      `json:"has_data"` // 媒体负载在块内（CAS 淘汰后），只报有无不回显内容
	MIME      string    `json:"mime,omitempty"`
	Source    string    `json:"source,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Scene     string    `json:"scene,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// MemoryAPI 是内置插件使用的全量图记忆接口。
type MemoryAPI interface {
	pubsdk.MemoryAPI
	// GraphData 返回整个知识图谱的完整快照。
	GraphData() (map[string]interface{}, error)
	// BlocksChangedSince 返回自 since 之后创建或更新过的块（供星图轻量轮询）。
	//
	// ★ 为何单开一个方法而不复用 GraphData：后者是全量快照（生产实测
	//   3190 块 + 2692 边，JSON 约 3.3MB、135ms），而轮询端点只想知「有新东西」。
	//   原先 pulse 端点是先取全量再过滤 —— 实测与全量端点同价（145ms），
	//   「轻量端点」名不副实。库侧 SQL 过滤后响应体差两个数量级。
	//
	// 属内置接口（非 pubsdk），与 GraphData 同理，不影响外部插件。
	BlocksChangedSince(since time.Time, limit int) ([]MemoryBlockView, error)
	// MediaBytes 按**块引用**取回媒体块的字节与 MIME。
	//
	// ★ 这是 CAS 淘汰后插件取媒体的唯一入口：块 ID 就是媒体的身份
	//   （内容派生 blk_media_<hash>），不再有独立的 digest 寻址层。
	//   ref 可以是完整块 ID，也可以是唯一前缀（模型/跨进程只带短引用）。
	//
	// 取不到（无唯一对应块、或块无负载）时返回 (nil, "", false, nil) ——
	// 「这个引用没有媒体」是调用方能自己处理的常态，不是错误。
	MediaBytes(ref string) (data []byte, mime string, ok bool, err error)
}

type Entity = pubsdk.Entity
type Relation = pubsdk.Relation
type Triple = pubsdk.Triple

// TextMemoryAPI 是内置插件使用的全量文本记忆接口。
type TextMemoryAPI interface {
	pubsdk.TextMemoryAPI
	// RecentEvents 返回最近 n 条记忆事件。
	RecentEvents(n int) ([]TextEvent, error)
	// Stats 返回文本记忆的运行统计。
	Stats() map[string]interface{}
}

type TextEvent = pubsdk.TextEvent

// MediaAttachment 是记忆附件（媒体）在插件边界上的表示。
// 与公共 SDK 同一类型，内置插件与外部插件用同一套字段。
type MediaAttachment = pubsdk.MediaAttachment

type DocMemoryAPI = pubsdk.DocMemoryAPI
type Doc = pubsdk.Doc
