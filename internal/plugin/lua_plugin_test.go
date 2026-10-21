package plugin

import (
	"os"
	"path/filepath"
	"testing"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	lua "github.com/yuin/gopher-lua"
)

func TestTryLoadLua_Basic(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"name": "testlua",
		"name_zh": "测试Lua",
		"name_en": "Test Lua",
		"version": "1.0.0",
		"entry": "main.lua"
	}`), 0644)

	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = {
  name = "testlua"
}

function plugin.start(sdk)
  sdk.log("info", "testlua started")
  sdk.register_tool("testlua_hello", {
    description = "Hello tool",
    parameters = {type = "object", properties = {}}
  }, function(args)
    return {content = "hello from lua"}
  end)
end

function plugin.stop()
  sdk.log("info", "testlua stopped")
end

return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "testlua", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}
	if plg.Name() != "testlua" {
		t.Fatalf("unexpected name: %s", plg.Name())
	}
	t.Logf("plugin loaded: %s", plg.Name())
}

func TestTryLoadLua_NoFile(t *testing.T) {
	dir := t.TempDir()
	plg, err := tryLoadLua(dir, "nonexistent", nil)
	if err != nil {
		t.Fatalf("tryLoadLua on empty dir should not error: %v", err)
	}
	if plg != nil {
		t.Fatal("expected nil for non-existent main.lua")
	}
}

func TestTryLoadLua_NoReturnTable(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"bad","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
-- just code, no return table
local x = 1
sdk.log("info", "no return table test")
`), 0644)

	plg, err := tryLoadLua(dir, "bad", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}
	t.Logf("loaded plugin without return table: %s", plg.Name())
}

func TestTryLoadLua_GlobalSDK(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"globalsdk","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
-- sdk is a global, should work without return table
sdk.log("info", "sdk is available as global")
sdk.register_tool("direct_tool", {
  description = "registered directly in top-level code"
}, function(args)
  return {result = "ok"}
end)
`), 0644)

	plg, err := tryLoadLua(dir, "globalsdk", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	if plg == nil {
		t.Fatal("tryLoadLua returned nil")
	}

	lp := plg.(*luaPlugin)
	lp.mu.Lock()
	toolCount := len(lp.tools)
	lp.mu.Unlock()
	if toolCount != 1 {
		t.Fatalf("expected 1 tool registration, got %d", toolCount)
	}
	t.Logf("tool registered during load phase: OK")
}

func TestLuaChannelsAndMemoryDefs(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"name": "chandefs",
		"version": "1.0.0",
		"entry": "main.lua"
	}`), 0644)

	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = { name = "chandefs" }

function plugin.start(sdk)
  sdk.register_tool("mem_tool", {
    description = "tool with memory defs",
    parameters = {type = "object", properties = {}},
    no_memory = true,
    cleaner = function(text) return "tool:" .. text end
  }, function(args)
    return {content = "ok"}
  end)

  sdk.register_output_channel("out_chan", 1, "output channel", {
    no_memory = true,
    cleaner = function(text) return "out:" .. text end
  }, function(args)
    return {ok = true}
  end)

  sdk.register_input_channel("in_chan", {
    no_memory = false,
    cleaner = function(text) return "in:" .. text end
  })
end

function plugin.stop() end
return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "chandefs", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}

	var gotTool *sdk.ToolDef
	var gotOutputCh *sdk.ChannelDef
	var gotOutputHandler sdk.ToolHandler
	var gotInputDef *sdk.ChannelDef

	regTool := func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		if name == "mem_tool" {
			gotTool = &def
		}
		return nil
	}
	regOutput := func(name string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
		if name == "out_chan" {
			gotOutputCh = &def
			gotOutputHandler = handler
		}
		return nil
	}
	regInput := func(name string, def sdk.ChannelDef) error {
		if name == "in_chan" {
			gotInputDef = &def
		}
		return nil
	}

	s := sdk.New("chandefs", sdk.SDKConfig{RegTool: regTool, RegOutput: regOutput, RegInput: regInput})

	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer plg.Stop()

	if gotTool == nil {
		t.Fatal("mem_tool not registered")
	}

	if !gotTool.NoMemory {
		t.Error("tool no_memory should be true")
	}
	if gotTool.Cleaner == nil {
		t.Fatal("tool cleaner should not be nil")
	}
	if out := gotTool.Cleaner("abc"); out != "tool:abc" {
		t.Errorf("tool cleaner result = %q, want %q", out, "tool:abc")
	}

	if gotOutputCh == nil {
		t.Fatal("output channel not registered")
	}
	if !gotOutputCh.NoMemory {
		t.Error("output channel no_memory should be true")
	}
	if gotOutputCh.Cleaner == nil {
		t.Fatal("output channel cleaner should not be nil")
	}
	if out := gotOutputCh.Cleaner("abc"); out != "out:abc" {
		t.Errorf("output channel cleaner result = %q, want %q", out, "out:abc")
	}
	if gotOutputHandler == nil {
		t.Fatal("output channel handler should not be nil")
	}
	res, err := gotOutputHandler(map[string]interface{}{"x": float64(1)})
	if err != nil {
		t.Fatalf("output handler error: %v", err)
	}
	if m, ok := res.(map[string]interface{}); !ok || m["ok"] != true {
		t.Errorf("output handler result = %#v, want {ok=true}", res)
	}

	if gotInputDef == nil {
		t.Fatal("input channel not registered")
	}
	if gotInputDef.NoMemory {
		t.Error("input channel no_memory should be false")
	}
	if gotInputDef.Cleaner == nil {
		t.Fatal("input channel cleaner should not be nil")
	}
	if out := gotInputDef.Cleaner("abc"); out != "in:abc" {
		t.Errorf("input channel cleaner result = %q, want %q", out, "in:abc")
	}
}

