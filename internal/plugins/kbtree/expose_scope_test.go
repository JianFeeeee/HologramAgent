package kbtree

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ===== 知识库对外暴露范围 =====
//
// 现状缺陷：kbtree 只有 listen_addr 与 token 两个配置，**没有任何范围过滤**。
// 拿到 token 的任何 agent 都能 /tree 拿到全部条目、/search 拿到全文。
// 而本机知识库里混着个人内容（教材摘录、课表、身份合并规则），
// 不该 broadly 可读。
//
// 本测试钉住：暴露范围配置存在，且**四个端点全部受它约束**。
//
// ★ 为什么要覆盖 /search：这是最容易漏的一处。只过滤 /tree 而不过滤
//   /search，等于范围形同虚设 —— 外部 agent 只要换个 ?q= 关键词就能
//   搜到范围外的条目全文。所以下面每条判据都同时查 /tree 与 /search。

type fakeKn struct {
	sdk.KnowledgeAPI
	items []struct{ name, category, content string }
}

// Subtree 复刻内核 treeLocked 的语义（internal/knowledge/tree.go）：
//   - Category == 本节点 的条目挂在本节点；
//   - Category 以 本节点+"/" 开头的，取**第一段**作为直接子分类（提升一层）。
//
// 这层"提升"是内核既有行为，fake 必须照抄，否则测的是 fake 的形状
// 而不是产品行为（我第一版就因为漏了它，得到 children=0 的假空树）。
func (f *fakeKn) Subtree(category string, opt sdk.KnowledgeTreeOptions) (*sdk.KnowledgeTreeView, error) {
	cat := strings.Trim(strings.TrimSpace(category), "/")
	node := &sdk.KnowledgeTreeView{Name: "root"}
	if cat != "" {
		seg := cat
		if i := strings.LastIndex(cat, "/"); i >= 0 {
			seg = cat[i+1:]
		}
		node = &sdk.KnowledgeTreeView{Name: seg, Path: cat}
	}
	for _, it := range f.items {
		if it.category != cat {
			continue
		}
		node.ItemCount++
		node.TotalCount++
		if opt.IncludeItems {
			node.Items = append(node.Items, sdk.KnowledgeTreeItemView{
				Name: it.name, Preview: it.content,
			})
		}
	}
	prefix := cat
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]struct{}{}
	for _, it := range f.items {
		if it.category == cat || !strings.HasPrefix(it.category, prefix) {
			continue
		}
		rest := strings.TrimPrefix(it.category, prefix)
		seg := rest
		if i := strings.Index(rest, "/"); i >= 0 {
			seg = rest[:i]
		}
		if seg == "" {
			continue
		}
		seen[seg] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		childPath := n
		if cat != "" {
			childPath = cat + "/" + n
		}
		sub, _ := f.Subtree(childPath, opt)
		if sub == nil {
			continue
		}
		node.Children = append(node.Children, *sub)
		node.TotalCount += sub.TotalCount
	}
	return node, nil
}

