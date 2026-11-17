package test

// 工具执行前**路径闸**的判据：按工具声明的 PathArgs 判沙箱 + 写前留档。
//
// ## 为什么这条判据必须有（2026-11-16）
//
// 安全模块的 `CheckPath` / `CheckNetwork` 在 2026-11-16 之前**零生产调用**：
// `agent.security.sandbox.enabled=true` 打开也拦不住任何东西，配置读进了
// Policy 就静止在那里。唯一真实生效的路径约束在 `files` 插件自己的
// `pathWithinSandbox` —— 每个插件各写一套，外部插件还根本拿不到。
//
// 现在内核在工具执行前按 `ToolDef.PathArgs` 统一判：
//
//	工具声明 PathArgs ⇒ 内核判沙箱（read/write 根分开）+ 写方向写前留档
//	不声明             ⇒ 不判（存量插件零改动）
//
// ## 入口与观察面
//
// 入口：`Agent.ExecuteTool(name, args)`（api.ToolHost 的实现，完整执行链）
// 观察：返回文本里是否含拒绝理由（拒绝是工具**结果**的一种，模型据此改路径）
//
// 不用"工具 handler 有没有被调用"作观察：那需要让 handler 自己记一个标志，
// 等于判据侵入被测代码；而"拒绝理由出现在结果里"正是模型与用户看到的那个面。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// pathGuardHarness 造一个 agent，注册一个声明了 PathArgs 的工具。
//
// sandbox 与 dataDir 都从入参注入（判据自己声明输入，不读内核默认值）。
func pathGuardHarness(t *testing.T, dataDir, toolName string, pathArgs []string,
	sandbox security.SandboxPolicy) *agent.Agent {
	return pathGuardHarnessDef(t, dataDir, sandbox, sdk.ToolDef{
		Name: toolName, PathArgs: pathArgs,
	})
}

// pathGuardHarnessDef 用完整 ToolDef 装配（网络闸需要 NetworkArgs 字段）。
func pathGuardHarnessDef(t *testing.T, dataDir string, sandbox security.SandboxPolicy,
	def sdk.ToolDef) *agent.Agent {

	t.Helper()

	rt := security.NewDisabledRuntime()
	rt.SetPolicy(security.Policy{Sandbox: sandbox})

	sh := agent.NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return "EXECUTED", nil }
	if err := sh.RegisterTool(def.Name, def, noop); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	a := agent.New(agent.AgentConfig{
		ID: "pathguard", StageHost: sh, Security: rt.AsGate(), DataDir: dataDir,
	})
	t.Cleanup(a.Stop)
	return a
}

// TestUndeclaredPathArgIsNotJudged：不声明 PathArgs 的工具**不受**任何路径限制。
//
// ★ 这是升级安全的底线：存量插件（含外部 plugin.bin）不改一行，行为必须
// 与升级前逐字一致。若内核去"猜哪个参数是路径"，这条会立刻破。
func TestUndeclaredPathArgIsNotJudged(t *testing.T) {
	allow := t.TempDir()
	a := pathGuardHarness(t, t.TempDir(), "fs_undeclared", nil, security.SandboxPolicy{
		Enabled:   true,
		ReadRoots: []string{allow},
	})
	out, _ := a.ExecuteTool("fs_undeclared", map[string]interface{}{
		"path": "/etc/passwd",
	})
	if !strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ 未声明 PathArgs 却被路径闸拦了（存量行为被改变）: %s", out)
	}
}

// TestReadOutsideRootIsRejected：声明的路径参数落在读根之外 ⇒ 拒绝执行。
func TestReadOutsideRootIsRejected(t *testing.T) {
	allow := t.TempDir()
	a := pathGuardHarness(t, t.TempDir(), "fs_read", []string{"path"}, security.SandboxPolicy{
		Enabled:   true,
		ReadRoots: []string{allow},
	})
	out, _ := a.ExecuteTool("fs_read", map[string]interface{}{
		"path": filepath.Join(t.TempDir(), "outside.txt"),
	})
	if strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ 读根之外的路径被放行了: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "sandbox") &&
		!strings.Contains(out, "not permitted") {
		t.Fatalf("★ 拒绝理由不可执行（模型无法据此改路径）: %s", out)
	}
}

