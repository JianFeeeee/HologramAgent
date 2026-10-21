package kbtree

import (
	"strings"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// scope 是知识库对外暴露范围。
//
// 存在的理由：kbtree 是**唯一**把知识库开放给外部进程的通道（HomeAgent
// 自己的 agent 走进程内直调，不经此）。没有范围限制时，任何拿到 token 的
// agent 都能 /tree 列出全部条目、/search 取回任意条目全文 —— 而本机库里
// 混着个人内容（教材摘录、课表、身份合并规则），不该 broadly 可读。
//
// 语义：
//   - paths 为空 ⇒ 不限范围（全部可见）。范围是"限制"不是"必填"，
//     留空时保持既有行为，避免升级即失效。
//   - 前缀匹配按**路径分段**比较："public" 命中 "public" 与 "public/tech"，
//     但**不**命中 "publication"（否则 publication 这类目录会意外暴露）。
//   - 范围外的条目在**服务端**就被剔除：客户端过滤等于没过滤，范围外内容
//     已经随响应发出去了。
type scope struct {
	paths []string
}

// newScope 解析范围配置：逗号 / 空格 / 换行分隔，逐项去空白与首尾斜杠。
//
// 为何容忍多种分隔符：这是给人在设置页手填的字段，`a, b` 与 `a b` 与
// 换行粘贴都常见；只认逗号会让"看起来填了却没生效"这种错配很难自查。
func newScope(raw string) *scope {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' ||
			r == ' ' || r == '\t' || r == '/' || r == '｜'
	})
	paths := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.Trim(strings.TrimSpace(f), "/")
		if f != "" {
			paths = append(paths, f)
		}
	}
	return &scope{paths: paths}
}

// allows 报告某分类（或根 ""）是否在暴露范围内。
//
// 条目级的判定：条目的 Category 为 "" 表示直接挂在根下（无分类），
// 它只在**范围留空**时可见 —— 根下条目没有分类可匹配，若允许它按
// 任意前缀可见，那范围就形同虚设（根下条目永远逃逸）。
func (s *scope) allows(category string) bool {
	if s == nil || len(s.paths) == 0 {
		return true
	}
	cat := strings.Trim(strings.TrimSpace(category), "/")
	if cat == "" {
		return false
	}
	for _, p := range s.paths {
		if cat == p || strings.HasPrefix(cat, p+"/") {
			return true
		}
	}
	return false
}

// filterTree 就地裁剪树视图，剔除范围外的分支。
//
// 就地改而不是重建：TreeView 是内核生成的，字段多（含 Media 切片），
// 重建容易漏字段导致"裁剪后条目信息变空"。
func (s *scope) filterTree(t *sdk.KnowledgeTreeView) {
	if t == nil || s == nil || len(s.paths) == 0 {
		return
	}
	// 根节点恒可见（否则调用方连"有什么可看的"都不知道）
	kept := t.Children[:0]
	for i := range t.Children {
		c := &t.Children[i]
		if !s.allows(c.Path) {
			continue
		}
		kept = append(kept, *c)
	}
	t.Children = kept
	// 根下直接挂载的条目：范围非空时不可见（allows("") == false）
	// 根下直接挂载的条目：范围非空时不可见（allows("") == false）。
	// 只在**本节点自己**这么做，不能对子树也做 —— 子树节点自己挂的条目
	// 属于该分类，在范围内，必须保留（我第一版无差别清空，
	// 结果整棵树只剩空壳节点：item_count 全 0，条目名一个都不剩）。
	if s.allows(t.Path) {
		// 保留本节点的条目
	} else {
		t.Items = nil
		t.ItemCount = 0
	}
	t.TotalCount = t.ItemCount
	for i := range t.Children {
		s.filterTree(&t.Children[i])
		t.TotalCount += t.Children[i].TotalCount
	}
}

// filterNames 过滤分类路径列表。
func (s *scope) filterNames(names []string) []string {
	if s == nil || len(s.paths) == 0 {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if s.allows(n) {
			out = append(out, n)
		}
	}
	return out
}

// filterCounts 过滤分类计数，并按范围重算 total。
func (s *scope) filterCounts(counts []sdk.KnowledgeCategoryCount) ([]sdk.KnowledgeCategoryCount, int) {
	if s == nil || len(s.paths) == 0 {
		return counts, totalLeafCount(counts)
	}
	out := make([]sdk.KnowledgeCategoryCount, 0, len(counts))
	for _, c := range counts {
		if s.allows(c.Category) {
			out = append(out, c)
		}
	}
	return out, totalLeafCount(out)
}

// totalLeafCount 只数叶子分类，避免中间层重复计数（与未过滤时的口径一致）。
func totalLeafCount(counts []sdk.KnowledgeCategoryCount) int {
	total := 0
	for _, c := range counts {
		if !isParentCategory(counts, c.Category) {
			total += c.Count
		}
	}
	return total
}
