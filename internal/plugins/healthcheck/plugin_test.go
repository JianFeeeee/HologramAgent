package healthcheck

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	agentCore "github.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	doc "github.com/JianFeeeee/HomeAgent/internal/memory/document"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

type toolCapture struct {
	handlers map[string]sdk.ToolHandler
	defs     map[string]sdk.ToolDef
}

func newToolCapture() *toolCapture {
	return &toolCapture{
		handlers: make(map[string]sdk.ToolHandler),
		defs:     make(map[string]sdk.ToolDef),
	}
}

func (tc *toolCapture) RegisterTool(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
	tc.handlers[name] = handler
	tc.defs[name] = def
	return nil
}
func (tc *toolCapture) RegisterStage(stage sdk.Stage, handler sdk.StageHandler) {}
func (tc *toolCapture) RegisterAPI(name string) error                           { return nil }

func newTestSDK(cfg sdk.SDKConfig) *sdk.PluginSDK {
	if cfg.Settings == nil {
		cfg.Settings = sdk.NewSettings("healthcheck", nil)
	}
	if cfg.Tool == nil {
		cfg.Tool = sdk.NewTool(agentCore.NewStageHost(), agentIO.NewIOManager())
	}
	return sdk.New("healthcheck", cfg)
}

func setupPlugin() (*Plugin, *toolCapture, error) {
	return setupPluginWith(sdk.SDKConfig{})
}

func setupPluginWith(cfg sdk.SDKConfig) (*Plugin, *toolCapture, error) {
	tc := newToolCapture()
	cfg.RegTool = tc.RegisterTool
	cfg.RegStage = tc.RegisterStage
	cfg.RegAPI = tc.RegisterAPI
	p := New("healthcheck")
	s := newTestSDK(cfg)
	if err := p.Start(s); err != nil {
		return nil, nil, err
	}
	return p, tc, nil
}

