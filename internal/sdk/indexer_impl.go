package sdk

import (
	"errors"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

// indexerImpl 桥接 memory.Indexer 到中立的 IndexerAPI。
type indexerImpl struct {
	idx  *memory.Indexer
	docs *document.Store // 可选：给「上下文」诊断面提供 L2 那一路
}

func NewIndexer(idx *memory.Indexer) IndexerAPI {
	return &indexerImpl{idx: idx}
}

// NewIndexerWithDocs 让诊断面同时覆盖 L2 冷上下文与 L3 图记忆（双路召回）。
func NewIndexerWithDocs(idx *memory.Indexer, docs *document.Store) IndexerAPI {
	return &indexerImpl{idx: idx, docs: docs}
}

func (i *indexerImpl) BuildContext(q string) (*IndexContext, error) {
	if i.idx == nil {
		return nil, errors.New("indexer not available")
	}
	ctx := i.idx.BuildContext(q)
	if ctx == nil {
		return &IndexContext{}, nil
	}
	entities := make([]Entity, len(ctx.Entities))
	for j, e := range ctx.Entities {
		entities[j] = Entity{Name: e.Name, Type: e.Type, MentionCount: e.MentionCount}
	}
	relations := make([]Relation, len(ctx.Relations))
	for j, r := range ctx.Relations {
		relations[j] = Relation{SourceName: r.SourceName, TargetName: r.TargetName, RelationType: r.RelationType}
	}
	out := &IndexContext{
		Entities:      entities,
		Relations:     relations,
		Summary:       ctx.Summary,
		TokenEstimate: ctx.TokenEstimate,
	}
	out.Docs = i.queryDocs(q, 8)
	return out, nil
}

func (i *indexerImpl) FormatContext(ctx *IndexContext) string {
	if i.idx == nil || ctx == nil {
		return ""
	}
	entities := make([]memory.Entity, len(ctx.Entities))
	for j, e := range ctx.Entities {
		entities[j] = memory.Entity{Name: e.Name, Type: e.Type, MentionCount: e.MentionCount}
	}
	return i.idx.FormatContext(&memory.InjectedContext{
		Entities:      entities,
		Summary:       ctx.Summary,
		TokenEstimate: ctx.TokenEstimate,
	})
}

// queryDocs 取 L2 那一路的命中片段（与模型预填充同一检索路径）。
func (i *indexerImpl) queryDocs(q string, limit int) []DocSnippet {
	if i.docs == nil || strings.TrimSpace(q) == "" {
		return nil
	}
	hits := i.docs.QueryScored(q, limit)
	out := make([]DocSnippet, 0, len(hits))
	for _, h := range hits {
		if h.Doc == nil {
			continue
		}
		when := h.Doc.CreatedAt.Format(time.DateTime)
		if h.Doc.HasEvents() {
			when = h.Doc.Events[0].Timestamp.Format(time.DateTime)
		}
		// 正文在事件的**文本块**里（DocEvent 只有 Blocks，没有 Input/Response）。
		text := docFirstText(h.Doc)
		out = append(out, DocSnippet{ID: h.Doc.ID, When: when, Text: truncateRunes(text, 120), Score: h.Score})
	}
	return out
}

// docFirstText 取文档可用于展示的首段正文（跳过媒体块）。
func docFirstText(d *document.Doc) string {
	if d == nil {
		return ""
	}
	for _, ev := range d.Events {
		for _, b := range ev.Blocks {
			if b.Modality != "" && b.Modality != "text" {
				continue
			}
			if t := strings.TrimSpace(b.Text); t != "" {
				return t
			}
		}
	}
	return d.Content
}

func truncateRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (i *indexerImpl) GetToolDefinitions() []map[string]interface{} {
	if i.idx == nil {
		return nil
	}
	return i.idx.GetToolDefinitions()
}

func (i *indexerImpl) BuildToolPrompt() string {
	if i.idx == nil {
		return ""
	}
	return i.idx.BuildToolPrompt()
}

var _ IndexerAPI = (*indexerImpl)(nil)
