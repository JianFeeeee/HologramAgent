package knowledgeplugin

import (
	"sort"
	"strings"
)

// timeLayout 是树视图对外暴露时间戳的格式（RFC3339，秒精度）。
// 显式声明而非用 time.RFC3339 常量：调用方按字面量解析，格式变了会静默错解。
const timeLayout = "2006-01-02T15:04:05Z07:00"

// 树形只读视图（供 HTTP/Skill 等外部消费者使用）。
//
// 与 TreeIndex 的区别，也是本文件存在的理由：TreeIndex 是**内部导出物**，
// 面向 .index.json 落盘，每个 IndexItem 带着 top-20 的 TF-IDF 特征向量。
// 把那个结构直接序列化给外部会有三个问题：
//  1. 体积：每条几百个浮点数（实测 200 条时 .index.json 已 246KB，且里面
//     还冗余存了 preview，而正本在 content.md）；
//  2. 泄漏：Vector 是稀疏特征表，等于把分词/IDF 统计细节对外暴露；
//  3. 语义错位：外部调用方要的是「有哪些分类、每个分类下有什么」，
//     而不是一个分词器内部表示。
//
// 所以这里定义**面向服务**的视图：不带向量，带条目数与可读摘要，
// 并支持按需只取某棵子树（懒加载），避免一次性吐出整棵树。

// TreeView 是分类树的一个节点。
type TreeView struct {
	// Name 是本节点名。根节点为 "root"。
	Name string `json:"name"`
	// Path 是从根到本节点的分类路径（不含根），如 "tech/go"。根节点为空。
	Path string `json:"path"`
	// ItemCount 是本节点**直接挂载**的条目数（不含子节点下的）。
	ItemCount int `json:"item_count"`
	// TotalCount 是本节点整棵子树下的条目总数（含后代）。前端做懒加载时
	// 只给 ItemCount 也能显示"展开前有多少"，TotalCount 则能一眼看出规模。
	TotalCount int `json:"total_count"`
	// Items 是直接挂在本节点的条目。depth=0 时不填（只给计数）。
	Items []TreeItemView `json:"items,omitempty"`
	// Children 是子分类，按名字排序（map 迭代无序，不排序则响应不可复现）。
	Children []TreeView `json:"children,omitempty"`
}