func TestToolsRegistered(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	expected := []string{
		"healthcheck",
		"healthcheck_plugins",
		"healthcheck_tools",
		"healthcheck_memory",
		"healthcheck_report",
		"healthcheck_perf",
	}
	for _, name := range expected {
		if _, ok := tc.handlers[name]; !ok {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestHealthcheckFull(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	if len(checks) == 0 {
		t.Fatal("expected at least some checks")
	}

	for _, c := range checks {
		cr := c.(map[string]interface{})
		name := cr["name"].(string)
		pass := cr["pass"].(bool)
		if !pass && cr["status"] != "skip" {
			t.Errorf("check %q failed: %v (detail: %v)", name, cr["status"], cr["detail"])
		}
	}
}

func TestHealthcheckPlugins(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_plugins"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

func TestHealthcheckToolsList(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_tools"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}
}

func TestHealthcheckMemoryNotAvailable(t *testing.T) {
	_, tc, err := setupPlugin()
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_memory"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	// Memory is nil in this setup, so it should skip gracefully
	if _, ok := resp["pass"]; ok {
		pass := resp["pass"].(bool)
		if !pass {
			t.Fatalf("expected pass=true when memory is nil, got false: %v", resp)
		}
	}
}

func TestHealthcheckWithMemory(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	memDB, err := memory.NewGraphDB(tmpDir + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer memDB.Close()

	// 预置一条生产数据，验证 healthcheck 自检后不触碰它
	_, _, err = memDB.Commit([]memory.Triple{{Subject: "用户", Relation: "喜欢", Object: "咖啡"}}, "test", 0)
	if err != nil {
		t.Fatal(err)
	}

	memSnapshot := func() (int, int) {
		m, _ := memDB.Introspect()
		ents, _ := m["entity_count"].(int)
		rels, _ := m["relation_count"].(int)
		return ents, rels
	}
	be, br := memSnapshot()

	_, tc, err := setupPluginWith(sdk.SDKConfig{Memory: sdk.NewGraphMemory(memDB)})
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck_memory"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v (detail=%v)", resp["status"], resp["detail"])
	}
	if pass, ok := resp["pass"].(bool); !ok || !pass {
		t.Fatalf("expected pass=true, got pass=%v status=%v detail=%v", pass, resp["status"], resp["detail"])
	}

	// 关键：生产记忆内容必须保持不变（未被 healthcheck 污染）
	ae, ar := memSnapshot()
	if ae != be || ar != br {
		t.Fatalf("production memory polluted by healthcheck self-test: before=(%d,%d) after=(%d,%d)", be, br, ae, ar)
	}

	// 再次确认：注入实例中不应出现 _hc_ 测试实体
	relResult, _ := memDB.Recall([]string{"_hc_"}, nil, 1, "")
	for _, e := range relResult.Entities {
		if len(e.Name) >= 4 && e.Name[:4] == "_hc_" {
			t.Fatalf("healthcheck left _hc_ entity in production memory: %q", e.Name)
		}
	}
}

func TestHealthcheckWithKnowledge(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_know_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ks := knowledge.NewStore(tmpDir)
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}
	defer ks.Stop()

	// 预置一条真实知识，验证 healthcheck 自检后不触碰它
	if err := ks.Add("生产知识点", "这是生产知识，不应被健康检查破坏"); err != nil {
		t.Fatal(err)
	}
	before := len(ks.List())

	_, tc, err := setupPluginWith(sdk.SDKConfig{Knowledge: sdk.NewKnowledge(ks)})
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	var knowledgeCheck map[string]interface{}
	for _, c := range checks {
		cr := c.(map[string]interface{})
		if cr["name"] == "knowledge" {
			knowledgeCheck = cr
			break
		}
	}

	if knowledgeCheck == nil {
		t.Fatal("expected knowledge check in results")
	}
	if knowledgeCheck["status"] != "ok" {
		t.Fatalf("expected knowledge check ok, got %v (detail=%v)", knowledgeCheck["status"], knowledgeCheck["detail"])
	}

	// 生产知识库内容必须保持不变（未被 healthcheck 污染）
	after := len(ks.List())
	if after != before {
		t.Fatalf("production knowledge polluted: before=%d after=%d", before, after)
	}
	for _, name := range ks.List() {
		if strings.HasPrefix(name, "_hc_knowledge_test_") {
			t.Fatalf("healthcheck left test knowledge in production: %q", name)
		}
	}
}

func TestHealthcheckWithDocStore(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "hc_doc_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	ds := doc.NewStore(tmpDir, memory.TokenizeWords)
	if err := ds.Start(); err != nil {
		t.Fatal(err)
	}
	defer ds.Stop()

	// 预置一篇生产文档，验证 healthcheck 自检后不触碰它
	if err := ds.Insert(&doc.Doc{ID: "prod_doc", Summary: "生产文档", Content: "这是生产文档，不应被健康检查破坏"}); err != nil {
		t.Fatal(err)
	}
	before := len(ds.Query("生产文档", 10))

	_, tc, err := setupPluginWith(sdk.SDKConfig{DocMemory: sdk.NewDocMemory(ds)})
	if err != nil {
		t.Fatal(err)
	}

	handler := tc.handlers["healthcheck"]
	result, err := handler(map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status ok, got %v", resp["status"])
	}

	checks := resp["checks"].([]interface{})
	var docCheck map[string]interface{}
	for _, c := range checks {
		cr := c.(map[string]interface{})
		if cr["name"] == "documents" {
			docCheck = cr
			break
		}
	}

	if docCheck == nil {
		t.Fatal("expected documents check in results")
	}
	if docCheck["status"] != "ok" {
		t.Fatalf("expected documents check ok, got %v (detail=%v)", docCheck["status"], docCheck["detail"])
	}

	// 生产文档必须保持不变（未被 healthcheck 污染）
	after := len(ds.Query("生产文档", 10))
	if after != before {
		t.Fatalf("production doc store polluted: before=%d after=%d", before, after)
	}
}

func TestLLMReportCollection(t *testing.T) {
	p := &Plugin{name: "healthcheck"}
	if len(p.reports) != 0 {
		t.Fatal("expected empty reports")
	}
	p.mu.Lock()
	p.reports = append(p.reports, llmReport{ToolName: "test_tool", Status: "ok", Detail: "test passed"})
	count := len(p.reports)
	p.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 report, got %d", count)
	}
	if p.reports[0].ToolName != "test_tool" {
		t.Fatalf("expected tool_name=test_tool, got %s", p.reports[0].ToolName)
	}
}

// TestSafeReadonlyTool 验证 LLM 自检工具过滤：写类/副作用工具被拒绝，只读工具被放行。
func TestSafeReadonlyTool(t *testing.T) {
	// 只读：应放行
	readonly := []string{
		"memory_recall", "memory_introspect", "doc_query",
		"knowledge_search", "knowledge_list", "person_query",
		"person_network", "llm_list_sources", "output_list_channels",
		"terminal_list", "files_list",
	}
	for _, name := range readonly {
		if !isSafeReadonlyTool(name) {
			t.Errorf("expected readonly tool %q to be safe, but rejected", name)
		}
	}

	// 写/删/改/副作用：应被拒绝
	mutating := []string{
		"memory_commit", "memory_edit", "memory_purge", "memory_delete_entity",
		"memory_merge", "memory_block_merge",
		"knowledge_create", "knowledge_delete",
		"doc_commit", "doc_delete",
		"person_set_trait", "person_relate",
		"output_send__qq", "output_send__cli",
		"llm_set_source", "config_set",
		"timer_set", "plgreload", "plugin_disable",
		"cmd_run", "files_write", "files_delete", "terminal_create", "terminal_write",
		"terminal_close", "spawn_child",
	}
	for _, name := range mutating {
		if isSafeReadonlyTool(name) {
			t.Errorf("expected mutating tool %q to be rejected, but allowed", name)
		}
	}
}

// TestCollectToolDefsForLLMNoMutating 验证 collectToolDefsForLLM 不会把写类工具交给 LLM 自检。
func TestCollectToolDefsForLLMNoMutating(t *testing.T) {
	p := &Plugin{name: "healthcheck", selfToolNames: map[string]bool{"healthcheck": true, "healthcheck_report": true}}

	// 构造一个包含写类工具的 ToolDef 集合，注入工具注册表
	stage := agentCore.NewStageHost()
	defs := []sdk.ToolDef{
		{Name: "memory_recall", Plugin: "memory", Description: "recall"},
		{Name: "memory_commit", Plugin: "memory", Description: "commit"},
		{Name: "doc_query", Plugin: "doc", Description: "query"},
		{Name: "doc_commit", Plugin: "doc", Description: "commit doc"},
		{Name: "knowledge_search", Plugin: "knowledge", Description: "search"},
		{Name: "knowledge_create", Plugin: "knowledge", Description: "create"},
		{Name: "cmd_run", Plugin: "cmd", Description: "run cmd"},
		{Name: "files_list", Plugin: "files", Description: "list"},
	}
	for _, d := range defs {
		d := d
		stage.RegisterTool(d.Name, d, func(map[string]interface{}) (interface{}, error) { return nil, nil })
	}

	tc := newToolCapture()
	s := newTestSDK(sdk.SDKConfig{
		RegTool:  tc.RegisterTool,
		RegStage: tc.RegisterStage,
		RegAPI:   tc.RegisterAPI,
		Tool:     sdk.NewTool(stage, agentIO.NewIOManager()),
	})

	got := p.collectToolDefsForLLM(s, "")
	allowed := map[string]bool{}
	for _, d := range got {
		allowed[d.Name] = true
	}

	// 只读工具应被包含
	for _, name := range []string{"memory_recall", "doc_query", "knowledge_search", "files_list"} {
		if !allowed[name] {
			t.Errorf("expected readonly tool %q in LLM selftest set, missing", name)
		}
	}
	// 写类/副作用工具绝不能被交给 LLM
	for _, name := range []string{"memory_commit", "doc_commit", "knowledge_create", "cmd_run"} {
		if allowed[name] {
			t.Errorf("mutating tool %q must NOT be in LLM selftest set", name)
		}
	}
}

// TestCollectToolDefsForLLMPluginFilter 验证指定插件时只收集该插件的只读工具。
func TestCollectToolDefsForLLMPluginFilter(t *testing.T) {
	p := &Plugin{name: "healthcheck", selfToolNames: map[string]bool{"healthcheck": true, "healthcheck_report": true}}

	stage := agentCore.NewStageHost()
	defs := []sdk.ToolDef{
		{Name: "memory_recall", Plugin: "memory", Description: "recall"},
		{Name: "memory_commit", Plugin: "memory", Description: "commit"},
		{Name: "doc_query", Plugin: "doc", Description: "query"},
		{Name: "doc_commit", Plugin: "doc", Description: "commit doc"},
		{Name: "knowledge_search", Plugin: "knowledge", Description: "search"},
		{Name: "files_list", Plugin: "files", Description: "list"},
		{Name: "files_write", Plugin: "files", Description: "write"},
		{Name: "cmd_run", Plugin: "cmd", Description: "run cmd"},
	}
	for _, d := range defs {
		d := d
		stage.RegisterTool(d.Name, d, func(map[string]interface{}) (interface{}, error) { return nil, nil })
	}

	tc := newToolCapture()
	s := newTestSDK(sdk.SDKConfig{
		RegTool:  tc.RegisterTool,
		RegStage: tc.RegisterStage,
		RegAPI:   tc.RegisterAPI,
		Tool:     sdk.NewTool(stage, agentIO.NewIOManager()),
	})

	got := p.collectToolDefsForLLM(s, "files")
	allowed := map[string]bool{}
	for _, d := range got {
		allowed[d.Name] = true
	}
	if len(got) != 1 || !allowed["files_list"] {
		t.Fatalf("expected only files_list for files plugin, got %v", allowed)
	}
	for _, name := range []string{"memory_recall", "doc_query", "knowledge_search", "files_write"} {
		if allowed[name] {
			t.Errorf("tool %q must NOT be in files-filtered set", name)
		}
	}

	gotMemory := p.collectToolDefsForLLM(s, "memory")
	if len(gotMemory) != 1 || gotMemory[0].Name != "memory_recall" {
		t.Fatalf("expected only memory_recall for memory plugin, got %v", gotMemory)
	}

	// 全量时不丢任何只读工具
	all := p.collectToolDefsForLLM(s, "")
	if len(all) != 4 {
		t.Fatalf("expected 4 readonly tools unfiltered, got %d", len(all))
	}
}

// TestReportsToChecks 验证 LLM 上报明细转为细粒度检查项（ok/skip 通过, fail 失败）。
func TestReportsToChecks(t *testing.T) {
	p := &Plugin{}
	p.reports = []llmReport{
		{ToolName: "qq_get_message", Status: "ok", Detail: "正常"},
		{ToolName: "doc_query", Status: "skip", Detail: "无可测数据"},
		{ToolName: "knowledge_search", Status: "fail", Detail: "搜索超时"},
	}
	checks := p.reportsToChecks()
	if len(checks) != 3 {
		t.Fatalf("expected 3 checks, got %d", len(checks))
	}
	byName := map[string]checkResult{}
	for _, c := range checks {
		byName[c.Name] = c
	}
	if !byName["llm_tool/qq_get_message"].Pass {
		t.Error("ok report must pass")
	}
	if !byName["llm_tool/doc_query"].Pass {
		t.Error("skip report must pass")
	}
	if byName["llm_tool/knowledge_search"].Pass {
		t.Error("fail report must NOT pass")
	}
}
