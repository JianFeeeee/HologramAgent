//go:build linux || darwin

package plugins

// 端到端判据：已发布的 rtchild 修复在**真实外部插件 + 真 RPC** 下成立。
//
// ## 为什么这一组必须单独存在（与 FullChain 的分工）
//
// `TestExternalRTChild_FullChain` 证明"全链路能跑通"——登记→划拨→注入→
// 输出回写→回收悬空。但它**一次都不测同名复用**，也不测批量停止：
// 它用的是全新的 id、且只在末尾 Cleanup 里 StopRTChildren。
//
// 而 2026-10-30 修的三处恰好全在"复用/清理"这条线上：
//
//	① RTTranscript 的读序（留档遮蔽在跑实例 ⇒ 两通电话串线）
//	② StopRTChildren 漏注销入站登记（child/rt-<id> 单调累积）
//	③ 留档跨代覆写语义
//
// 三者都是**跨通话生命周期**的缺陷 —— 单实例 e2e 天然测不到。这一组用
// 同一个插件产物、同一个通道对，反复"拨号—挂断"，把清理路径跑成主路径。
//
// ## 前置（与 FullChain 同口径）
//
//	RTCHILD_E2E_PLUGIN_BIN=/path/to/plugin.bin.linux.amd64 \
//	  go test ./internal/plugins/ -run TestExternalRTChild_Reuse -v
//
// 未提供产物时 **skip** 而非 fail（CI 不强制先构建插件）。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
)

// rtE2EHost 装配一个"插件面 + 内核 Agent"的现场，供复用类判据共用。
//
// 与 FullChain 内联装配的唯一差别是抽出来复用：三条判据各要一份干净现场，
// 内联复制会让"为什么这三条要各自 spawn 一套"变得不可读。
func rtE2EHost(t *testing.T, bin string) (*testPluginEnv, *agentCore.Agent, string, string) {
	t.Helper()
	env := setupIntegration(t)

	plgDir := env.tmpDir + "/plugins"
	installRTPlugin(t, plgDir, "rtvoice-e2e", bin)
	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载外部插件: %v", err)
	}
	if env.pluginReg.Get("rtvoice-e2e") == nil {
		t.Fatal("rtvoice-e2e 未加载")
	}

	host := agentCore.New(agentCore.AgentConfig{
		ID:       "reuse-host",
		IO:       env.iom,
		Provider: &e2eEchoProvider{},
	})
	return env, host, "rtvoice-e2e-in", "rtvoice-e2e"
}

