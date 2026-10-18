package plugin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	luaSDK "github.com/JianFeeeee/HomeAgent/internal/lua/sdk"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	lua "github.com/yuin/gopher-lua"
)

// TestLuaSDKMockSingleSource 守住「Lua mock 只有一份事实源」。
//
// 三份 sdk.lua（内核内嵌 / 工具链模板 / 项目副本）历史上各自漂移过，
// 表现为「mock 里有的 API，内核运行时是 nil」这类静默失配。
// 事实源是 SDK 仓的 sdk/lua/sdk.lua，内核副本由
// third_party/homeagent-sdk/scripts/sync-lua-sdk.sh 同步。
func TestLuaSDKMockSingleSource(t *testing.T) {
	canonical, err := os.ReadFile(filepath.Join("..", "..", "third_party", "homeagent-sdk", "sdk", "lua", "sdk.lua"))
	if err != nil {
		t.Skipf("SDK repo canonical sdk.lua not available: %v", err)
	}
	if string(canonical) != luaSDK.SDKSource {
		t.Fatal("内核内嵌 sdk.lua 与 SDK 仓 sdk/lua/sdk.lua 不一致；" +
			"请跑 third_party/homeagent-sdk/scripts/sync-lua-sdk.sh")
	}
}

var luaMockFuncRe = regexp.MustCompile(`(?m)^function sdk\.([A-Za-z0-9_.]+)\s*\(`)

// TestLuaBridgeCoversMock 守住「mock 承诺的每个函数，运行时都有绑定」。
//
// 只查 mock → 运行时这一向：mock 定义了但没绑定，插件会先看到 mock 能调、
// 之后内核里是 nil（或反向的假象）。反向（运行时多出未文档化的函数）无害。
func TestLuaBridgeCoversMock(t *testing.T) {
	bridge, err := os.ReadFile("lua_plugin.go")
	if err != nil {
		t.Fatalf("read lua_plugin.go: %v", err)
	}
	src := string(bridge)

	// 纯 Lua 实现，不经内核绑定。
	exempt := map[string]bool{"json.encode": true, "json.decode": true}

	matches := luaMockFuncRe.FindAllStringSubmatch(luaSDK.SDKSource, -1)
	if len(matches) < 40 {
		t.Fatalf("parsed only %d sdk.* functions from mock; parser likely broken", len(matches))
	}
	for _, m := range matches {
		full := m[1]
		if exempt[full] {
			continue
		}
		leaf := full
		if i := strings.LastIndex(full, "."); i >= 0 {
			leaf = full[i+1:]
		}
		if !strings.Contains(src, `RawSetString("`+leaf+`"`) {
			t.Errorf("sdk.%s: mock 有定义，但 lua_plugin.go 没有 RawSetString(%q) 绑定", full, leaf)
		}
	}
}

func luaTableFrom(t *testing.T, code string) (*lua.LState, *lua.LTable) {
	t.Helper()
	L := lua.NewState()
	if err := L.DoString("return " + code); err != nil {
		L.Close()
		t.Fatalf("eval lua table: %v", err)
	}
	tbl, ok := L.Get(-1).(*lua.LTable)
	if !ok {
		L.Close()
		t.Fatalf("expected a table from %q", code)
	}
	L.Pop(1)
	return L, tbl
}