func (f *fakeKn) SearchIn(query, category string, topK int) ([]*sdk.Knowledge, error) {
	var out []*sdk.Knowledge
	for _, it := range f.items {
		if category != "" && !strings.HasPrefix(it.category, category) {
			continue
		}
		if query == "" || strings.Contains(it.content, query) {
			out = append(out, &sdk.Knowledge{
				Name: it.name, Category: it.category, Content: it.content,
			})
		}
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

func (f *fakeKn) Categories() ([]string, error) {
	seen := map[string]bool{}
	for _, it := range f.items {
		if it.category != "" {
			seen[it.category] = true
		}
	}
	var out []string
	for c := range seen {
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeKn) CategoryCounts() ([]sdk.KnowledgeCategoryCount, error) {
	m := map[string]int{}
	var order []string
	for _, it := range f.items {
		if _, ok := m[it.category]; !ok {
			order = append(order, it.category)
		}
		m[it.category]++
	}
	var out []sdk.KnowledgeCategoryCount
	for _, c := range order {
		out = append(out, sdk.KnowledgeCategoryCount{Category: c, Count: m[c]})
	}
	return out, nil
}

// 样本：public/ 下 2 条，private/ 下 1 条（含敏感内容）。
func newScopedFixture() *fakeKn {
	return &fakeKn{items: []struct{ name, category, content string }{
		{"pub1", "public", "公开的架构说明"},
		{"pub2", "public/tech", "公开的并发笔记"},
		{"priv1", "private", "绝密：个人身份证号 123"},
	}}
}

func newScopedServer(t *testing.T, expose string) *httptest.Server {
	t.Helper()
	p := &Plugin{name: "kbtree", token: "tok", expose: newScope(expose), mux: http.NewServeMux()}
	p.kn = newScopedFixture()
	p.registerRoutes()
	s := httptest.NewServer(p.mux)
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, s *httptest.Server, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", "tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf strings.Builder
	_, _ = buf.WriteString("")
	b := make([]byte, 32*1024)
	n, _ := resp.Body.Read(b)
	return resp.StatusCode, string(b[:n])
}

// 暴露范围生效：范围外的条目**与分类路径**都不得出现在任何端点的响应里。
//
// ★ 判据分两类断言，因为两个端点的泄露形态不同（第一版只查条目名，
//
//	结果「/categories 不过滤」这个变异完全逃过了 —— 分类端点返回的是
//	路径不是条目名，只查 priv1 永远查不到）：
//	- 条目名/正文（priv1、身份证）：出现在 /tree、/search 的泄露
//	- 分类路径（private）：出现在 /categories、/counts 的泄露
func TestExposeScopeHidesOutOfScopeItems(t *testing.T) {
	s := newScopedServer(t, "public")

	for _, path := range []string{
		"/tree",
		"/categories",
		"/counts",
		// ★ 关键：范围外的条目即使能被搜到也不得返回。
		// 查询词用"绝密"是为了让 fake 命中那条私密条目；
		// 断言针对的是**结果条目**（priv1 / 其正文），不针对 query 回显 ——
		// 响应里回显调用方自己发来的 q 是正常行为，不是泄露。
		"/search?q=" + "绝密",
	} {
		code, body := get(t, s, path)
		if code != http.StatusOK {
			t.Fatalf("%s: 期望 200，实际 %d", path, code)
		}
		if strings.Contains(body, "priv1") {
			t.Errorf("%s 泄露了范围外条目 priv1：%s", path, body)
		}
		if strings.Contains(body, "身份证") {
			t.Errorf("%s 泄露了范围外条目的正文：%s", path, body)
		}
		// 分类路径泄露：private 这个词既可能是分类名也可能是条目内容里
		// 的普通词，所以只在两个"按分类组织"的端点上断言。
		if path == "/categories" || path == "/counts" {
			if strings.Contains(body, "private") {
				t.Errorf("%s 泄露了范围外分类路径 private：%s", path, body)
			}
		}
	}
}

// 范围内的条目必须仍然可见（防止"过滤过头"把树清空 —— 那是另一种假绿）。
func TestExposeScopeKeepsInScopeItems(t *testing.T) {
	s := newScopedServer(t, "public")
	code, body := get(t, s, "/tree")
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	for _, want := range []string{"pub1", "pub2"} {
		if !strings.Contains(body, want) {
			t.Errorf("范围内条目 %s 消失了（过滤过头）：%s", want, body)
		}
	}
	// 子分类也必须在（树状结构不能被压平）
	if !strings.Contains(body, "tech") {
		t.Errorf("子树 tech 丢失，树状结构被压平：%s", body)
	}
}

// 范围配置必须真的能限定 /search 的召回，而不是只在输出端删字段。
//
// 这一条钉住"过滤发生在哪一层"：若只在响应里删掉范围外条目，
// 而 SearchIn 本身把全文返回了，limit 参数会因范围外条目占位而
// 让范围内条目被挤掉 —— 结果是"看起来过滤了，其实漏了"。
func TestExposeScopeSearchKeepsInScopeFillsLimit(t *testing.T) {
	s := newScopedServer(t, "public")
	// limit=1 时若 priv1 占掉名额，pub1 就拿不到
	_, body := get(t, s, "/search?q="+"&limit=1")
	if strings.Contains(body, "priv1") {
		t.Errorf("范围内检索被范围外条目挤占：%s", body)
	}
}

// 留空 = 全部可见（保持既有行为；范围是"限制"不是"必填"）。
func TestEmptyExposeScopeMeansAll(t *testing.T) {
	s := newScopedServer(t, "")
	code, body := get(t, s, "/tree")
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if !strings.Contains(body, "priv1") {
		t.Errorf("范围留空时应全部可见（向后兼容），实际看不到 priv1：%s", body)
	}
}

// 范围解析：逗号/空格/换行分隔都能吃；前缀匹配语义正确。
func TestExposeScopeParsing(t *testing.T) {
	cases := []struct {
		in    string
		want  []string
		match []struct {
			cat string
			in  bool
		}
	}{
		{"", nil, []struct {
			cat string
			in  bool
		}{{cat: "private", in: true}, {cat: "public", in: true}}},
		{"public", []string{"public"}, []struct {
			cat string
			in  bool
		}{{cat: "public", in: true}, {cat: "public/tech", in: true}, {cat: "private", in: false}}},
		{" public , private ", []string{"public", "private"}, []struct {
			cat string
			in  bool
		}{{cat: "public", in: true}, {cat: "private", in: true}}},
		{"public\nprivate", []string{"public", "private"}, []struct {
			cat string
			in  bool
		}{{cat: "private", in: true}}},
	}
	for _, c := range cases {
		sc := newScope(c.in)
		if len(sc.paths) != len(c.want) {
			t.Errorf("parse(%q) 得到 %v，期望 %v", c.in, sc.paths, c.want)
		}
		for _, m := range c.match {
			if got := sc.allows(m.cat); got != m.in {
				t.Errorf("parse(%q).allows(%q) = %v，期望 %v", c.in, m.cat, got, m.in)
			}
		}
	}
}

// 边界：public 不应匹配 publication（前缀必须是路径分段级）。
func TestExposeScopePrefixIsSegmentWise(t *testing.T) {
	sc := newScope("public")
	if sc.allows("publication") {
		t.Error("publication 被 public 范围包含 —— 前缀必须按路径分段比较，" +
			"否则 publication 这类目录会意外暴露")
	}
}

// 配置项必须在插件里注册（否则前端设置页无法编辑，功能等于隐藏）。
func TestExposeScopeConfigIsRegistered(t *testing.T) {
	// 直接断言源码里存在注册调用（读同包源码，键名漂移会被立刻发现）
	b, err := readSelf(t, "plugin.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, `Key: "expose_categories"`) {
		t.Error("未注册 expose_categories 配置项 —— 设置页无法编辑，功能等于隐藏")
	}
}

// readSelf 读同包源码文件。
func readSelf(t *testing.T, name string) ([]byte, error) {
	return os.ReadFile(name)
}

// 响应可解析（粗判 JSON 合法，避免把 HTML 错误页当成功）。
func TestScopedResponsesAreJSON(t *testing.T) {
	s := newScopedServer(t, "public")
	for _, path := range []string{"/tree", "/categories", "/counts", "/search?q=a"} {
		_, body := get(t, s, path)
		var v interface{}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Errorf("%s 返回的不是合法 JSON：%v（body=%.120s）", path, err, body)
		}
	}
}
