package kbtree

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

type rig struct {
	p   *Plugin
	ks  *knowledge.Store
	sup *supervisor.Daemon
	srv *httptest.Server
	tok string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ks := knowledge.NewStore(t.TempDir())
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ks.Stop)
	for _, e := range []struct{ name, body string }{
		{"tech/go/并发", "goroutine 调度 GMP 抢占 通道"},
		{"tech/go/context", "context 取消 超时 传播"},
		{"tech/rust/所有权", "borrow checker move 语义"},
		{"life/sleep", "作息 褪黑素 深睡"},
		{"cook/coffee", "手冲 烘焙 水温"},
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

	s := sdk.New("test", sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Knowledge:  sdk.NewKnowledge(ks),
		Config:     sdk.NewConfig(cfg),
	})
	// 让 Start 监听 :0（随机空闲端口），这样既走真实启动路径又不撞固定端口
	p := New("kbtree")
	p.addr = "127.0.0.1:0"
	if err := p.Start(s); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	// 测试直接打插件的 mux（真实端口已由 :0 分配，无需再开 server）
	srv := httptest.NewServer(p.mux)
	t.Cleanup(srv.Close)

	return &rig{p: p, ks: ks, sup: sup, srv: srv, tok: p.token}
}

// get 带 token 请求。
func (r *rig) get(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, r.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-Key", r.tok)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *rig) getNoAuth(t *testing.T, path string) int {
	t.Helper()
	resp, err := r.srv.Client().Get(r.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// 无 token / 错 token 一律 401。
func TestRequiresToken(t *testing.T) {
	r := newRig(t)
	if code := r.getNoAuth(t, "/tree"); code != http.StatusUnauthorized {
		t.Errorf("无 token 应 401，实为 %d", code)
	}
	// 错 token
	resp, err := r.srv.Client().Get(r.srv.URL + "/tree?token=wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("错 token 应 401，实为 %d", resp.StatusCode)
	}
}

// token 也可用 ?token= 传（外部脚本友好）。
func TestTokenViaQuery(t *testing.T) {
	r := newRig(t)
	resp, err := r.srv.Client().Get(r.srv.URL + "/tree?token=" + r.tok)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("?token= 应放行，实为 %d", resp.StatusCode)
	}
	// Bearer 也行
	req, _ := http.NewRequest(http.MethodGet, r.srv.URL+"/tree", nil)
	req.Header.Set("Authorization", "Bearer "+r.tok)
	resp2, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("Bearer token 应放行，实为 %d", resp2.StatusCode)
	}
}

// 根路径自述端点。
func TestRootSelfDescription(t *testing.T) {
	r := newRig(t)
	code, body := r.get(t, "/")
	if code != http.StatusOK {
		t.Fatalf("应 200，实为 %d", code)
	}
	if !strings.Contains(string(body), "read_only") {
		t.Errorf("自述里应标明只读: %s", body)
	}
	if !strings.Contains(string(body), "/tree") {
		t.Errorf("自述里应列出端点: %s", body)
	}
}

// 树端点：结构、计数、懒加载。
func TestTreeEndpoint(t *testing.T) {
	r := newRig(t)
	code, body := r.get(t, "/tree?items=0")
	if code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", code, body)
	}
	if !strings.Contains(string(body), `"total_count":5`) {
		t.Errorf("总条目应为 5: %s", body)
	}
	if !strings.Contains(string(body), `"tech"`) {
		t.Errorf("应含 tech 分类: %s", body)
	}
	// items=0 时无 preview
	if strings.Contains(string(body), `"preview"`) {
		t.Error("items=0 不应返回 preview")
	}

	// 懒加载：depth=1 仍给出准确 total
	code, body = r.get(t, "/tree?depth=1&items=0")
	if code != http.StatusOK {
		t.Fatalf("depth=1 应 200，实为 %d", code)
	}
	if !strings.Contains(string(body), `"total_count":5`) {
		t.Errorf("剪枝不应改变总数: %s", body)
	}
}

// 子树端点。
func TestTreeSubtreeEndpoint(t *testing.T) {
	r := newRig(t)
	code, body := r.get(t, "/tree?category=tech/go&items=1")
	if code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", code, body)
	}
	var resp struct {
		Tree struct {
			Path      string `json:"path"`
			Name      string `json:"name"`
			ItemCount int    `json:"item_count"`
		} `json:"tree"`
	}
	json.Unmarshal(body, &resp)
	if resp.Tree.Path != "tech/go" || resp.Tree.ItemCount != 2 {
		t.Errorf("tech/go 应 2 条，实为 %q/%d", resp.Tree.Path, resp.Tree.ItemCount)
	}
	if resp.Tree.Name != "go" {
		t.Errorf("name 应为本级段名 go，实为 %q", resp.Tree.Name)
	}
}

