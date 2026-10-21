package sdk

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

type knowledgeImpl struct{ ks *knowledge.Store }

func NewKnowledge(ks *knowledge.Store) KnowledgeAPI { return &knowledgeImpl{ks: ks} }

func (k *knowledgeImpl) Search(query string, topK int) ([]*Knowledge, error) {
	if k.ks == nil {
		return nil, nil
	}
	got := k.ks.Search(query, topK)
	out := make([]*Knowledge, len(got))
	for i, item := range got {
		out[i] = &Knowledge{Name: item.Name, Content: item.Content, Category: item.Category}
	}
	return out, nil
}

func (k *knowledgeImpl) Add(name, content string) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.Add(name, content)
}

func (k *knowledgeImpl) SearchIn(query, category string, topK int) ([]*Knowledge, error) {
	if k.ks == nil {
		return nil, nil
	}
	got := k.ks.SearchIn(query, category, topK)
	out := make([]*Knowledge, len(got))
	for i, item := range got {
		out[i] = &Knowledge{Name: item.Name, Content: item.Content, Category: item.Category}
	}
	return out, nil
}

func (k *knowledgeImpl) Tree(opt KnowledgeTreeOptions) (*KnowledgeTreeView, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.Tree(knowledge.TreeOptions{
		MaxDepth:     opt.MaxDepth,
		IncludeItems: opt.IncludeItems,
		PreviewLimit: opt.PreviewLimit,
	}), nil
}

func (k *knowledgeImpl) Subtree(category string, opt KnowledgeTreeOptions) (*KnowledgeTreeView, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.Subtree(category, knowledge.TreeOptions{
		MaxDepth:     opt.MaxDepth,
		IncludeItems: opt.IncludeItems,
		PreviewLimit: opt.PreviewLimit,
	}), nil
}

// ImportDir 转发到内核 Store。
func (k *knowledgeImpl) ImportDir(opt knowledge.ImportOptions) (knowledge.ImportStats, error) {
	if k.ks == nil {
		return knowledge.ImportStats{}, fmt.Errorf("knowledge: 知识库不可用")
	}
	return k.ks.ImportDir(opt)
}

func (k *knowledgeImpl) Categories() ([]string, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.Categories(), nil
}

func (k *knowledgeImpl) CategoryCounts() ([]KnowledgeCategoryCount, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.CategoryCounts(), nil
}

func (k *knowledgeImpl) AddWithMedia(name, content string, media []KnowledgeMediaRef) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.AddWithMedia(name, content, media)
}

func (k *knowledgeImpl) AttachMedia(name string, media ...KnowledgeMediaRef) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.AttachMedia(name, media...)
}

func (k *knowledgeImpl) ReindexDense() (int, int) {
	if k.ks == nil {
		return 0, 0
	}
	return k.ks.ReindexDense()
}

func (k *knowledgeImpl) DenseStats() map[string]interface{} {
	if k.ks == nil {
		return map[string]interface{}{}
	}
	return k.ks.DenseStats()
}

func (k *knowledgeImpl) List() ([]string, error) {
	if k.ks == nil {
		return nil, nil
	}
	return k.ks.List(), nil
}

func (k *knowledgeImpl) Stats() map[string]interface{} {
	if k.ks == nil {
		return map[string]interface{}{}
	}
	return k.ks.Stats()
}

func (k *knowledgeImpl) Remove(name string) error {
	if k.ks == nil {
		return nil
	}
	return k.ks.Remove(name)
}

var _ KnowledgeAPI = (*knowledgeImpl)(nil)
