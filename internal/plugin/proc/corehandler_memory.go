package proc

import (
	"encoding/json"
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// handleGraphMemory 处理图记忆（实体/关系）：recall / commit / introspect / merge / purge。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleGraphMemory(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 图记忆（原 case 9/10/11/12/13）----
	case MethodMemoryRecall:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Query []string `json:"query"`
			Depth int      `json:"depth"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		entities, relations, err := mem.Recall(p.Query, p.Depth)
		if err != nil {
			return nil, err
		}
		if entities == nil {
			entities = []pubsdk.Entity{}
		}
		if relations == nil {
			relations = []pubsdk.Relation{}
		}
		return map[string]interface{}{"entities": entities, "relations": relations}, nil

	case MethodMemoryCommit:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Triples []pubsdk.Triple `json:"triples"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, mem.Commit(p.Triples)

	case MethodMemoryIntrospect:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		return mem.Introspect()

	case MethodMemoryMerge:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Source string `json:"source"`
			Target string `json:"target"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		n, err := mem.MergeEntities(p.Source, p.Target)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"merged": n}, nil

	case MethodMemoryPurge:
		mem := h.sdk.Memory()
		if mem == nil {
			return nil, errUnavailable("memory")
		}
		var p struct {
			Criteria map[string]string `json:"criteria"`
			Mode     string            `json:"mode"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Mode == "" {
			p.Mode = "soft"
		}
		n, err := mem.Purge(p.Criteria, p.Mode)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"purged": n}, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleDocMemory 处理文档记忆：query / insert / insertWithMedia / remove / stats。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleDocMemory(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 文档记忆（原 case 14/32/33/34）----
	case MethodDocQuery:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Text string `json:"text"`
			TopK int    `json:"top_k"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		docs := dm.Query(p.Text, p.TopK)
		if docs == nil {
			docs = []*pubsdk.Doc{}
		}
		return map[string]interface{}{"docs": docs}, nil

	case MethodDocInsert:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Doc    *pubsdk.Doc `json:"doc,omitempty"`
			DocRef SharedRef   `json:"doc_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 文档全文可达几十 KB～数 MB，优先走共享内存。
		if err := h.resolveJSONRef(p.DocRef, &p.Doc); err != nil {
			return nil, err
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("doc.insert: 缺少 doc 字段")
		}
		return nil, dm.Insert(p.Doc)

	case MethodDocInsertMedia:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			Doc         *pubsdk.Doc              `json:"doc,omitempty"`
			Attachments []pubsdk.MediaAttachment `json:"attachments,omitempty"`
			DocRef      SharedRef                `json:"doc_ref,omitempty"`
			AttachRef   SharedRef                `json:"attachments_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 文档正文 + 附件（含媒体二进制/data URL）都优先走共享内存。
		if err := h.resolveJSONRef(p.DocRef, &p.Doc); err != nil {
			return nil, err
		}
		if err := h.resolveJSONRef(p.AttachRef, &p.Attachments); err != nil {
			return nil, err
		}
		if p.Doc == nil {
			return nil, fmt.Errorf("doc.insertWithMedia: 缺少 doc 字段")
		}
		if err := dm.InsertWithMedia(p.Doc, p.Attachments); err != nil {
			return nil, err
		}
		// 回传内核补过的字段：ID 新建时才生成，Content 含内核补的媒体标记，
		// MediaDigests 是附件落盘后的完整 digest——插件靠它们后续引用同一份媒体。
		return map[string]interface{}{"doc": p.Doc}, nil

	case MethodDocRemove:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		var p struct {
			ID string `json:"id"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		dm.Remove(p.ID)
		return nil, nil

	case MethodDocStats:
		dm := h.sdk.DocMemory()
		if dm == nil {
			return nil, errUnavailable("doc memory")
		}
		return dm.Stats(), nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// knowledgeMediaAdder 是 knowledge.addWithMedia 需要的扩展能力。
//
// 定义为局部接口而非直接依赖 internal/sdk.KnowledgeAPI：那样会让
// internal/plugin/proc → internal/sdk，而后者已依赖 internal/plugin 的类型，
// 形成循环（CoreSDK 的注释已说明这一点）。断言失败时返回"能力不可用"，
// 而不是静默退化成不写媒体——后者会让调用方以为媒体已入库。
type knowledgeMediaAdder interface {
	AddWithMedia(name, content string, media []knowledge.KnowledgeMediaRef) error
}

// handleKnowledge 处理知识库：search / add / list。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleKnowledge(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 知识库（原 case 15/35/36）----
	case MethodKnowledgeSearch:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		var p struct {
			Query    string `json:"query"`
			TopK     int    `json:"top_k"`
			Category string `json:"category,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 分类限定是内核侧扩展能力（见 internal/sdk/knowledge.go），
		// 用局部接口断言取用；能力缺失时退回全库搜索而不是报错——
		// 那只是"少了个筛选条件"，不是调用失败。
		if p.Category != "" {
			if scoped, ok := kn.(interface {
				SearchIn(query, category string, topK int) ([]*pubsdk.Knowledge, error)
			}); ok {
				results, err := scoped.SearchIn(p.Query, p.Category, p.TopK)
				if err != nil {
					return nil, err
				}
				if results == nil {
					results = []*pubsdk.Knowledge{}
				}
				return map[string]interface{}{"results": results}, nil
			}
		}
		results, err := kn.Search(p.Query, p.TopK)
		if err != nil {
			return nil, err
		}
		if results == nil {
			results = []*pubsdk.Knowledge{}
		}
		return map[string]interface{}{"results": results}, nil

	case MethodKnowledgeAdd:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		var p struct {
			Name       string    `json:"name"`
			Content    string    `json:"content,omitempty"`
			ContentRef SharedRef `json:"content_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 知识正文可达数十 KB，优先走共享内存。内容是 JSON 字符串，
		// 所以从 ref 读出后需再解一层。
		if err := h.resolveJSONRef(p.ContentRef, &p.Content); err != nil {
			return nil, err
		}
		return nil, kn.Add(p.Name, p.Content)

	case MethodKnowledgeAddMedia:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		var p struct {
			Name       string                        `json:"name"`
			Content    string                        `json:"content,omitempty"`
			Media      []knowledge.KnowledgeMediaRef `json:"media,omitempty"`
			ContentRef SharedRef                     `json:"content_ref,omitempty"`
			MediaRef   SharedRef                     `json:"media_ref,omitempty"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		// 正文与媒体清单都可能很大，优先走共享内存（与 knowledge.add 同形）。
		if err := h.resolveJSONRef(p.ContentRef, &p.Content); err != nil {
			return nil, err
		}
		if err := h.resolveJSONRef(p.MediaRef, &p.Media); err != nil {
			return nil, err
		}
		// 多模态是内核侧扩展能力（公开 SDK 契约不含它，见 internal/sdk/knowledge.go）。
		// 这里用局部接口 + 类型断言取用，而不把 internal/sdk 拉进本包：
		// 后者已依赖 internal/plugin，直接引会成环（见 corehandler.go 顶部注释）。
		adder, ok := kn.(knowledgeMediaAdder)
		if !ok {
			return nil, errUnavailable("knowledge multimodal")
		}
		if err := adder.AddWithMedia(p.Name, p.Content, p.Media); err != nil {
			return nil, err
		}
		// 回传媒体清单：插件后续要按 digest 引用同一份媒体。
		return map[string]interface{}{"media": p.Media}, nil

	case MethodKnowledgeList:
		kn := h.sdk.Knowledge()
		if kn == nil {
			return nil, errUnavailable("knowledge")
		}
		names, err := kn.List()
		if err != nil {
			return nil, err
		}
		if names == nil {
			names = []string{}
		}
		return map[string]interface{}{"names": names}, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleTextMemory 处理文本记忆追加。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleTextMemory(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 文本记忆（原 case 41）----
	case MethodTextMemoryAppend:
		tm := h.sdk.TextMemory()
		if tm == nil {
			return nil, errUnavailable("text memory")
		}
		var p struct {
			Event pubsdk.TextEvent `json:"event"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, tm.Append(p.Event)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}