// 不存在的分类：404 + 附现有分类。
func TestTreeNotFound(t *testing.T) {
	r := newRig(t)
	code, body := r.get(t, "/tree?category=nope/here")
	if code != http.StatusNotFound {
		t.Fatalf("应 404，实为 %d", code)
	}
	if !strings.Contains(string(body), "categories") {
		t.Errorf("404 应附现有分类便于自查: %s", body)
	}
}

// 检索端点：首位最相关、范围外不混入、缺 q 报 400。
func TestSearchEndpoint(t *testing.T) {
	r := newRig(t)
	code, body := r.get(t, "/search?q=borrow&category=tech")
	if code != http.StatusOK {
		t.Fatalf("应 200，实为 %d: %s", code, body)
	}
	var resp struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
		Hint string `json:"hint"`
	}
	json.Unmarshal(body, &resp)
	if len(resp.Results) == 0 {
		t.Fatal("应有命中")
	}
	if resp.Results[0].Name != "tech/rust/所有权" {
		t.Errorf("首位应最相关，实为 %q", resp.Results[0].Name)
	}
	for _, it := range resp.Results {
		if !strings.HasPrefix(it.Name, "tech/") {
			t.Errorf("范围外 %q 混入", it.Name)
		}
	}
	if resp.Hint == "" {
		t.Error("应带 hint")
	}
	// 缺 q
	if code, _ := r.get(t, "/search"); code != http.StatusBadRequest {
		t.Errorf("缺 q 应 400，实为 %d", code)
	}
}

// 分类与计数端点。
func TestCategoriesAndCountsEndpoint(t *testing.T) {
	r := newRig(t)
	_, body := r.get(t, "/categories")
	for _, want := range []string{"tech", "tech/go", "tech/rust", "life", "cook"} {
		if !strings.Contains(string(body), `"`+want+`"`) {
			t.Errorf("缺少分类 %s: %s", want, body)
		}
	}

	_, body = r.get(t, "/counts")
	var resp struct {
		Counts []struct {
			Category string `json:"category"`
			Count    int    `json:"count"`
		} `json:"counts"`
		Total int `json:"total"`
	}
	json.Unmarshal(body, &resp)
	byCat := map[string]int{}
	for _, c := range resp.Counts {
		byCat[c.Category] = c.Count
	}
	if byCat["tech"] != 3 {
		t.Errorf("tech 应聚合 3 条，实为 %d", byCat["tech"])
	}
	if resp.Total != 5 {
		t.Errorf("total 只数叶子应为 5，实为 %d", resp.Total)
	}
}

// 只读：写方法一律 405，且不改变知识库。
func TestReadOnly(t *testing.T) {
	r := newRig(t)
	before := len(r.ks.List())
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(m, r.srv.URL+"/tree", nil)
		req.Header.Set("X-API-Key", r.tok)
		resp, err := r.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s 应 405，实为 %d", m, resp.StatusCode)
		}
	}
	if len(r.ks.List()) != before {
		t.Error("只读服务不应改变知识库")
	}
}

// 知识库不可用时 Start 应失败（不占端口）。
func TestStartFailsWithoutKnowledge(t *testing.T) {
	cfg := &types.Config{Daemon: types.DaemonConfig{CheckInterval: time.Minute, HeartbeatInterval: 30 * time.Second}}
	sup := supervisor.New(cfg)
	sup.Start()
	defer sup.Shutdown()
	s := sdk.New("test", sdk.SDKConfig{
		Supervisor: supervisor.NewSDKAdapter(sup),
		Config:     sdk.NewConfig(cfg),
	})
	if err := New("kbtree").Start(s); err == nil {
		t.Error("无知识库时 Start 应失败")
	}
}

// 技能文档里的端点路径必须与代码一致（防止文档漂移）。
func TestSkillDocEndpointsMatchCode(t *testing.T) {
	doc, err := readSkillDoc()
	if err != nil {
		t.Skipf("读不到 skill 文档: %v", err)
	}
	for _, ep := range []string{"/tree", "/categories", "/counts", "/search"} {
		if !strings.Contains(doc, ep) {
			t.Errorf("SKILL.md 未提及端点 %s（文档与代码漂移）", ep)
		}
	}
}

func readSkillDoc() (string, error) {
	// 测试在 internal/plugins/kbtree，skill 在 ../../assets/skills
	paths := []string{
		filepath.Join("..", "..", "assets", "skills", "knowledge-base", "SKILL.md"),
		filepath.Join("..", "..", "..", "assets", "skills", "knowledge-base", "SKILL.md"),
	}
	var lastErr error
	for _, p := range paths {
		if b, err := readFile(p); err == nil {
			return string(b), nil
		} else {
			lastErr = err
		}
	}
	return "", lastErr
}

func readFile(p string) ([]byte, error) {
	return os.ReadFile(p)
}
