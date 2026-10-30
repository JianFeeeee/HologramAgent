//go:build linux || darwin

package plugins

// 端到端判据：**真实外部插件**（子进程）驱动 rtchild 全链路。
//
// ## 为什么这条判据必须存在（单测覆盖不到的那一段）
//
// `internal/agent/core/rtchild_*_test.go` 证明的是**进程内**逻辑：
// rtSlot 单槽、打断、流式、交接。但真实插件是**独立子进程**，
// 它到内核之间还隔着：SDK 声明 → proc 桥（stdio RPC + 共享内存）→
// 内核登记表 → 通道划拨 → 注入路由 → 输出回写。
//
// 这一段出问题的历史是有据可查的：
//   - `notify_parent` 回调漏接线（工具在、一调就"没有上级"）
//   - `routeInputByOwner` 只认 residents，rt 输入被兜底给主 agent
//   - 输出侧漏映射 `ChannelDef.Realtime → CapRT`（插件声明了 rt 却划不上）
//
// 三者都是"进程内单测绿、真实链路断"，所以必须有这条跨进程判据。
//
// ## 前置
//
// 插件需先用 `hmapdev build` 产出 .hmap 并解到某个目录。未提供时 **skip**
// 而非 fail（与 real_plugin_smoke_test.go 同口径：CI 上不强制先跑构建）。
//
//	RTCHILD_E2E_PLUGIN_BIN=/path/to/plugin.bin.linux.amd64 \
//	  go test ./internal/plugins/ -run TestExternalRTChild -v

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// e2eEchoProvider 是最小 provider：ChatStream 立刻回一段文本。
//
// 用它而不是真模型：本判据验证的是**链路**（通道登记→划拨→注入→路由→
// 输出回写），不是模型质量。真模型会让本判据依赖网络与配额，变成 flaky。
type e2eEchoProvider struct{}

func (p *e2eEchoProvider) Name() string          { return "e2e-echo" }
func (p *e2eEchoProvider) MaxContextTokens() int { return 8192 }
func (p *e2eEchoProvider) Chat(_ context.Context, _ *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	return &agentAPI.CompletionResponse{Content: "echo"}, nil
}
func (p *e2eEchoProvider) ChatStream(_ context.Context, _ *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	ch := make(chan agentAPI.StreamChunk, 2)
	ch <- agentAPI.StreamChunk{Content: "这是 rtchild 的实时回复"}
	ch <- agentAPI.StreamChunk{Done: true}
	close(ch)
	return ch, nil
}

// rtE2EPluginBin 取外部插件产物路径（环境变量或 hmapdev 的默认输出）。
func rtE2EPluginBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("RTCHILD_E2E_PLUGIN_BIN"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("RTCHILD_E2E_PLUGIN_BIN 指向的文件不可读：%v", err)
		}
		return p
	}
	// 默认位置：/tmp/rtvoice-e2e/rtvoice-e2e/dist 解出的产物。
	for _, cand := range []string{
		"/tmp/pkg-rt/plugin.bin.linux.amd64",
		"/tmp/rtvoice-e2e/rtvoice-e2e/build/plugin.bin",
	} {
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	t.Skip("未找到 rtvoice-e2e 插件产物；先 hmapdev build 并解包，" +
		"或用 RTCHILD_E2E_PLUGIN_BIN=<plugin.bin 路径> 指定")
	return ""
}

// installRTPlugin 把外部插件产物装进插件目录（复用 installRealPlugin 的落盘口径）。
func installRTPlugin(t *testing.T, plgDir, name, binPath string) {
	t.Helper()
	dst := filepath.Join(plgDir, name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("建插件目录: %v", err)
	}
	data, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dst, "plugin.bin"), data, 0o755); err != nil {
		t.Fatalf("写 plugin.bin: %v", err)
	}
	manifest := fmt.Sprintf(
		`{"name":%q,"name_zh":%q,"name_en":%q,"version":"0.1.0","entry":"plugin.so","auto_start":true}`,
		name, name, name)
	if err := os.WriteFile(filepath.Join(dst, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写 plugin.json: %v", err)
	}
}