func TestLuaParseInjectOptions(t *testing.T) {
	L, tbl := luaTableFrom(t, `{
		no_memory = true,
		context_policy = "prune",
		cleaner_name = "sanitize",
		priority = "L2",
	}`)
	defer L.Close()

	L.Push(tbl)
	got := parseInjectOptions(L, 1)
	L.Pop(1)

	if !got.NoMemory {
		t.Error("NoMemory should be true")
	}
	if got.ContextPolicy != sdk.ContextPolicyPrune {
		t.Errorf("ContextPolicy = %q, want prune", got.ContextPolicy)
	}
	if got.CleanerName != "sanitize" {
		t.Errorf("CleanerName = %q, want sanitize", got.CleanerName)
	}
	if got.Priority != sdk.PriorityL2 {
		t.Errorf("Priority = %q, want L2", got.Priority)
	}

	// 缺省/非表 = 零值（记入记忆 + 不裁剪），与旧三参数注入等价。
	//
	// ★ 不能用 `z != sdk.InjectOptions{}` 比较：InjectOptions 现在含
	// `Payload map[string]interface{}`（插件业务字段扩展口），结构体含 map
	// 就**不可比较**，编译直接报 invalid operation。
	// 逐字段验证，顺便把「哪些字段应为空」写清楚。
	if z := parseInjectOptions(L, 99); z.NoMemory || z.ContextPolicy != "" ||
		z.RecallPolicy != "" || z.ScenePolicy != "" || z.CleanerName != "" ||
		z.Priority != "" || len(z.Payload) != 0 {
		t.Errorf("missing opts should be zero value, got %#v", z)
	}
}

func TestLuaContentBlocksParse(t *testing.T) {
	L, tbl := luaTableFrom(t, `{
		{ type = "text", text = "看图" },
		{ type = "image_url", image_url = { url = "data:image/png;base64,AAAA", detail = "high" } },
		{ type = "audio_url", audio_url = { url = "https://x/a.mp3" } },
	}`)
	defer L.Close()

	L.Push(tbl)
	blocks := luaToContentBlocks(L, 1)
	L.Pop(1)

	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "看图" {
		t.Errorf("block[0] = %#v", blocks[0])
	}
	if blocks[1].ImageURL == nil || blocks[1].ImageURL.URL != "data:image/png;base64,AAAA" || blocks[1].ImageURL.Detail != "high" {
		t.Errorf("block[1] image_url = %#v", blocks[1].ImageURL)
	}
	if blocks[2].AudioURL == nil || blocks[2].AudioURL.URL != "https://x/a.mp3" {
		t.Errorf("block[2] audio_url = %#v", blocks[2].AudioURL)
	}
}

func TestLuaAttachmentsBase64(t *testing.T) {
	// "hello" 的 base64 是 aGVsbG8=
	L, tbl := luaTableFrom(t, `{
		{ digest = "sha256:abc", mime = "image/png", name = "a.png" },
		{ mime = "image/jpeg", data = "aGVsbG8=" },
		{ mime = "image/png", data = "!!!not-base64!!!" },
	}`)
	defer L.Close()

	atts := luaToAttachments(L, tbl)

	if len(atts) != 3 {
		t.Fatalf("got %d attachments, want 3", len(atts))
	}
	if atts[0].Digest != "sha256:abc" || atts[0].MIME != "image/png" || atts[0].Name != "a.png" || atts[0].Data != nil {
		t.Errorf("att[0] = %#v", atts[0])
	}
	if string(atts[1].Data) != "hello" {
		t.Errorf("att[1] data = %q, want hello", string(atts[1].Data))
	}
	// 坏 base64 只丢 data，不整单失败——坏附件不应阻断记忆写入。
	if atts[2].Data != nil {
		t.Errorf("att[2] bad base64 should be dropped, got %q", string(atts[2].Data))
	}
}

func TestLuaDefinitionsCarryContextPolicy(t *testing.T) {
	L, tbl := luaTableFrom(t, `{
		description = "t",
		no_memory = true,
		context_policy = "prune",
	}`)
	defer L.Close()

	plg := &luaPlugin{name: "cp"}
	def := parseToolDef(L, tbl, plg, "t")
	if def.ContextPolicy != sdk.ContextPolicyPrune {
		t.Errorf("ToolDef.ContextPolicy = %q, want prune", def.ContextPolicy)
	}
	if !def.NoMemory {
		t.Error("ToolDef.NoMemory should be true")
	}

	chDef := parseChannelDef(L, tbl, plg)
	if chDef.ContextPolicy != sdk.ContextPolicyPrune {
		t.Errorf("ChannelDef.ContextPolicy = %q, want prune", chDef.ContextPolicy)
	}
}