// TreeItemView 是一个知识条目的只读视图。
type TreeItemView struct {
	Name      string   `json:"name"`
	Preview   string   `json:"preview"`
	Size      int      `json:"size"`
	Tags      []string `json:"tags,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	// Media 是该条目挂载的媒体（多模态：条目可被"以图搜"召回）。
	Media []TreeMediaView `json:"media,omitempty"`
}

// TreeMediaView 是媒体引用摘要。
type TreeMediaView struct {
	Digest string `json:"digest"`
	MIME   string `json:"mime"`
	Kind   string `json:"kind,omitempty"`
}

// TreeOptions 控制 TreeView 的取舍。
type TreeOptions struct {
	// MaxDepth 限制返回的层数。0 = 不限；1 = 只根 + 第一层分类。
	// 分类很多时用它做懒加载，避免单次响应膨胀。
	MaxDepth int
	// IncludeItems 是否填充 Items。只看结构时可关掉以减小响应。
	IncludeItems bool
	// PreviewLimit 是预览字数上限，0 用默认 120。
	PreviewLimit int
}

// 树形视图的默认预览长度。
const defaultTreePreview = 120

// Tree 返回面向服务的分类树视图。
func (s *Store) Tree(opt TreeOptions) *TreeView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.treeLocked("", opt, 1)
}

// Subtree 返回某棵子树的视图。category 为空时等价于 Tree。
func (s *Store) Subtree(category string, opt TreeOptions) *TreeView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cat := strings.Trim(strings.TrimSpace(category), "/")
	if cat == "" {
		return s.treeLocked("", opt, 1)
	}
	return s.treeLocked(cat, opt, 1)
}

// nodeSegment 取路径的**最后一段**作为本级段名。
func nodeSegment(category, root string) string {
	if i := strings.LastIndex(category, "/"); i >= 0 {
		return category[i+1:]
	}
	if category != "" {
		return category
	}
	return root
}

// treeLocked 是 Tree/Subtree 的实现（调用方须持读锁）。
//
// category 非空时返回那棵子树（根节点 Path 即 category）；
// 为空时返回整棵树（根节点 Name "root"，Path 空）。
//
// 节点 Name 是**本级段名**（如 "go"），Path 是完整路径（如 "tech/go"）。
// 两者分工：Name 给前端渲染层级标签，Path 给程序定位与再请求。
// 曾经 Name 直接写全路径，于是 tech 的子节点叫 "tech/go"，前端拿它拼
// 层级会得到 "tech/tech/go"，且与根节点 "root" 的语义不一致。
func (s *Store) treeLocked(category string, opt TreeOptions, depth int) *TreeView {
	previewLimit := opt.PreviewLimit
	if previewLimit <= 0 {
		previewLimit = defaultTreePreview
	}
	// 该子树是否存在
	if category != "" {
		if !s.hasInScopeLocked(category) {
			return nil
		}
	}

	out := &TreeView{Name: nodeSegment(category, "root"), Path: category}

	// 收集本节点直接挂载的条目（Category == category）
	for _, k := range s.items {
		if k.Category != category {
			continue
		}
		out.ItemCount++
		if !opt.IncludeItems {
			continue
		}
		item := TreeItemView{
			Name:      k.Name,
			Preview:   previewOf(k.Content, previewLimit),
			Size:      len(k.Content),
			Tags:      k.Tags,
			UpdatedAt: k.UpdatedAt.Format(timeLayout),
		}
		for _, m := range k.Media {
			item.Media = append(item.Media, TreeMediaView{Digest: m.Digest, MIME: m.MIME, Kind: m.Kind})
		}
		out.Items = append(out.Items, item)
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })

	// 收集直接子分类：Category == category+"/xxx"（即只有一层深）
	seen := make(map[string]struct{})
	prefix := category
	if prefix != "" {
		prefix += "/"
	}
	for _, k := range s.items {
		if k.Category == category || !strings.HasPrefix(k.Category, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k.Category, prefix)
		child := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			child = rest[:i] // 只取第一段 ⇒ 直接子分类
		}
		if child == "" {
			continue
		}
		seen[child] = struct{}{}
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	// MaxDepth 剪枝：depth > MaxDepth 时只给计数，不再下钻
	if opt.MaxDepth > 0 && depth >= opt.MaxDepth {
		for _, n := range names {
			sub := s.treeLocked(categoryPath(category, n), TreeOptions{MaxDepth: 0, IncludeItems: false}, depth+1)
			out.TotalCount += sub.TotalCount
		}
		out.TotalCount += out.ItemCount
		return out
	}

	for _, n := range names {
		childPath := categoryPath(category, n)
		sub := s.treeLocked(childPath, opt, depth+1)
		if sub == nil {
			continue
		}
		out.Children = append(out.Children, *sub)
		out.TotalCount += sub.TotalCount
	}
	out.TotalCount += out.ItemCount
	return out
}

// categoryPath 拼子分类路径（根为空时返回裸名）。
func categoryPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "/" + child
}

// previewOf 按 rune 截断预览（按字节切会把多字节字符切成乱码）。
func previewOf(s string, limit int) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}

// Categories 列出全部分类路径（去重、排序）。给"只想要平铺分类列表"的调用方。
func (s *Store) Categories() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, k := range s.items {
		if k.Category == "" {
			continue
		}
		// 一并给出中间层（tech/go/并发 ⇒ tech、tech/go 都要出现）
		parts := strings.Split(k.Category, "/")
		for i := range parts {
			seen[strings.Join(parts[:i+1], "/")] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CategoryCounts 给出每个分类（含中间层）下的条目总数，按数量倒序。
// 给"哪些分类最值得先看"这类场景。
func (s *Store) CategoryCounts() []CategoryCount {
	s.mu.RLock()
	defer s.mu.RUnlock()
	exact := map[string]int{}
	for _, k := range s.items {
		if k.Category != "" {
			exact[k.Category]++
		}
	}
	agg := map[string]int{}
	for cat, n := range exact {
		parts := strings.Split(cat, "/")
		for i := range parts {
			agg[strings.Join(parts[:i+1], "/")] += n
		}
	}
	out := make([]CategoryCount, 0, len(agg))
	for c, n := range agg {
		out = append(out, CategoryCount{Category: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Category < out[j].Category
	})
	return out
}

// CategoryCount 是一个分类的条目数。
type CategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}