// TestExternalRTChild_FullChain 是**跨进程全链路**判据：
//
//	外部插件声明 rt 通道
//	  → 内核登记表里两条通道都带 CapRT
//	  → SpawnRTChild 成对划拨成功
//	  → 插件注入 → 路由进 rtSlot → rtchild 跑一轮
//	  → rtchild 输出写进 outputch → 回到插件 handler
//
// 任何一环断开，本判据都会在对应断言上失败（而不是"超时说不清原因"）。
func TestExternalRTChild_FullChain(t *testing.T) {
	bin := rtE2EPluginBin(t)

	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRTPlugin(t, plgDir, "rtvoice-e2e", bin)
	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载外部插件: %v", err)
	}
	if env.pluginReg.Get("rtvoice-e2e") == nil {
		t.Fatal("rtvoice-e2e 未加载")
	}

	// ── ① 插件声明的 rt 通道必须真的带 CapRT ──
	//
	// 这一条专钉"输出侧漏映射"那个缺陷：输入侧在 ChannelRegistry.Register
	// 里映射，输出侧在 Registry.regOutput 里映射，两处代码分开。
	inCh := "rtvoice-e2e-in"
	outCh := "rtvoice-e2e"
	reg := env.iom.ChannelRegistry()

	inEntry, ok := reg.Lookup(inCh)
	if !ok {
		t.Fatalf("rt inputch %q 未登记（插件声明没到内核）", inCh)
	}
	if !inEntry.Capabilities.Supports(agentIO.CapRT) {
		t.Fatalf("rt inputch 缺 CapRT（capabilities=%v）—— Def.Realtime 未映射", inEntry.Capabilities)
	}

	if dev := env.iom.GetDevice(outCh); dev == nil {
		t.Fatalf("rt outputch %q 未注册成 device", outCh)
	} else if !dev.OutputCapabilities().Supports(agentIO.CapRT) {
		t.Fatalf("rt outputch 缺 CapRT（caps=%v）—— regOutput 漏了 Def.Realtime 映射",
			dev.OutputCapabilities())
	}

	// ── ② 成对划拨 ──
	// 内核侧的 Agent 由本测试自建（testPluginEnv 不含 Agent：它只装配插件面）。
	host := agentCore.New(agentCore.AgentConfig{
		ID:       "e2e-host",
		IO:       env.iom,
		Provider: &e2eEchoProvider{},
	})
	rtID, err := host.SpawnRTChild(agentCore.RTChildOptions{
		ID: "e2e-rt", Input: inCh, Output: outCh,
	})
	if err != nil {
		t.Fatalf("创建 rtchild 失败: %v", err)
	}
	t.Cleanup(func() { host.StopRTChildren() })
	for _, n := range []string{inCh, outCh} {
		ch, _ := reg.Lookup(n)
		if ch.Owner != rtID {
			t.Fatalf("通道 %s 的 Owner = %q，期望 %q —— 成对划拨未生效", n, ch.Owner, rtID)
		}
	}

	// ── ③ 插件注入 → rtchild 跑起来 ──
	//
	// 走真实 RPC：调插件工具 rtvoice_inject，它内部 InjectInterruptTextOpts。
	if _, err := env.stageHost.ExecuteTool("rtvoice_inject",
		map[string]interface{}{"text": "用户说：讲个短故事"}); err != nil {
		t.Fatalf("调插件注入工具失败: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var rounds uint64
	for time.Now().Before(deadline) {
		if st, ok := host.RTChildStats(rtID); ok && st.Rounds > 0 {
			rounds = st.Rounds
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rounds == 0 {
		t.Fatal("插件注入后 rtchild 从未跑过一轮 —— 输入没有到达 rtSlot" +
			"（检查 routeInputByOwner 的 rt 分支）")
	}
	t.Logf("rtchild 已跑 %d 轮", rounds)

	// ── ④ 输出回到插件 handler ──
	//
	// 把插件的观测面拉回来：它记录的 outputs 里必须有东西。
	var dump map[string]interface{}
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		res, err := env.stageHost.ExecuteTool("rtvoice_dump", map[string]interface{}{})
		if err != nil {
			t.Fatalf("调插件 dump 工具失败: %v", err)
		}
		if m, ok := res.(map[string]interface{}); ok {
			dump = m
			if n, _ := m["output_count"].(float64); n > 0 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if dump == nil {
		t.Fatal("dump 没有返回结构化结果")
	}
	if n, _ := dump["output_count"].(float64); n == 0 {
		t.Fatalf("rtchild 跑了 %d 轮，但插件 outputch 一片空白 —— "+
			"输出没有回到插件（检查 rtEmit 与 outputch 的接线）", rounds)
	}

	// 注入记录也要能回读（证明是**本插件**注入的，不是别的路径）。
	if inj, ok := dump["injections"].([]interface{}); !ok || len(inj) == 0 {
		t.Fatalf("插件没有记录到自己的注入：%v", dump["injections"])
	}

	t.Logf("跨进程全链路通过：rtchild 轮次=%d，插件收到输出=%v 条",
		rounds, dump["output_count"])

	// ── ⑤ 回收：交接 + 通道悬空 ──
	if _, err := host.CloseRTChild(rtID); err != nil {
		t.Fatalf("回收 rtchild 失败: %v", err)
	}
	for _, n := range []string{inCh, outCh} {
		ch, ok := reg.Lookup(n)
		if !ok {
			continue
		}
		if ch.Owner == "" {
			t.Fatalf("通道 %s 被归还为未分配 —— 应保持悬空（主 agent 无 rt 能力）", n)
		}
	}
	if strings.TrimSpace(fmt.Sprint(host.RTChildren())) != "[]" {
		t.Fatalf("回收后登记表仍有 %v", host.RTChildren())
	}
}
