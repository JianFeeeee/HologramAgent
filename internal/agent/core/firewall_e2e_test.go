package core

// 刀 S：**真机端到端**验证安全模块。
//
// 前面的判据都跑在 mock provider 上。这里用**真实的 Agent**（与生产同一套
// StageHost / IOManager / 设备出口），验证三件事真的连通：
//
//	① 出向：Agent 发往 provider 的消息里，明文已变成别名
//	② 落盘：变量库有记录，且磁盘上没有明文
//	③ 入向：插件工具收到的参数是**明文**（回填生效，插件无感知）
//	④ 入向：设备通道收到的 payload 是**明文**
//
// 为什么必须做真机验证：mock 判据只能证明"装饰器逻辑对"，
// 证明不了"装饰器挂在正确的点位上"。位置错（比如挂在 stage 之后）
// 在 mock 里完全看不出来。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/security"
)

const firewallToken = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// fwToolDef 构造一个最小工具声明（本文件专用，避开与既有测试的同名辅助冲突）。
func fwToolDef(name string) sdk.ToolDef {
	return sdk.ToolDef{
		Name:        name,
		Description: "安全模块测试工具",
		Parameters: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"token": map[string]interface{}{"type": "string"}},
		},
	}
}

func setupFirewall(t *testing.T) (*security.Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := security.InitGlobal(security.Options{
		Path:    filepath.Join(dir, "vars.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })
	return store, dir
}

// ★★ 判据：插件工具收到的参数是**明文**（回填生效）。
//
// 这是整个设计的核心目的：模型手上只有别名，插件拿到真凭据，
// 而**插件对替换完全无感知**。
func TestFirewall_ToolArgsFilledBeforePlugin(t *testing.T) {
	store, _ := setupFirewall(t)
	a, _, _ := newRootForResidents(t)

	// 插件工具：记录它实际收到的参数
	var seenToken string
	const toolName = "plugintest_store_secret"
	if err := a.stageHost.RegisterTool(toolName, fwToolDef(toolName),
		func(args map[string]interface{}) (interface{}, error) {
			seenToken, _ = args["token"].(string)
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}

	// 模型只知道别名（它从未见过明文）
	alias, err := store.Submit(firewallToken, "测试")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.stageHost.ExecuteTool(toolName, map[string]interface{}{
		"token": "@{" + alias + "}",
		"note":  "保持不变",
	}); err != nil {
		t.Fatal(err)
	}

	// ★ 插件看到的是明文
	if seenToken != firewallToken {
		t.Fatalf("插件应收到明文，实际收到 %q（回填未生效 ⇒ 插件拿不到真凭据）", seenToken)
	}
	t.Logf("✅ 插件工具收到的 token 是明文（%d 字节），别名 %s 已回填", len(seenToken), alias)
}

// ★★ 判据：设备通道收到的 payload 是**明文**。
//
// 这条验证回填插在了正确位置 —— 在插件 stage **之后**、设备 Execute **之前**。
// 若插错（比如 stage 之前），插件会看到明文；
// 若压根没插，设备会收到字面的 "@{sec_xxx}"。
func TestFirewall_DevicePayloadFilledBeforeExecute(t *testing.T) {
	store, _ := setupFirewall(t)
	a, _, _ := newRootForResidents(t)

	var delivered string
	const outCh = "plugintest_secch"
	if err := a.io.RegisterDevice(&probeDevice{
		name: outCh,
		caps: agentIO.CapText,
		handler: func(args map[string]interface{}) (interface{}, error) {
			return "已发送", nil
		},
		got: &delivered,
	}); err != nil {
		t.Fatal(err)
	}

	alias, err := store.Submit(firewallToken, "测试")
	if err != nil {
		t.Fatal(err)
	}

	// 模型发出的 payload 里只有别名
	got := a.executeOutputSendTool(agentAPI.ToolCall{
		Name: "output_send__" + outCh,
		Arguments: map[string]interface{}{
			"payload": "我的 token 是 @{" + alias + "} 请使用",
		},
	})

	// ★ 设备收到明文
	if !strings.Contains(delivered, firewallToken) {
		t.Fatalf("设备应收到明文，实际 %q（回填位置错或未生效）\n工具返回: %s",
			delivered, got)
	}
	if strings.Contains(delivered, alias) {
		t.Errorf("别名不应出现在设备载荷里: %q", delivered)
	}
	t.Logf("✅ 设备通道收到明文（%d 字节）", len(delivered))
}

// ★ 判据：出向拦截真的发生在真实 provider 调用上（用真实 ProviderManager）。
func TestFirewall_OutboundScrubbedOnRealProviderPath(t *testing.T) {
	setupFirewall(t)
	var captured string
	mgr := agentAPI.NewProviderManager()
	mgr.Register("probe", &fwCaptureProvider{name: "probe", capture: &captured})

	_, err := mgr.Default().Chat(context.Background(), &agentAPI.CompletionRequest{
		Messages: []agentAPI.Message{
			{Role: "user", Content: "这是我的凭据: " + firewallToken},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(captured, firewallToken) {
		t.Fatalf("明文泄漏到 provider: %s", captured)
	}
	if !strings.Contains(captured, "安全模块自动拦截") {
		t.Errorf("未见拦截提示: %s", captured)
	}
	t.Logf("✅ 出向拦截生效，provider 收到: %s", captured)
}

// ★ 判据：磁盘上**没有明文**（加密落盘），且权限 0600。
func TestFirewall_NoPlaintextOnDisk(t *testing.T) {
	store, dir := setupFirewall(t)
	if _, err := store.Submit(firewallToken, "落盘"); err != nil {
		t.Fatal(err)
	}

	// 扫变量库目录下所有文件
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), firewallToken) {
			t.Fatalf("磁盘文件 %s 出现明文", e.Name())
		}
		fi, _ := e.Info()
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("文件 %s 权限过宽: %04o", e.Name(), perm)
		}
	}
	t.Logf("✅ 目录下 %d 个文件均无明文，权限均受限", len(entries))
}

// ★ 判据：**同明文合并**（同一次对话里多个通道用同一个 secret ⇒ 同一别名）。
func TestFirewall_SameSecretMergesAcrossChannels(t *testing.T) {
	store, _ := setupFirewall(t)

	// 两处独立出现同一明文（模拟两个插件都用同一个 token）
	alias1, _ := store.Submit(firewallToken, "通道A")
	alias2, _ := store.Submit(firewallToken, "通道B")

	if alias1 != alias2 {
		t.Fatalf("同一明文必须合并到同一别名: %s vs %s", alias1, alias2)
	}
	if store.Count() != 1 {
		t.Errorf("应只有 1 条记录（合并），实际 %d", store.Count())
	}
	t.Logf("✅ 同一明文跨通道合并到 %s（记录数 1）", alias1)
}

// ★ 判据：端到端一轮 —— 出向后别名回填，模型可用同一别名再次调用。
//
// 这是完整闭环：拦截 → 别名进上下文 → 模型用别名 → 回填 → 插件拿到明文。
func TestFirewall_FullRoundTrip(t *testing.T) {
	store, _ := setupFirewall(t)
	a, _, _ := newRootForResidents(t)

	var pluginSaw string
	const toolName = "plugintest_roundtrip"
	if err := a.stageHost.RegisterTool(toolName, fwToolDef(toolName),
		func(args map[string]interface{}) (interface{}, error) {
			pluginSaw, _ = args["secret"].(string)
			return "ok", nil
		}); err != nil {
		t.Fatal(err)
	}

	// ① 第一步：模型输出的文本里带明文 ⇒ 被内核拦截成别名
	//    （模拟真实链路：模型回复 → 出向扫描）
	scrubbed, evts := store.Replace("用这个部署: "+firewallToken, "llm_out:assistant", security.Patterns)
	if len(evts) != 1 {
		t.Fatalf("应拦截 1 处，实际 %d", len(evts))
	}
	alias := evts[0].Alias
	t.Logf("① 拦截: 明文 → %s", alias)

	// ② 模型后续调用工具时只能引用别名（它没见过明文）
	if _, err := a.stageHost.ExecuteTool(toolName, map[string]interface{}{
		"secret": "@{" + alias + "}",
	}); err != nil {
		t.Fatal(err)
	}

	// ③ 插件拿到真凭据
	if pluginSaw != firewallToken {
		t.Fatalf("插件应拿到明文，实际 %q", pluginSaw)
	}

	// ④ 上下文中始终只有别名（模型的视野里无明文）
	if strings.Contains(scrubbed, firewallToken) {
		t.Fatal("上下文里出现明文")
	}
	// ⑤ 变量记录里也无明文
	b, _ := json.Marshal(store.List())
	if strings.Contains(string(b), firewallToken) {
		t.Fatal("变量记录泄露明文")
	}
	t.Logf("② 模型用 @{%s} 调用 → ③ 插件收到明文 → ④⑤ 上下文与记录均无明文", alias)
}

// fwCaptureProvider 记录收到的消息内容（模拟真实 provider 收到的请求）。
type fwCaptureProvider struct {
	name    string
	capture *string
}

func (c *fwCaptureProvider) Name() string { return c.name }
func (c *fwCaptureProvider) Chat(_ context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	if req != nil && len(req.Messages) > 0 {
		*c.capture = req.Messages[0].Content
	}
	return &agentAPI.CompletionResponse{}, nil
}
func (c *fwCaptureProvider) ChatStream(_ context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	if req != nil && len(req.Messages) > 0 {
		*c.capture = req.Messages[0].Content
	}
	ch := make(chan agentAPI.StreamChunk)
	close(ch)
	return ch, nil
}
func (c *fwCaptureProvider) MaxContextTokens() int { return 1000 }
