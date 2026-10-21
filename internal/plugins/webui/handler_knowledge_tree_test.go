package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

func newTreeHandler(t *testing.T) (*Handler, *knowledge.Store) {
	t.Helper()
	ks := knowledge.NewStore(t.TempDir())
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ks.Stop)
	for _, e := range []struct{ name, body string }{
		{"tech/go/并发", "goroutine 调度 GMP 抢占"},
		{"tech/go/context", "context 取消 超时"},
		{"tech/rust/所有权", "borrow checker move 语义"},
		{"life/sleep", "作息 褪黑素"},
	} {
		if err := ks.Add(e.name, e.body); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &types.Config{Daemon: types.DaemonConfig{
		CheckInterval: time.Minute, HeartbeatInterval: 30 * time.Second,
	}}
	sup := supervisor.New(cfg)
	sup.Start()
	t.Cleanup(sup.Shutdown)
	return NewHandler(testSDK(sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})), ks
}

// 路由不被 /api/v1/knowledge/ 通配吃掉（Go 1.22+ 最长前缀匹配）。
func TestKnowledgeTreeRouteNotSwallowed(t *testing.T) {
	h, _ := newTreeHandler(t)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/knowledge/tree", nil))
	if rec.Code == http.StatusNotFound && rec.Body.Len() == 0 {
		t.Fatal("路由未命中：/api/v1/knowledge/tree 返回了 mux 的 404")
	}
	// 鉴权未配置时是 503（说明命中了我们的 handler，而不是 mux 404）
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未配 api_key 时应为 503，实为 %d body=%s", rec.Code, rec.Body.String())
	}
}

func treeGet(t *testing.T, h *Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	// 绕过鉴权直接打 handler：这里测的是路由与业务，鉴权另有测试覆盖
	h.handleKnowledgeTree(rec, req)
	return rec
}

func TestKnowledgeTreeAPI(t *testing.T) {
	h, _ := newTreeHandler(t)

	rec := treeGet(t, h, "/api/v1/knowledge/tree")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tree struct {
			Name       string `json:"name"`
			Path       string `json:"path"`
			TotalCount int    `json:"total_count"`
			Children   []struct {
				Name       string `json:"name"`
				Path       string `json:"path"`
				TotalCount int    `json:"total_count"`
				Children   []struct {
					ItemCount int `json:"item_count"`
				} `json:"children"`
			} `json:"children"`
		} `json:"tree"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Tree.Name != "root" || resp.Tree.Path != "" {
		t.Errorf("根应为 root/空，实为 %q/%q", resp.Tree.Name, resp.Tree.Path)
	}
	if resp.Tree.TotalCount != 4 {
		t.Errorf("总条目应为 4，实为 %d", resp.Tree.TotalCount)
	}
	// 子节点 Name 是本级段名、Path 是全路径
	var tech *struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		TotalCount int    `json:"total_count"`
		Children   []struct {
			ItemCount int `json:"item_count"`
		} `json:"children"`
	}
	for i := range resp.Tree.Children {
		if resp.Tree.Children[i].Name == "tech" {
			tech = &resp.Tree.Children[i]
		}
	}
	if tech == nil {
		t.Fatal("缺少 tech 节点")
	}
	if tech.Path != "tech" || tech.TotalCount != 3 {
		t.Errorf("tech 应为 path=tech total=3，实为 %q/%d", tech.Path, tech.TotalCount)
	}
	if len(tech.Children) != 2 {
		t.Errorf("tech 下应有 2 个子分类，实为 %d", len(tech.Children))
	}

	// 响应里不得出现向量
	if containsStr(rec.Body.String(), `"vector"`) {
		t.Error("树 API 不应返回向量")
	}
}

// 子树路由：/tree/tech/go
func TestKnowledgeTreeSubtreeAPI(t *testing.T) {
	h, _ := newTreeHandler(t)
	rec := treeGet(t, h, "/api/v1/knowledge/tree/tech/go")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Tree struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			ItemCount int    `json:"item_count"`
		} `json:"tree"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Tree.Path != "tech/go" || resp.Tree.ItemCount != 2 {
		t.Errorf("tech/go 应有 2 条，实为 %q/%d", resp.Tree.Path, resp.Tree.ItemCount)
	}
	if resp.Tree.Name != "go" {
		t.Errorf("Name 应为本级段名 go，实为 %q", resp.Tree.Name)
	}
}

