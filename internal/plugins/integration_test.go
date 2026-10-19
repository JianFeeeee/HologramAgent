package plugins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "github.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	luaVM "github.com/JianFeeeee/HomeAgent/internal/lua"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	doc "github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	cli "github.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	"github.com/JianFeeeee/HomeAgent/internal/plugins/webui"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

type testPluginEnv struct {
	tmpDir    string
	stageHost *agentCore.StageHost
	iom       *agentIO.IOManager
	pluginReg *plugin.Registry
	memDB     *memory.GraphDB
	ks        *knowledge.Store
	docStore  *doc.Store
	cfgReg    *internalConfig.ConfigRegistry
}

func setupIntegration(t *testing.T) *testPluginEnv {
	return setupIntegrationWithProvider(t, nil)
}

func setupIntegrationWithProvider(t *testing.T, pm *agentAPI.ProviderManager) *testPluginEnv {
	t.Helper()

	// 测试绝不能绑生产端口。这里加载的是**全部内置插件**（含 webui），
	// 而 webui 默认监听 :8080 —— 那正是生产实例的端口。
	// 不覆盖的后果不是测试失败，而是**测试静默地跟生产抢端口**：
	// 先到者胜，另一个 bind 失败。webui 现在会在 bind 失败时明确报错，
	// 于是表现为「测试随机失败」+「生产 WebUI 随机死掉」，且两边看起来
	// 互不相干，极难定位（实际踩到过）。
	//
	// 用 SetListenOverride 指到 127.0.0.1:0（内核分配空闲端口）：
	// 测试拿到真实可用的 HTTP 服务，且与任何固定端口实例互不干扰。
	if prev := webui.ListenOverride(); prev == "" {
		webui.SetListenOverride("127.0.0.1:0")
		t.Cleanup(func() { webui.SetListenOverride(prev) })
	}

	tmpDir, err := os.MkdirTemp("", "hc_integration_*")
	if err != nil {
		t.Fatal(err)
	}

	stageHost := agentCore.NewStageHost()
	iom := agentIO.NewIOManager()
	pluginReg := plugin.NewRegistry()

	pluginReg.SetIOManager(iom)
	pluginReg.SetMemory(nil)

	pluginReg.SetToolRegistrar(func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		return stageHost.RegisterTool(name, def, handler)
	})
	pluginReg.SetStageRegistrar(func(stage sdk.Stage, handler sdk.StageHandler) {
		stageHost.RegisterStage(stage, handler)
	})
	pluginReg.SetAPIRegistrar(func(name string) error {
		return nil
	})

	memDB, err := memory.NewGraphDB(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	pluginReg.SetMemory(memDB)

	ks := knowledge.NewStore(filepath.Join(tmpDir, "knowledge"))
	if err := ks.Start(); err != nil {
		t.Fatal(err)
	}

	docStore := doc.NewStore(filepath.Join(tmpDir, "documents"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		t.Fatal(err)
	}

	pluginReg.SetStageHost(stageHost)
	pluginReg.SetProviderManager(pm)
	pluginReg.SetKnowledge(ks)
	pluginReg.SetDocStore(docStore)

	cli.DefaultSocket = filepath.Join(tmpDir, "cli.sock")

	// 经 ConfigRegistry 装配内核路径配置（clawhubadapter/pluginmgr 等经 SDK settings 读取）
	cfgReg := internalConfig.NewConfigRegistry("")
	cfgReg.SeedDefaults(tmpDir)

	// ★ 预置临时端口，避免测试之间（以及本机生产实例）抢固定默认端口。
	//
	// 为什么必须在 Load 之前预置：插件表由 RegisterDef 在插件 Start 时创建，
	// 此时才能 Set；而端口冲突发生在 Start 内部（net.Listen 失败即 HTTP 服务
	// 静默不启动，或测试二进制被信号打断）。SetPluginConfig 会先建表再写，
	// 正好填补这个时序缺口。
	//
	// 用 :0 让 OS 分配空闲端口——固定端口在「并行跑测试」或「本机有 homed
	// 常驻」时必然周期性失败（实测：干净树连跑 20 轮也复现 2 次）。
	for _, kv := range []struct{ plugin, key string }{
		{"pluginmgr", "http_addr"},
		{"remotedevice", "listen_addr"},
	} {
		if err := cfgReg.SetPluginConfig(kv.plugin, kv.key, "127.0.0.1:0"); err != nil {
			t.Fatalf("预置 %s.%s 临时端口: %v", kv.plugin, kv.key, err)
		}
	}
	// webui 的监听地址走独立旁路（SetListenOverride 优先级高于 settings，
	// 因为历史上内核在插件表建立前写 settings 会失败）。
	webui.SetListenOverride("127.0.0.1:0")
	t.Cleanup(func() { webui.SetListenOverride("") })

	pluginReg.SetConfigRegistry(cfgReg)

	plgDir := filepath.Join(tmpDir, "plugins")
	os.MkdirAll(plgDir, 0755)
	if err := pluginReg.Load(plgDir); err != nil {
		t.Fatal(err)
	}

	return &testPluginEnv{
		tmpDir:    tmpDir,
		stageHost: stageHost,
		iom:       iom,
		pluginReg: pluginReg,
		memDB:     memDB,
		ks:        ks,
		docStore:  docStore,
		cfgReg:    cfgReg,
	}
}

func (e *testPluginEnv) cleanup() {
	e.pluginReg.StopAll()
	e.memDB.Close()
	e.ks.Stop()
	e.docStore.Stop()
	os.RemoveAll(e.tmpDir)
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func TestIntegrationAllPluginsRegister(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	expectedTools := []string{
		"cmd_run",
		"terminal_create", "terminal_write", "terminal_read",
		"terminal_resize", "terminal_close", "terminal_list",
		"healthcheck", "healthcheck_plugins", "healthcheck_tools", "healthcheck_memory", "healthcheck_perf",
		// ⚠️ timer_set **不在**这里（2026-10-19 起）。
		//
		// 本用例的语义是「编译期内置插件是否都注册了工具」，set 走的是本地
		// 装配（setupIntegration 只加载 internal/plugins 下的内置包）。
		// timer 已搬为**外部插件**（plugins/timer，.hmap），不参与本进程装配，
		// 所以它出现在这个列表里只会造出「永远绿不了」的假失败。
		//
		// 外部插件的工具是否真的注册成功，由另一层负责：装完 .hmap 后
		// 用 timer_list / timer_set 实调一次（见交付报告里的现网验证）。
		// 别为了「让它绿」把 timer 加回 all.go —— 那会让内置与外部两份
		// 同时存在，工具重名而胜者取决于加载顺序。
	}

	defs := env.stageHost.GetToolDefs()
	registered := make(map[string]bool)
	for _, def := range defs {
		registered[def.Name] = true
	}

	var missing []string
	for _, tool := range expectedTools {
		if !registered[tool] {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		all := make([]string, 0, len(defs))
		for _, d := range defs {
			all = append(all, d.Name)
		}
		t.Fatalf("missing tools: %v\nall registered: %v", missing, all)
	}
	t.Logf("all %d expected tools registered (total: %d)", len(expectedTools), len(defs))
}

// ---------------------------------------------------------------------------
// cmd_run
// ---------------------------------------------------------------------------

func TestIntegrationCmdRun(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("cmd_run", map[string]interface{}{
		"command": "echo hello_world",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["exit_code"] != 0.0 {
		t.Fatalf("expected exit_code 0, got %v", resp["exit_code"])
	}
	stdout, ok := resp["stdout"].(string)
	if !ok || stdout != "hello_world" {
		t.Fatalf("expected stdout 'hello_world', got %q", stdout)
	}
	t.Logf("cmd_run OK: exit_code=0 stdout=%q", stdout)
}

func TestIntegrationCmdRunWithWorkdir(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("cmd_run", map[string]interface{}{
		"command": "pwd",
		"workdir": "/tmp",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["exit_code"] != 0.0 {
		t.Fatalf("expected exit_code 0, got %v", resp["exit_code"])
	}
	stdout := resp["stdout"].(string)
	if stdout != "/tmp" {
		t.Fatalf("expected stdout '/tmp', got %q", stdout)
	}
	t.Logf("cmd_run workdir OK: stdout=%q", stdout)
}

func TestIntegrationCmdRunInvalidTimeout(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("cmd_run", map[string]interface{}{
		"command": "echo ok",
		"timeout": "not-a-duration",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	errMsg, ok := resp["error"].(string)
	if !ok || errMsg == "" {
		t.Fatalf("expected error for invalid timeout, got %v", resp)
	}
	t.Logf("cmd_run invalid_timeout OK: error=%q", errMsg)
}

func TestIntegrationCmdRunStderr(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("cmd_run", map[string]interface{}{
		"command": "sh -c \"echo stderr_test >&2\"",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["exit_code"] != 0.0 {
		t.Fatalf("expected exit_code 0, got %v", resp["exit_code"])
	}
	stderr := resp["stderr"].(string)
	if stderr != "stderr_test" {
		t.Fatalf("expected stderr 'stderr_test', got %q", stderr)
	}
	t.Logf("cmd_run stderr OK: stderr=%q", stderr)
}

// ---------------------------------------------------------------------------
// Terminal (PTY)
// ---------------------------------------------------------------------------

func TestIntegrationPtyCreateListClose(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	// Create
	result, err := env.stageHost.ExecuteTool("terminal_create", map[string]interface{}{
		"shell": "/bin/sh",
		"name":  "hci_test_shell",
	})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	id, ok := resp["id"].(string)
	if !ok || id == "" {
		status := resp["status"]
		detail := resp["detail"]
		if status == "error" {
			t.Skipf("PTY not available: %v", detail)
		}
		t.Fatalf("expected non-empty terminal id, got id=%q status=%v detail=%v", id, status, detail)
	}
	t.Logf("terminal_create OK: id=%s", id)

	// List
	result2, err := env.stageHost.ExecuteTool("terminal_list", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	data2, _ := json.Marshal(result2)
	var resp2 map[string]interface{}
	json.Unmarshal(data2, &resp2)

	terminals, ok := resp2["terminals"].([]interface{})
	if !ok {
		t.Fatalf("expected terminals array, got %T", resp2["terminals"])
	}
	if len(terminals) < 1 {
		t.Fatal("expected at least 1 terminal")
	}
	t.Logf("terminal_list OK: %d terminals", len(terminals))

	// Close
	_, err = env.stageHost.ExecuteTool("terminal_close", map[string]interface{}{
		"id": id,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("terminal_close OK: id=%s", id)
}

func TestIntegrationPtyInteractive(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	createResult, err := env.stageHost.ExecuteTool("terminal_create", map[string]interface{}{
		"shell": "/bin/sh",
		"name":  "hci_interactive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cdata, _ := json.Marshal(createResult)
	var cresp map[string]interface{}
	json.Unmarshal(cdata, &cresp)

	id, ok := cresp["id"].(string)
	if !ok || id == "" {
		status := cresp["status"]
		if status == "error" {
			t.Skipf("PTY not available: %v", cresp["detail"])
		}
		t.Fatalf("expected terminal id, got %v", cresp)
	}

	// Write a command
	_, err = env.stageHost.ExecuteTool("terminal_write", map[string]interface{}{
		"id":    id,
		"input": "echo pty_works\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(500 * time.Millisecond)

	// Read output
	readResult, err := env.stageHost.ExecuteTool("terminal_read", map[string]interface{}{
		"id": id,
	})
	if err != nil {
		t.Fatal(err)
	}
	rdata, _ := json.Marshal(readResult)
	var rresp map[string]interface{}
	json.Unmarshal(rdata, &rresp)

	output, ok := rresp["output"].(string)
	if !ok || output == "" {
		t.Fatalf("expected output, got output=%q response=%v", output, rresp)
	}
	t.Logf("terminal_write+read OK: output=%q", output)

	// Close
	env.stageHost.ExecuteTool("terminal_close", map[string]interface{}{
		"id": id,
	})
}

func TestIntegrationPtyResize(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	createResult, err := env.stageHost.ExecuteTool("terminal_create", map[string]interface{}{
		"shell": "/bin/sh",
		"name":  "hci_resize_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cdata, _ := json.Marshal(createResult)
	var cresp map[string]interface{}
	json.Unmarshal(cdata, &cresp)

	id, ok := cresp["id"].(string)
	if !ok || id == "" {
		status := cresp["status"]
		if status == "error" {
			t.Skipf("PTY not available: %v", cresp["detail"])
		}
		t.Fatalf("expected terminal id, got %v", cresp)
	}

	_, err = env.stageHost.ExecuteTool("terminal_resize", map[string]interface{}{
		"id":   id,
		"rows": 80.0,
		"cols": 200.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("terminal_resize OK: id=%s rows=80 cols=200", id)

	env.stageHost.ExecuteTool("terminal_close", map[string]interface{}{
		"id": id,
	})
}

// ---------------------------------------------------------------------------
// Healthcheck
// ---------------------------------------------------------------------------

func TestIntegrationHealthcheck(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("healthcheck", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}
	checks := resp["checks"].([]interface{})
	if len(checks) == 0 {
		t.Fatal("expected non-empty checks array")
	}
	t.Logf("healthcheck OK: %d checks, status=%v", len(checks), resp["status"])
}

func TestIntegrationHealthcheckPlugins(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("healthcheck_plugins", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}
	t.Logf("healthcheck_plugins OK: status=%v", resp["status"])
}

func TestIntegrationHealthcheckToolsList(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("healthcheck_tools", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}
	t.Logf("healthcheck_tools OK: status=%v", resp["status"])
}

func TestIntegrationHealthcheckMemory(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	result, err := env.stageHost.ExecuteTool("healthcheck_memory", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}
	t.Logf("healthcheck_memory OK: status=%v", resp["status"])
}

func TestIntegrationToolNotFound(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	_, err := env.stageHost.ExecuteTool("nonexistent_tool_xyz", nil)
	if err == nil {
		t.Fatal("expected error for nonexistent tool")
	}
	t.Logf("tool_not_found OK: err=%v", err)
}

func TestIntegrationLLMDrivenDiscoveryWithRealKey(t *testing.T) {
	apiKey := os.Getenv("DEEPSEEK_API_KEY")
	if apiKey == "" {
		t.Skip("DEEPSEEK_API_KEY not set")
	}

	// Lua VM + DeepSeek Provider
	tmpVM := t.TempDir()
	vm := luaVM.NewVM(tmpVM + "/adapters")
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	defer vm.Stop()

	pm := agentAPI.NewProviderManager()
	pm.Register("deepseek", agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
		Model:   "deepseek-v4-flash",
		BaseURL: "https://api.deepseek.com",
		APIKey:  apiKey,
	}, vm, "deepseek", "deepseek"))

	// Setup — 加载所有真实内置插件
	env := setupIntegrationWithProvider(t, pm)
	defer env.cleanup()

	// 列出已加载的真实工具
	defs := env.stageHost.GetToolDefs()
	t.Logf("共有 %d 个已注册的真实工具:", len(defs))
	for _, d := range defs {
		t.Logf("  - %s", d.Name)
	}

	// 调用 healthcheck 进行全面检查（含 LLM 驱动阶段）
	result, err := env.stageHost.ExecuteTool("healthcheck", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}

	data, _ := json.Marshal(result)
	var resp map[string]interface{}
	json.Unmarshal(data, &resp)

	t.Logf("===== Full Healthcheck Result =====")
	t.Logf("status:  %v", resp["status"])
	t.Logf("summary: %v", resp["summary"])
	t.Logf("total:   %v", resp["total"])
	t.Logf("passed:  %v", resp["passed"])
	t.Logf("failed:  %v", resp["failed"])

	checks := resp["checks"].([]interface{})
	for _, c := range checks {
		cr := c.(map[string]interface{})
		prefix := "✅"
		if cr["status"] == "fail" {
			prefix = "❌"
		}
		t.Logf("  %s %s: %s  %s", prefix, cr["name"], cr["status"], cr["detail"])
	}

	if resp["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", resp["status"])
	}

	// 验证 LLM 发现阶段的存在
	foundDiscovery := false
	for _, c := range checks {
		cr := c.(map[string]interface{})
		if cr["name"] == "llm_discovery" {
			foundDiscovery = true
			if cr["status"] != "ok" {
				t.Fatalf("LLM discovery failed: status=%s detail=%s", cr["status"], cr["detail"])
			}
			break
		}
	}
	if !foundDiscovery {
		t.Fatal("expected llm_discovery check in results")
	}

	t.Logf("✅ LLM-driven tool discovery test PASSED")
}