// TestExternalRTChild_ReuseDoesNotLeakTranscript 钉住①在**真实链路**下成立：
// 同名 rtchild 回收后重建，新建实例的 transcript 不得含上一通的内容；
// 同时上一通的留档（在覆写之前）必须仍然可取 —— 那是交接契约。
//
// 变异方式：RTTranscript 改回 archive-first ⇒ 第二条断言必红（进程内判据
// 已实测；本判据保证同样的错误在跨进程装配下也拦得住）。
func TestExternalRTChild_ReuseDoesNotLeakTranscript(t *testing.T) {
	bin := rtE2EPluginBin(t)
	env, host, inCh, outCh := rtE2EHost(t, bin)
	defer env.cleanup()
	defer host.StopRTChildren()

	const id = "e2e-reuse"

	// ── 第一通：注入 → 跑一轮 → 回收 ──
	if _, err := host.SpawnRTChild(agentCore.RTChildOptions{ID: id, Input: inCh, Output: outCh}); err != nil {
		t.Fatalf("第一通创建失败: %v", err)
	}
	if _, err := env.stageHost.ExecuteTool("rtvoice_inject",
		map[string]interface{}{"text": "第一通说的话"}); err != nil {
		t.Fatalf("第一通注入失败: %v", err)
	}
	waitHistory(t, host, id, 2, "第一通（user+assistant）")

	firstMsgs, ok := host.RTTranscript(id)
	if !ok || len(firstMsgs) == 0 {
		t.Fatalf("第一通 transcript 为空（ok=%v len=%d）", ok, len(firstMsgs))
	}
	firstText := agentCore.FormatRTTranscript(firstMsgs)
	if n, err := host.CloseRTChild(id); err != nil || n == 0 {
		t.Fatalf("第一通回收失败（n=%d err=%v）", n, err)
	}

	// ★ 回收后留档必须仍可取 —— 这是「推信号 + 拉状态」的另一半。
	// 若这条红，说明有人为了"修串线"把留档删了（我本人曾写错过这一版）。
	archived, ok := host.RTTranscript(id)
	if !ok || len(archived) == 0 {
		t.Fatalf("★ 回收后取不到上一通原文 —— 交接契约断链（推了信号却拉不到正文）")
	}
	if !strings.Contains(agentCore.FormatRTTranscript(archived), "第一通说的话") {
		t.Fatalf("留档里没有第一通的内容：%s",
			truncateE2E(agentCore.FormatRTTranscript(archived)))
	}

	// ── 第二通：同名重建，必须"忘掉"上一通 ──
	if _, err := host.SpawnRTChild(agentCore.RTChildOptions{ID: id, Input: inCh, Output: outCh}); err != nil {
		t.Fatalf("同名重建失败: %v", err)
	}
	if _, err := env.stageHost.ExecuteTool("rtvoice_inject",
		map[string]interface{}{"text": "第二通说的话"}); err != nil {
		t.Fatalf("第二通注入失败: %v", err)
	}
	waitHistory(t, host, id, 2, "第二通（user+assistant）")

	second, ok := host.RTTranscript(id)
	if !ok {
		t.Fatal("第二通取不到 transcript")
	}
	secondText := agentCore.FormatRTTranscript(second)
	if strings.Contains(secondText, "第一通说的话") {
		t.Fatalf("★ 同名重建后 transcript 含**第一通**的内容 —— 两通电话串线：%s",
			truncateE2E(secondText))
	}
	if !strings.Contains(secondText, "第二通说的话") {
		t.Fatalf("第二通 transcript 里没有它自己的输入：%s", truncateE2E(secondText))
	}
	_ = firstText
}