// 不存在的分类：404 + 附上现有分类便于自查。
func TestKnowledgeTreeNotFound(t *testing.T) {
	h, _ := newTreeHandler(t)
	rec := treeGet(t, h, "/api/v1/knowledge/tree/nope/here")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404，实为 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error      string   `json:"error"`
		Categories []string `json:"categories"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Categories) == 0 {
		t.Error("404 应附带现有分类列表，便于调用方自查")
	}
}

// 懒加载：depth=1 只给一层但计数准确；items=0 不返回条目。
func TestKnowledgeTreeLazyLoad(t *testing.T) {
	h, _ := newTreeHandler(t)
	rec := treeGet(t, h, "/api/v1/knowledge/tree?depth=1&items=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d", rec.Code)
	}
	var resp struct {
		Tree struct {
			TotalCount int `json:"total_count"`
			Children   []struct {
				Name       string `json:"name"`
				TotalCount int    `json:"total_count"`
				Children   []any  `json:"children"`
			} `json:"children"`
		} `json:"tree"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Tree.TotalCount != 4 {
		t.Errorf("剪枝不应改变总数，实为 %d", resp.Tree.TotalCount)
	}
	for _, c := range resp.Tree.Children {
		if len(c.Children) != 0 {
			t.Errorf("depth=1 时 %s 不应展开子节点", c.Name)
		}
	}
	if containsStr(rec.Body.String(), `"preview"`) {
		t.Error("items=0 时不应返回 preview")
	}
}

// 分类内检索：/tree/tech?q=...
func TestKnowledgeTreeSearchInCategory(t *testing.T) {
	h, _ := newTreeHandler(t)
	rec := treeGet(t, h, "/api/v1/knowledge/tree/tech?q=borrow")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results  []knowledgeView `json:"results"`
		Category string          `json:"category"`
		Query    string          `json:"query"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	// 首位必须是真正相关的 rust/所有权。
	//
	// 不断言"只命中 1 条"：词法路对任何查询都会给全库打个低分（这是它
	// 融合设计的已知性质——靠排序把强命中顶到前面，而不是靠过滤把弱命中
	// 删掉），所以 tech 子树内会有 3 条候选。这是有意行为，不是缺陷。
	if len(resp.Results) == 0 {
		t.Fatal("tech 子树内检索 borrow 应有命中")
	}
	if resp.Results[0].Name != "tech/rust/所有权" {
		t.Errorf("首位应是最相关的 rust/所有权，实为 %q", resp.Results[0].Name)
	}
	// 但范围外的 life/sleep 绝不能出现
	for _, r := range resp.Results {
		if !strings.HasPrefix(r.Name, "tech/") {
			t.Errorf("范围外条目 %q 混入 tech 子树检索结果", r.Name)
		}
	}
	if resp.Category != "tech" {
		t.Errorf("应回显 category=tech，实为 %q", resp.Category)
	}
}

// 平铺分类与计数端点。
func TestKnowledgeTreeCategoriesAndCounts(t *testing.T) {
	h, _ := newTreeHandler(t)

	rec := treeGet(t, h, "/api/v1/knowledge/tree/categories")
	if rec.Code != http.StatusOK {
		t.Fatalf("categories 应 200，实为 %d", rec.Code)
	}
	var c1 struct {
		Categories []string `json:"categories"`
	}
	json.Unmarshal(rec.Body.Bytes(), &c1)
	want := map[string]bool{"tech": true, "tech/go": true, "tech/rust": true, "life": true}
	for _, c := range c1.Categories {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("缺少 %v，实为 %v", want, c1.Categories)
	}

	rec = treeGet(t, h, "/api/v1/knowledge/tree/counts")
	if rec.Code != http.StatusOK {
		t.Fatalf("counts 应 200，实为 %d", rec.Code)
	}
	var c2 struct {
		Counts []struct {
			Category string `json:"category"`
			Count    int    `json:"count"`
		} `json:"counts"`
	}
	json.Unmarshal(rec.Body.Bytes(), &c2)
	byName := map[string]int{}
	for _, x := range c2.Counts {
		byName[x.Category] = x.Count
	}
	if byName["tech"] != 3 || byName["life"] != 1 {
		t.Errorf("计数错误: %v", byName)
	}
}

// 只读面：非 GET 一律拒绝（防止有人以为能通过它写入）。
func TestKnowledgeTreeIsReadOnly(t *testing.T) {
	h, ks := newTreeHandler(t)
	before := len(ks.List())
	for _, m := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := httptest.NewRecorder()
		h.handleKnowledgeTree(rec, httptest.NewRequest(m, "/api/v1/knowledge/tree", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 应 405，实为 %d", m, rec.Code)
		}
	}
	if len(ks.List()) != before {
		t.Error("只读端点不应改变条目数")
	}
}

// 知识库不可用时给 503 而不是 panic。
func TestKnowledgeTreeUnavailable(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := treeGet(t, h, "/api/v1/knowledge/tree")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("无知识库应 503，实为 %d", rec.Code)
	}
}

func containsStr(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
