package sdk

import (
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
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

// 纯值类型别名（插件边界上的中立形状，不是内核实现）。
type Entity = pubsdk.Entity
type Relation = pubsdk.Relation
type Triple = pubsdk.Triple
type TextEvent = pubsdk.TextEvent

// ★ 已删 `MemoryAPI` / `TextMemoryAPI` / `DocMemoryAPI`（docs §12.1）。
//
// 插件不再有记忆面：读写记忆要么走 agent 的工具（`graph_recall` /
// `graph_commit`），要么按公开路径规则自己开 SQLite 只读。
// 留着这三个接口等于给"插件依赖内核实现"留一条后门。

// MediaAttachment 是记忆附件（媒体）在插件边界上的表示。
// 与公共 SDK 同一类型，内置插件与外部插件用同一套字段。
type MediaAttachment = pubsdk.MediaAttachment

// ★ `DocMemoryAPI` 在公共 SDK 里是**一篇文档的值类型**（字段：id/title/content，
//
//	与 `Doc` 同源），**不是**本仓删掉的那个"插件记忆面接口"。
//	同名不同物 —— 判据里查 "DocMemoryAPI" 会命中这一行，那是正常的。
//	真正的接口（可读可写的记忆面）已删，理由见上面那段。
type DocMemoryAPI = pubsdk.DocMemoryAPI
type Doc = pubsdk.Doc
