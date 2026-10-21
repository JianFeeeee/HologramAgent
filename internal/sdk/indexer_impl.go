package sdk

import (
	"errors"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// indexerImpl 桥接 memory.Indexer 到中立的 IndexerAPI。
type indexerImpl struct {
	idx *memory.Indexer
}

func NewIndexer(idx *memory.Indexer) IndexerAPI {
	return &indexerImpl{idx: idx}
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
	return &IndexContext{
		Entities:      entities,
		Relations:     relations,
		Summary:       ctx.Summary,
		TokenEstimate: ctx.TokenEstimate,
	}, nil
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