// TestWriteOutsideRootIsRejected：写方向按 write_roots 判，与读根无关。
func TestWriteOutsideRootIsRejected(t *testing.T) {
	allowWrite := t.TempDir()
	a := pathGuardHarness(t, t.TempDir(), "fs_write", []string{"path"}, security.SandboxPolicy{
		Enabled:    true,
		WriteRoots: []string{allowWrite},
	})
	// 写根之外 ⇒ 拒
	out, _ := a.ExecuteTool("fs_write", map[string]interface{}{
		"path": filepath.Join(t.TempDir(), "nope.txt"),
	})
	if strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ 写根之外的路径被放行了: %s", out)
	}
	// 写根之内 ⇒ 放行
	out, _ = a.ExecuteTool("fs_write", map[string]interface{}{
		"path": filepath.Join(allowWrite, "ok.txt"),
	})
	if !strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ 写根之内的路径被拒了: %s", out)
	}
}

// TestWriteArchivesProtectedPathBeforeExecution：受保护路径的**写**会先留档。
//
// ★ 留档必须发生在执行**之前**：执行后原文已被覆盖，那时再留档就晚了。
// 判据用"留档文件此刻已存在"作观察 —— 它是 guard 的 L1 还原唯一数据源。
func TestWriteArchivesProtectedPathBeforeExecution(t *testing.T) {
	dataDir := t.TempDir()
	protectedDir := t.TempDir()
	security.SetProtectedPaths([]string{protectedDir + string(os.PathSeparator)})
	t.Cleanup(func() { security.SetProtectedPaths(nil) })

	target := filepath.Join(protectedDir, "app.conf")
	const original = "ORIGINAL"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	a := pathGuardHarness(t, dataDir, "fs_write", []string{"path"}, security.SandboxPolicy{
		Enabled:    true,
		WriteRoots: []string{protectedDir},
	})

	out, _ := a.ExecuteTool("fs_write", map[string]interface{}{"path": target})
	if !strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ 受保护路径的写入被拒（应放行但留档）: %s", out)
	}

	rel := strings.TrimPrefix(target, "/")
	baseline := filepath.Join(security.FileBaselineDir(dataDir), rel)
	got, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatalf("★ 受保护路径写入前没有留档（guard 将无基线可还原）: %v", err)
	}
	if string(got) != original {
		t.Fatalf("★ 留档内容不是原文: %q", got)
	}
}

// TestReadDoesNotArchive：读方向不留档（只读不改原文，留档等于白写磁盘）。
func TestReadDoesNotArchive(t *testing.T) {
	dataDir := t.TempDir()
	protectedDir := t.TempDir()
	security.SetProtectedPaths([]string{protectedDir + string(os.PathSeparator)})
	t.Cleanup(func() { security.SetProtectedPaths(nil) })

	target := filepath.Join(protectedDir, "readable.conf")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := pathGuardHarness(t, dataDir, "fs_read", []string{"path"}, security.SandboxPolicy{
		Enabled:   true,
		ReadRoots: []string{protectedDir},
	})

	if _, err := a.ExecuteTool("fs_read", map[string]interface{}{"path": target}); err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	rel := strings.TrimPrefix(target, "/")
	if _, err := os.Stat(filepath.Join(security.FileBaselineDir(dataDir), rel)); err == nil {
		t.Fatal("★ 读操作却产生了留档")
	}
}

// TestNetworkArgRejectedWhenNetworkDisabled：声明了 NetworkArgs 的工具在
// `allow_network=false` 时必须被拒。
//
// ★ 与路径闸对称：`Policy.CheckNetwork` 在 2026-11-16 之前同样**零生产调用**
// —— `allow_network=false` 配置进去也不生效。这条判据钉的就是它真生效了。
func TestNetworkArgRejectedWhenNetworkDisabled(t *testing.T) {
	a := pathGuardHarnessDef(t, t.TempDir(), security.SandboxPolicy{
		Enabled:      true,
		AllowNetwork: false,
	}, sdk.ToolDef{Name: "web_fetch", NetworkArgs: []string{"url"}})

	out, _ := a.ExecuteTool("web_fetch", map[string]interface{}{"url": "https://example.com"})
	if strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ allow_network=false 却放行了网络工具: %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "network") {
		t.Fatalf("★ 拒绝理由没说清是网络策略（模型无法理解）: %s", out)
	}
}

// TestNetworkAllowedWhenEnabled：sandbox 未启用网络限制时照常执行。
func TestNetworkAllowedWhenEnabled(t *testing.T) {
	a := pathGuardHarnessDef(t, t.TempDir(), security.SandboxPolicy{
		Enabled:      true,
		AllowNetwork: true,
	}, sdk.ToolDef{Name: "web_fetch2", NetworkArgs: []string{"url"}})
	out, _ := a.ExecuteTool("web_fetch2", map[string]interface{}{"url": "https://example.com"})
	if !strings.Contains(out, "EXECUTED") {
		t.Fatalf("★ allow_network=true 却拦了网络工具: %s", out)
	}
}