// TestExternalRTChild_BulkStopUnregistersInbound 钉住②在真实链路下成立：
// 批量停止必须把 child/rt-<id> 从共享登记表里摘掉。
//
// 为什么要在跨进程层测：入站登记是**内核自己建的**，但登记表由插件
// 装配共用。进程内判据用的是裸 IOManager；这里用的是真实插件装好的那张表
// ——"在真实装配下也摘得干净"才算数。
func TestExternalRTChild_BulkStopUnregistersInbound(t *testing.T) {
	bin := rtE2EPluginBin(t)
	env, host, inCh, outCh := rtE2EHost(t, bin)
	defer env.cleanup()

	reg := env.iom.ChannelRegistry()

	const id = "e2e-bulk"
	if _, err := host.SpawnRTChild(agentCore.RTChildOptions{ID: id, Input: inCh, Output: outCh}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	// 入站登记的名字由内核约定：child/rt-<id>。
	// 这里不直接调用未导出的 inboundChannelName，而是按登记表现实探测：
	// 批量停止前后必须有**恰好一条** child/rt- 前缀的登记消失。
	const inboundPrefix = "child/rt/"
	countInbound := func() int {
		n := 0
		for _, ch := range reg.List() {
			if strings.HasPrefix(ch.Name, inboundPrefix) {
				n++
			}
		}
		return n
	}
	before := countInbound()
	if before == 0 {
		t.Fatalf("创建后没有任何 %q 前缀的入站登记 —— 判据前提不成立", inboundPrefix)
	}

	if n := host.StopRTChildren(); n != 1 {
		t.Fatalf("StopRTChildren 返回 %d，期望 1", n)
	}
	after := countInbound()
	if after != before-1 {
		t.Fatalf("★ 批量停止后入站登记 %d → %d（期望减少 1）—— "+
			"登记会随 create/destroy 次数单调累积，而外部无法单独注销",
			before, after)
	}

	// 插件声明的 rt 通道必须**悬空**而非消失（用户裁定：主 agent 无 rt 能力）。
	for _, n := range []string{inCh, outCh} {
		ch, ok := reg.Lookup(n)
		if !ok {
			t.Fatalf("批量停止把插件声明的 rt 通道 %q 也删了 —— 应保持悬空", n)
		}
		if ch.Owner == "" {
			t.Fatalf("rt 通道 %q 被归还为未分配（应悬空指向已销毁的 %q）", n, id)
		}
	}
}

// TestExternalRTChild_RepeatedCyclesDoNotAccumulate 是"长跑"形态的护栏：
// 连续多轮建—停，登记表与 rtChildren 都不得累积。
//
// 这条对应生产里的真实场景：模型被反复要求"打个电话问一句"。
// 单次回收正确、但多次之后残留堆积，是典型的"每次少清一点"型缺陷
// （②正是这种：单数路径对、复数路径漏）。
func TestExternalRTChild_RepeatedCyclesDoNotAccumulate(t *testing.T) {
	bin := rtE2EPluginBin(t)
	env, host, inCh, outCh := rtE2EHost(t, bin)
	defer env.cleanup()

	reg := env.iom.ChannelRegistry()
	countInbound := func() int {
		n := 0
		for _, ch := range reg.List() {
			if strings.HasPrefix(ch.Name, "child/rt/") {
				n++
			}
		}
		return n
	}

	const cycles = 3
	base := countInbound()
	for i := 0; i < cycles; i++ {
		id := fmt.Sprintf("cycle-%d", i)
		if _, err := host.SpawnRTChild(agentCore.RTChildOptions{
			ID: id, Input: inCh, Output: outCh,
		}); err != nil {
			t.Fatalf("第 %d 轮创建失败: %v", i, err)
		}
		if n := host.StopRTChildren(); n != 1 {
			t.Fatalf("第 %d 轮 StopRTChildren 返回 %d，期望 1", i, n)
		}
	}

	if got := countInbound(); got != base {
		t.Fatalf("★ %d 轮建—停后入站登记从 %d 涨到 %d —— 每轮都有残留", cycles, base, got)
	}
	if ids := host.RTChildren(); len(ids) != 0 {
		t.Fatalf("多轮建—停后 rtChildren 仍有 %v", ids)
	}
}

// ── 共用小工具 ──

// waitHistory 等到 rtchild 的 history 里出现 want 条消息（超时即失败）。
//
// ## 为什么等 history 而不等 Rounds（本仓已有专门教训）
//
// `rtRunOnce` 先 `rc.round++`（Rounds 立刻 +1），**之后**才流式生成、
// 最后才 `rtAppendHistory`。所以"Rounds 到了"不等于"话说完了、历史入账了"——
// 此时 RTTranscript 可能仍是空的。
//
// 这正是 dff745c「rt_children stop 判据改为等历史入账，不等 Rounds」的
// 同一个坑，我第一版又踩了一遍（首轮判据报"transcript 为空（len=0）"）。
// 等 history 才是可观测的正确条件。
func waitHistory(t *testing.T, host *agentCore.Agent, id string, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		if msgs, ok := host.RTTranscript(id); ok {
			if len(msgs) >= want {
				return
			}
			last = len(msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, ok := host.RTChildStats(id)
	t.Fatalf("%s：history 只到 %d 条（期望 ≥%d，ok=%v stats=%+v）—— "+
		"输入可能没到达 rtSlot，或话说完了但历史没入账", what, last, want, ok, st)
}

func truncateE2E(s string) string {
	r := []rune(s)
	if len(r) <= 240 {
		return s
	}
	return string(r[:240]) + "…"
}