func TestLuaChannelsLoadPhaseStash(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"stashch","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
-- registered at load phase, before plugin.start(sdk)
sdk.register_output_channel("load_out", 2, "desc", {no_memory = true}, function(args) return {r = 1} end)
sdk.register_input_channel("load_in", {no_memory = true})
return { name = "stashch", start = function(sdk) end, stop = function() end }
`), 0644)

	plg, err := tryLoadLua(dir, "stashch", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	lp := plg.(*luaPlugin)

	lp.mu.Lock()
	outCount := len(lp.outputChs)
	inCount := len(lp.inputDefs)
	outReg := lp.outputChs["load_out"]
	inDef := lp.inputDefs["load_in"]
	lp.mu.Unlock()

	if outCount != 1 {
		t.Fatalf("expected 1 stashed output channel, got %d", outCount)
	}
	if inCount != 1 {
		t.Fatalf("expected 1 stashed input channel, got %d", inCount)
	}
	if !outReg.def.NoMemory {
		t.Error("stashed output channel should have NoMemory")
	}
	if !inDef.NoMemory {
		t.Error("stashed input channel should have NoMemory")
	}

	var gotOut, gotIn bool
	s := sdk.New("stashch", sdk.SDKConfig{
		RegOutput: func(name string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
			if name == "load_out" {
				if !def.NoMemory {
					t.Error("output channel NoMemory lost through Start")
				}
				if handler == nil {
					t.Error("output channel handler lost through Start")
				}
				gotOut = true
			}
			return nil
		},
		RegInput: func(name string, def sdk.ChannelDef) error {
			if name == "load_in" {
				if !def.NoMemory {
					t.Error("input channel NoMemory lost through Start")
				}
				gotIn = true
			}
			return nil
		},
	})

	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer plg.Stop()

	if !gotOut || !gotIn {
		t.Fatalf("channels not registered through Start: out=%v in=%v", gotOut, gotIn)
	}
}

func TestLuaAlignedAPIs(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"aligned","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = { name = "aligned" }

_G.res = {}
_G.stage_count = 0
_G.stage_ctx = nil

function plugin.start(sdk)
  sdk.register_stage("before_toolcall", function(ctx)
    _G.stage_count = _G.stage_count + 1
    _G.stage_ctx = ctx
  end, "own_tools")

  local r, err = sdk.memory.recall("alice", 2)
  _G.res.recall_ok = (r ~= nil and err == nil)
  _G.res.recall_entities_type = type(r and r.entities)

  local r2, err2 = sdk.memory.recall({"alice"}, 2)
  _G.res.recall_array_ok = (r2 ~= nil and err2 == nil)
  _G.res.recall_array_err = err2

  local _, cerr = sdk.memory.commit({{subject="a", relation="r", object="b"}})
  _G.res.commit_ok = (cerr == nil)

  local _, ierr = sdk.memory.introspect()
  _G.res.introspect_ok = (ierr == nil)
  local _, merr = sdk.memory.merge("a", "b")
  _G.res.merge_ok = (merr == nil)
  local _, perr = sdk.memory.purge({}, true)
  _G.res.purge_ok = (perr == nil)

  local docs = sdk.doc.query("q", 3)
  _G.res.doc_type = type(docs)
  local _, dierr = sdk.doc.insert({id="d1", title="t", content="c"})
  _G.res.doc_insert_ok = (dierr == nil)
  sdk.doc.remove("d1")
  _G.res.doc_stats = type(sdk.doc.stats())

  local kres, kerr = sdk.knowledge.search("q")
  _G.res.kn_search_ok = (kerr == nil and type(kres) == "table")
  local _, kadderr = sdk.knowledge.add("tag", "content")
  _G.res.kn_add_ok = (kadderr == nil)
  local klist = sdk.knowledge.list()
  _G.res.kn_list_ok = (type(klist) == "table")

  local _, tmerr = sdk.text_memory.append({role="user", content="hello", timestamp=123, channel="c1"})
  _G.res.tm_ok = (tmerr == nil)

  local srcs = sdk.llm.list_sources()
  _G.res.llm_list_type = type(srcs)
  local _, serr = sdk.llm.set_source("default")
  _G.res.llm_set_ok = (serr == nil)
  _G.res.llm_cur_type = type(sdk.llm.current_source())

  local p = sdk.social.get_person("alice")
  _G.res.social_person = p
  _G.res.social_net_type = type(sdk.social.get_network("alice", 1))
  local tr = sdk.social.get_trait("alice", "kind")
  _G.res.social_trait = tr
  _G.res.social_rels_type = type(sdk.social.get_relations("alice"))
  _G.res.social_list_type = type(sdk.social.list_persons())

  sdk.set_auto_restart(true)
  _G.res.auto_restart_called = true

  sdk.settings.set_core("test_lua_key", "lv")
  _G.res.core_val = sdk.settings.get_core("test_lua_key")
  local cores = sdk.settings.list_core("test")
  _G.res.core_list_has = (type(cores) == "table" and #cores > 0)
  sdk.settings.set_plugin("other", "okey", "oval")
  _G.res.plugin_val = sdk.settings.get_plugin("other", "okey")
  _G.res.plugin_list = sdk.settings.list_plugin("other", "")
  sdk.settings.register_def({key="def_key", type="string", display_name="DK", default="dv"})
  _G.res.sett_list_type = type(sdk.settings.list(""))
  _G.res.def_val = sdk.get_setting("def_key")
  _G.res.defs_type = type(sdk.settings.defs(""))
  _G.res.dump_type = type(sdk.settings.dump())
  _G.res.plugins_type = type(sdk.settings.plugins())

  sdk.log("info", "aligned api test done")
end

function plugin.stop() end
return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "aligned", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	lp := plg.(*luaPlugin)

	var capturedHandler sdk.StageHandler
	reg := internalConfig.NewConfigRegistry("")
	sett := sdk.NewSettings("aligned", reg)
	// 目标插件须先注册配置定义才会建表（任意 scope 不再隐式建表）
	sdk.NewSettings("other", reg).RegisterDef(sdk.ConfigDef{Key: "okey", Default: "oval", Type: "string"})
	s := sdk.New("aligned", sdk.SDKConfig{
		Settings: sett,
		RegStage: func(stage sdk.Stage, handler sdk.StageHandler) {
			capturedHandler = handler
		},
	})

	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer plg.Stop()

	L := lp.L
	res, ok := L.GetGlobal("res").(*lua.LTable)
	if !ok {
		t.Fatal("res global missing")
	}
	getBool := func(key string) bool { return lua.LVAsBool(res.RawGetString(key)) }

	if !getBool("recall_ok") {
		t.Error("memory.recall should succeed (nil-safe)")
	}
	if res.RawGetString("recall_entities_type").String() != "table" {
		t.Error("memory.recall entities should be a table")
	}
	if !getBool("recall_array_ok") {
		t.Error("memory.recall with array query should succeed: " + res.RawGetString("recall_array_err").String())
	}
	for _, k := range []string{"commit_ok", "introspect_ok", "merge_ok", "purge_ok", "doc_insert_ok", "kn_search_ok", "kn_add_ok", "kn_list_ok", "tm_ok", "llm_set_ok"} {
		if !getBool(k) {
			t.Errorf("%s failed", k)
		}
	}
	if res.RawGetString("doc_type").String() != "table" || res.RawGetString("doc_stats").String() != "table" {
		t.Error("doc.query/stats should return tables")
	}
	if res.RawGetString("llm_list_type").String() != "table" {
		t.Error("llm list should return tables")
	}
	if res.RawGetString("llm_cur_type").String() != "nil" {
		t.Error("llm.current_source should be nil when LLMAPI not wired")
	}
	trTbl, ok := res.RawGetString("social_trait").(*lua.LTable)
	if !ok {
		t.Fatal("social.get_trait should return a table")
	}
	if lua.LVAsBool(trTbl.RawGetString("found")) {
		t.Error("social.get_trait found should be false when SocialAPI not wired")
	}
	if res.RawGetString("social_person").Type() != lua.LTNil {
		t.Error("social.get_person should be nil when SocialAPI not wired")
	}
	if res.RawGetString("social_net_type").String() != "table" ||
		res.RawGetString("social_rels_type").String() != "table" ||
		res.RawGetString("social_list_type").String() != "table" {
		t.Error("social list/network/relations should return tables")
	}
	if !getBool("auto_restart_called") {
		t.Error("set_auto_restart should be callable")
	}
	if res.RawGetString("core_val").String() != "lv" {
		t.Errorf("settings.get_core after set_core = %v, want lv", res.RawGetString("core_val"))
	}
	if !getBool("core_list_has") {
		t.Error("settings.list_core should list set key")
	}
	if res.RawGetString("plugin_val").String() != "oval" {
		t.Errorf("settings.get_plugin = %v, want oval", res.RawGetString("plugin_val"))
	}
	if res.RawGetString("def_val").String() != "dv" {
		t.Errorf("register_def default should be readable via get_setting, got %v", res.RawGetString("def_val"))
	}
	for _, k := range []string{"sett_list_type", "defs_type", "dump_type", "plugins_type"} {
		if res.RawGetString(k).String() != "table" {
			t.Errorf("%s should be a table", k)
		}
	}

	// own_tools scope: 只有本插件工具触发
	if capturedHandler == nil {
		t.Fatal("stage handler not registered")
	}
	capturedHandler(&sdk.StageContext{
		RawMessage: "hi",
		LLMText:    "llm text",
		ToolCalls:  []sdk.ToolCall{{Plugin: "aligned", Name: "x"}},
	})
	capturedHandler(&sdk.StageContext{
		ToolCalls: []sdk.ToolCall{{Plugin: "other", Name: "y"}},
	})
	if got := int(lua.LVAsNumber(L.GetGlobal("stage_count"))); got != 1 {
		t.Fatalf("own_tools stage should fire only for own plugin, fired %d", got)
	}
	ctxTbl, ok := L.GetGlobal("stage_ctx").(*lua.LTable)
	if !ok {
		t.Fatal("stage_ctx global missing")
	}
	if ctxTbl.RawGetString("llm_text").String() != "llm text" {
		t.Errorf("stage ctx llm_text = %v, want 'llm text'", ctxTbl.RawGetString("llm_text"))
	}
	if ctxTbl.RawGetString("tool_calls").Type() != lua.LTTable {
		t.Error("stage ctx tool_calls should be a table")
	}
	if ctxTbl.RawGetString("raw_message").String() != "hi" {
		t.Errorf("stage ctx raw_message = %v, want 'hi'", ctxTbl.RawGetString("raw_message"))
	}
}

func TestLuaStageWriteback(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"wblua","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = { name = "wblua" }

function plugin.start(sdk)
  sdk.register_stage("on_input", function(ctx)
    ctx.raw_message = "[清洗]" .. ctx.raw_message
    ctx.final_text = "改写后的最终文本"
  end)
  sdk.register_stage("post_action", function(ctx)
    ctx.llm_text = ctx.llm_text .. "[尾部标记]"
  end)
end

function plugin.stop() end
return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "wblua", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}

	var handlers = map[sdk.Stage]sdk.StageHandler{}
	reg := internalConfig.NewConfigRegistry("")
	sett := sdk.NewSettings("wblua", reg)
	s := sdk.New("wblua", sdk.SDKConfig{
		Settings: sett,
		RegStage: func(stage sdk.Stage, handler sdk.StageHandler) {
			handlers[stage] = handler
		},
	})

	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer plg.Stop()

	// 触发 on_input stage，验证 Lua 修改写回内核 sc
	onInput, ok := handlers[sdk.StageOnInput]
	if !ok {
		t.Fatal("on_input handler not registered")
	}
	sc := &sdk.StageContext{RawMessage: "原始消息"}
	if err := onInput(sc); err != nil {
		t.Fatalf("on_input: %v", err)
	}
	if sc.RawMessage != "[清洗]原始消息" {
		t.Errorf("raw_message writeback: got %q, want %q", sc.RawMessage, "[清洗]原始消息")
	}
	if sc.FinalText != "改写后的最终文本" {
		t.Errorf("final_text writeback: got %q", sc.FinalText)
	}

	// 触发 post_action stage
	post, ok := handlers[sdk.StagePostAction]
	if !ok {
		t.Fatal("post_action handler not registered")
	}
	sc2 := &sdk.StageContext{LLMText: "模型输出"}
	if err := post(sc2); err != nil {
		t.Fatalf("post_action: %v", err)
	}
	if sc2.LLMText != "模型输出[尾部标记]" {
		t.Errorf("llm_text writeback: got %q, want %q", sc2.LLMText, "模型输出[尾部标记]")
	}
}

// TestLuaEventsSubscribeAndStopCleanup 覆盖 sdk.events.subscribe：
//  1. 订阅真的能收到内核事件（走内部 SDK 的 Subscribe，不是永远为 nil 的公共 Events()）；
//  2. Stop 会取消订阅，之后 Publish 不得再触碰已 Close 的 LState。
func TestLuaEventsSubscribeAndStopCleanup(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"evlua","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = { name = "evlua" }

function plugin.start(sdk)
  _G.hits = 0
  local unsub, err = sdk.events.subscribe("agent_output", function(evt)
    _G.hits = _G.hits + 1
    _G.last_type = evt.type
    _G.last_source = evt.source
  end)
  _G.sub_err = err
  _G.unsub_type = type(unsub)
end

function plugin.stop() end
return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "evlua", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	lp := plg.(*luaPlugin)

	bus := events.NewBus()
	reg := internalConfig.NewConfigRegistry("")
	sett := sdk.NewSettings("evlua", reg)
	s := sdk.New("evlua", sdk.SDKConfig{EventBus: bus, Settings: sett})

	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	L := lp.L
	if errStr := L.GetGlobal("sub_err").String(); errStr != "nil" {
		t.Fatalf("subscribe returned error: %s", errStr)
	}
	if got := L.GetGlobal("unsub_type").String(); got != "function" {
		t.Fatalf("subscribe should return an unsubscribe function, got %s", got)
	}

	bus.Publish(&events.Event{Type: events.EventAgentOutput, Source: "test-src"})
	if hits := int(lua.LVAsNumber(L.GetGlobal("hits"))); hits != 1 {
		t.Fatalf("event handler hits = %d, want 1", hits)
	}
	if got := L.GetGlobal("last_source").String(); got != "test-src" {
		t.Fatalf("event source = %q, want test-src", got)
	}

	// Stop 取消订阅 + 关 L；此后再 Publish 不得 panic / use-after-close。
	if err := plg.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	bus.Publish(&events.Event{Type: events.EventAgentOutput, Source: "after-stop"})
}

// TestLuaSyncInjectUnavailable 钉住「Lua 同步注入必须立即返回明确错误、不能挂死」。
// 背景：同步注入要等本轮回复，而 Lua 回调持有插件锁 ⇒ 原实现必然自锁。
func TestLuaSyncInjectUnavailable(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{"name":"synclua","entry":"main.lua"}`), 0644)
	os.WriteFile(filepath.Join(dir, "main.lua"), []byte(`
local plugin = { name = "synclua" }
function plugin.start(sdk)
  local r, err = sdk.inject_input_sync("src", "ch", "hello")
  _G.sync_reply = r
  _G.sync_err = err
  local _, err2 = sdk.inject_input_sync_opts("src", "ch", "hello", { no_memory = true })
  _G.sync_err2 = err2
  local _, err3 = sdk.inject_input_media_sync("src", "ch", "hi", {})
  _G.sync_media_err = err3
end
function plugin.stop() end
return plugin
`), 0644)

	plg, err := tryLoadLua(dir, "synclua", nil)
	if err != nil {
		t.Fatalf("tryLoadLua failed: %v", err)
	}
	reg := internalConfig.NewConfigRegistry("")
	sett := sdk.NewSettings("synclua", reg)
	s := sdk.New("synclua", sdk.SDKConfig{Settings: sett})
	if err := plg.Start(s); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer plg.Stop()

	L := plg.(*luaPlugin).L
	if L.GetGlobal("sync_reply").Type() != lua.LTNil {
		t.Errorf("sync inject should return nil reply, got %v", L.GetGlobal("sync_reply"))
	}
	for _, k := range []string{"sync_err", "sync_err2", "sync_media_err"} {
		if L.GetGlobal(k).Type() != lua.LTString {
			t.Errorf("%s should be an error string (unavailable), got %v", k, L.GetGlobal(k))
		}
	}
}
