package core

// 驻留子：生命周期（N3）、跨 agent 投递（N4）、inputch 处理表（N5）、contextfull（N6）。
//
// 设计 docs/zh/resident-subagent-design.md §6/§7/§8/§9/§10。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// newRootWith 造一个带完整图记忆的父 agent，并指定它（以及它的驻留子）用的 provider。
func newRootWith(t *testing.T, provider agentAPI.Provider) (*Agent, *memory.GraphDB, string) {
	t.Helper()
	dir := t.TempDir()
	main, err := memory.NewGraphDB(filepath.Join(dir, "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	a := New(AgentConfig{
		ID:              "parent",
		Provider:        provider,
		ProviderManager: agentAPI.NewProviderManager(),
		IO:              agentIO.NewIOManager(),
		StageHost:       NewStageHost(),
		Memory:          main,
		DataDir:         dir,
	})
	a.Start() // 父也有自己的调度器：子的消息要真的进它的中断队列并被处理
	t.Cleanup(func() { a.Stop(); main.Close() })
	return a, main, dir
}

// newRootForResidents 是默认构造（正常窗口）。
func newRootForResidents(t *testing.T) (*Agent, *memory.GraphDB, string) {
	t.Helper()
	return newRootWith(t, &countingProvider{})
}

func spawnTestResident(t *testing.T, parent *Agent, dir, id string, inputChs ...string) ResidentInfo {
	t.Helper()
	info, err := parent.SpawnResident(ResidentOptions{
		ID:         id,
		TaskPrompt: "盯住这个通道，有情况就汇报",
		InputChs:   inputChs,
		TempPath:   filepath.Join(dir, "residents", id, "graph.db"),
	})
	if err != nil {
		t.Fatalf("创建驻留子失败: %v", err)
	}
	return info
}

// waitFor 轮询直到条件成立（测试里不使用 sleep 猜时序）。
func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	waitForWithin(t, what, 5*time.Second, fn)
}

func waitForWithin(t *testing.T, what string, within time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// ---- N3：生命周期 ----

func TestResident_LifecycleAndNoOrphans(t *testing.T) {
	parent, _, dir := newRootForResidents(t)

	// 父先注册两个 inputch（模拟插件注册），再把其中一个划给子。
	reg := parent.io.ChannelRegistry()
	if err := reg.Register(agentIO.InputChannel{Name: "qq", Plugin: "qq"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(agentIO.InputChannel{Name: "sub/in", Plugin: "sub"}); err != nil {
		t.Fatal(err)
	}

	info := spawnTestResident(t, parent, dir, "child-1", "sub/in")
	if info.State != "running" {
		t.Fatalf("新建的驻留子状态=%q", info.State)
	}
	if list := parent.Residents(); len(list) != 1 || list[0].ID != "child-1" {
		t.Fatalf("登记表=%+v", list)
	}

	// 划入生效：inputch 的归属变成子。
	ch, ok := reg.Lookup("sub/in")
	if !ok || ch.Owner != "child-1" {
		t.Fatalf("划入未生效：%+v", ch)
	}
	// 未划入的仍是未分配。
	if qq, _ := reg.Lookup("qq"); qq.Owner != "" {
		t.Fatalf("未划入的 inputch 不该有归属：%+v", qq)
	}
	// 父的入站 inputch（接收该子的消息）登记在父名下。
	if inbound, ok := reg.Lookup("child/child-1"); !ok || inbound.Owner != "parent" {
		t.Fatalf("父的入站 inputch 未登记：%+v ok=%v", inbound, ok)
	}

	// 子的轻量内核：有共同面、没有整理面。
	c1 := parent.residents["child-1"]
	if c1.agent.memory != nil {
		t.Fatal("驻留子不该有记忆整理面")
	}
	if c1.agent.graphMem() == nil {
		t.Fatal("驻留子必须有图记忆共同面")
	}
	// 子的 L4 只属于父。
	if !c1.agent.isKernelLevelSource("parent") {
		t.Fatal("父必须是子的内核级来源（子的 L4 归父独占）")
	}
	if c1.agent.isKernelLevelSource("别人") {
		t.Fatal("非父来源不得成为子的内核级来源")
	}

	// 销毁：出登记表、归还 inputch、temp 目录丢弃。
	if err := parent.DestroyResident("child-1"); err != nil {
		t.Fatal(err)
	}
	if len(parent.Residents()) != 0 {
		t.Fatal("销毁后登记表应为空")
	}
	if ch, _ := reg.Lookup("sub/in"); ch.Owner != "" {
		t.Fatalf("销毁后 inputch 应回到未分配：%+v", ch)
	}
	// 父的入站 inputch（child/<id>）必须在销毁时一并注销，否则登记表残留脏数据——
	// HologramAgent 实测：destroy 后 child/<id> 仍挂在根 agent 名下，且无工具可单独注销。
	if inbound, ok := reg.Lookup("child/child-1"); ok {
		t.Fatalf("销毁后父的入站 inputch 应被注销，实际残留：%+v", inbound)
	}
	if err := parent.DestroyResident("child-1"); err == nil {
		t.Fatal("重复销毁应报错")
	}

	// **父退出 ⇒ 全部子销毁、不留孤儿**。
	spawnTestResident(t, parent, dir, "c-a", "sub/in")
	spawnTestResident(t, parent, dir, "c-b")
	if n := parent.StopResidents(); n != 2 {
		t.Fatalf("StopResidents 销毁 %d 个，期望 2", n)
	}
	if len(parent.Residents()) != 0 {
		t.Fatal("父退出后登记表必须为空")
	}
	for _, id := range []string{"c-a", "c-b"} {
		if _, err := osStat(filepath.Join(dir, "residents", id)); err == nil {
			t.Fatalf("子 %s 的 temp 目录应被丢弃", id)
		}
		if _, ok := reg.Lookup(inboundChannelName(id)); ok {
			t.Fatalf("父退出后子 %s 的入站 inputch 应被注销", id)
		}
	}
}

// osStat 只是为了让"目录是否还存在"的断言可读（存在返回 nil 错误）。
func osStat(path string) (interface{}, error) {
	_, err := os.Stat(path)
	return nil, err
}

// ---- N4：跨 agent 投递 ----

func TestResident_DeliveryBothDirections(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	spawnTestResident(t, parent, dir, "child-1")
	child := parent.residents["child-1"].agent

	// 父 → 子：发送消息 ⇒ 子在 **L4** 上收到（父是子的内核级来源）。
	if err := parent.SendToResident("child-1", "先停一下，改做 X"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "子收到 L4 中断", func() bool {
		return child.DumpScheduler().Stats.InterruptsByLevel[LevelCritical] >= 1
	})

	// 子 → 父：主动消息 ⇒ 父在 **L3** 上收到（不是 L4）。
	// 注意 L3 的枚举值是 LevelInteractive（LevelMessage 是 L2）。
	child.notifyParent("我这边发现了点东西")
	waitFor(t, "父收到子的 L3 消息", func() bool {
		return parent.DumpScheduler().Stats.InterruptsByLevel[LevelInteractive] >= 1
	})
	if got := parent.DumpScheduler().Stats.InterruptsByLevel[LevelCritical]; got != 0 {
		t.Fatalf("子的主动消息不得以 L4 出现在父的阶梯上（实际 %d 次）", got)
	}
}

// ---- N5：inputch 处理表 ----

func TestResident_InputchTableAutoAndProactive(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	spawnTestResident(t, parent, dir, "child-1")
	child := parent.residents["child-1"].agent

	// 说明：create 会把任务提示词作为**第一条输入**投给子（"create 即开工"），
	// 所以这里先等那一轮写完 —— 表里每多一轮就多一条，正是"每轮必有记录"。
	waitFor(t, "任务提示词那一轮写入", func() bool {
		table, err := parent.ResidentTable("child-1")
		return err == nil && len(table) >= 1
	})
	base, err := parent.ResidentTable("child-1")
	if err != nil {
		t.Fatal(err)
	}
	n := len(base)

	// ① 子不主动写 ⇒ 系统自动写（每一轮必有记录）。
	child.io.InjectInput("sub/in", "text", map[string]interface{}{"content": "干活"})
	waitFor(t, "自动写处理表", func() bool {
		table, err := parent.ResidentTable("child-1")
		return err == nil && len(table) == n+1 && !table[n].Proactive
	})
	table, err := parent.ResidentTable("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if table[n].InputCh != "sub/in" {
		t.Fatalf("处理表应记本轮 inputch，实际 %q", table[n].InputCh)
	}

	// ② 子主动写 ⇒ 本轮不再自动写。
	child.recordInputchNote("本轮我自己记：已完成第一阶段")
	child.autoRecordInputch(&TaskFrame{Input: "第二轮"})
	table, err = parent.ResidentTable("child-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != n+2 || !table[n+1].Proactive || !strings.Contains(table[n+1].Text, "第一阶段") {
		t.Fatalf("主动写优先的语义不成立：%+v", table)
	}
}

// ---- N6：contextfull + 三种处置 ----

func TestResident_ContextFullAndDispositions(t *testing.T) {
	parent, main, dir := newRootForResidents(t)

	// ① contextfull：把子的**积累上下文**（a.context，不是拼好的消息）灌到超过窗口 90%。
	//    注意不能靠"拼好的消息很大"来触发：拼装前时间线已被 token 预算裁到 ~80% 窗口。
	spawnTestResident(t, parent, dir, "child-1")
	rc := parent.residents["child-1"]
	child := rc.agent
	child.context.Append(ContextEvent{
		Timestamp: time.Now(), Source: "sub/in",
		Input: strings.Repeat("上下文填充", 8000), // 40000 字 ≈ 80000 token ≫ 8192×0.9
	})
	child.io.InjectInput("sub/in", "text", map[string]interface{}{"content": "继续"})

	// 父在 **L4** 上收到 contextfull（带子标识）—— 只推信号。
	waitFor(t, "父收到 contextfull 的 L4 中断", func() bool {
		return parent.DumpScheduler().Stats.InterruptsByLevel[LevelCritical] >= 1
	})
	waitFor(t, "父的登记表显示子 contextfull", func() bool {
		for _, r := range parent.Residents() {
			if r.ID == "child-1" && r.ContextFull {
				return true
			}
		}
		return false
	})

	// ② 压缩（保留语义）：上下文变短 + 处理表清空 + 子继续存在。
	child.recordInputchNote("压缩前的记录")
	if _, err := parent.CompressResident("child-1"); err != nil {
		t.Fatal(err)
	}
	if len(parent.Residents()) != 1 {
		t.Fatal("压缩后子必须继续存在（压缩是保留语义）")
	}
	if table, _ := parent.ResidentTable("child-1"); len(table) != 0 {
		t.Fatalf("压缩必须清理 inputch 处理表，实际 %d 条", len(table))
	}

	// ③ 回收（取消语义）：父选中的 temp 记录合入 main，然后取消该子。
	if _, _, err := child.graphMem().Commit([]memory.Triple{
		{Subject: "子的发现", Relation: "指向", Object: "结论"},
	}, "sess", 1); err != nil {
		t.Fatalf("子写自己的 temp 应成功: %v", err)
	}
	if _, err := parent.ReclaimResident("child-1", reclaimKeepAll); err != nil {
		t.Fatal(err)
	}
	if len(parent.Residents()) != 0 {
		t.Fatal("回收是取消语义：子不该继续存在")
	}
	res, err := main.Recall([]string{"子的发现"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range res.Entities {
		if e.Name == "子的发现" {
			found = true
		}
	}
	if !found {
		t.Fatal("回收应把选中的 temp 记录合入主记忆")
	}

	// ④ 销毁：随时可做、立刻移除。
	spawnTestResident(t, parent, dir, "child-2")
	if err := parent.DestroyResident("child-2"); err != nil {
		t.Fatal(err)
	}
	if len(parent.Residents()) != 0 {
		t.Fatal("销毁后不该还在登记表里")
	}
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// ---- N7：端到端 + 压力 ----

func TestResident_E2EAndStress(t *testing.T) {
	parent, _, dir := newRootForResidents(t)
	reg := parent.io.ChannelRegistry()

	// 压力规模可用环境变量放大（默认 8 子 × 12 轮）：
	//   RESIDENT_STRESS_N / RESIDENT_STRESS_ROUNDS
	nResidents := envInt("RESIDENT_STRESS_N", 8)
	roundsEach := envInt("RESIDENT_STRESS_ROUNDS", 12)
	ids := make([]string, 0, nResidents)
	for i := 0; i < nResidents; i++ {
		id := "sub-" + string(rune('a'+i))
		ch := "sub/" + id + "/in"
		if err := reg.Register(agentIO.InputChannel{Name: ch, Plugin: "sub"}); err != nil {
			t.Fatal(err)
		}
		spawnTestResident(t, parent, dir, id, ch)
		ids = append(ids, id)
	}

	// 压力：每个子灌 roundsEach 轮输入；其中一半走父→子的 L4 消息，一半走普通输入。
	for _, id := range ids {
		for r := 0; r < roundsEach; r++ {
			// 内容必须唯一：内核会去重相同输入（去重路径不产生处理表记录）。
			msg := fmt.Sprintf("%s 第 %d 轮", id, r)
			if r%2 == 0 {
				if err := parent.SendToResident(id, msg); err != nil {
					t.Fatal(err)
				}
			} else {
				parent.residents[id].agent.io.InjectInput("sub/"+id+"/in", "text",
					map[string]interface{}{"content": msg})
			}
		}
	}

	// 全部子都必须活着，且每一轮都留下处理表记录（自动或主动）。
	waitForWithin(t, "全部子完成各自轮次", 30*time.Second, func() bool {
		for _, id := range ids {
			table, err := parent.ResidentTable(id)
			if err != nil || len(table) < roundsEach/2 {
				return false
			}
		}
		return true
	})

	// 双向通信在压力下也成立：让每个子都汇报一次（父侧 L3）。
	for _, id := range ids {
		parent.residents[id].agent.notifyParent("压力汇报 " + id)
	}
	waitForWithin(t, "父收到全部子的汇报", 10*time.Second, func() bool {
		return parent.DumpScheduler().Stats.InterruptsByLevel[LevelInteractive] >= uint64(nResidents)
	})

	// 父退出 ⇒ 全部子销毁、登记表清空（不留孤儿）。
	if n := parent.StopResidents(); n != nResidents {
		t.Fatalf("父退出应销毁 %d 个子，实际 %d", nResidents, n)
	}
	if len(parent.Residents()) != 0 {
		t.Fatal("父退出后登记表必须为空")
	}
	t.Logf("压力通过：%d 个驻留子 × %d 轮（父→子 L4 与普通输入各半）+ 双向汇报",
		nResidents, roundsEach)
}

// ---- 传统上下文：轻量内核不做动态上下文的裁剪 ----

// captureProvider 记录模型**实际收到**的消息。
//
// 为什么不直接看 TaskFrame：`prepareInputTask` 只做前半段（去重/通道/阶段/落上下文），
// 消息是在 `runTaskSteps` 的 stepPrepare 里才拼出来的；而且断言"模型看到了什么"
// 本来就比断言内核内部字段更接近事实。
type captureProvider struct {
	countingProvider
	mu       sync.Mutex
	calls    int
	messages []agentAPI.Message
}

func (p *captureProvider) Chat(ctx context.Context, req *agentAPI.CompletionRequest) (*agentAPI.CompletionResponse, error) {
	p.mu.Lock()
	p.calls++
	if req != nil {
		p.messages = append([]agentAPI.Message(nil), req.Messages...)
	}
	p.mu.Unlock()
	return &agentAPI.CompletionResponse{Content: "ok"}, nil
}

func (p *captureProvider) chatText() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b strings.Builder
	for _, m := range p.messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return p.calls, b.String()
}

// 子 agent 的上下文是**传统上下文**：累积的事件全部交给模型，
// 内核**不得**按动态上下文的预算静默丢弃（那是父 agent 的能力）。
// 装不下时由 contextfull 上报父决策，而不是自己丢。
func TestLightKernel_TraditionalContextNoTrimming(t *testing.T) {
	provider := &captureProvider{}
	parent, _, dir := newRootWith(t, provider)
	spawnTestResident(t, parent, dir, "child-1")
	child := parent.residents["child-1"].agent

	if !child.isLightKernel() {
		t.Fatal("驻留子必须被识别为轻量内核")
	}

	// 先等"create 即开工"那一轮（任务提示词）跑完，否则下面抓到的是它的请求，
	// 而不是我们注入了大段上下文之后的那一轮。
	waitFor(t, "任务提示词那一轮结束", func() bool {
		table, err := parent.ResidentTable("child-1")
		return err == nil && len(table) >= 1
	})
	// 前提：动态上下文的份额 < 窗口（否则测不出区别）。
	b := ComputeTokenBudget(child.provider, child.systemPrompt)
	if b.MaxContext <= b.ContextTokens {
		t.Fatalf("前提不成立：窗口(%d) 应大于动态上下文份额(%d)", b.MaxContext, b.ContextTokens)
	}
	// 填充量：**超过动态份额、但仍在窗口内**。
	// ⇒ 完整内核会因预算把最早那条裁掉；轻量内核不该裁（只受窗口硬上限约束）。
	filler := strings.Repeat("填", b.ContextTokens/2+200)
	if EstimateTokens(filler) <= b.ContextTokens {
		t.Fatalf("测试前提不成立：填充(%d token) 应超过动态份额(%d)", EstimateTokens(filler), b.ContextTokens)
	}
	child.context.Append(ContextEvent{Timestamp: time.Now(), Source: "sub/in", Input: "最早的事件标记EARLY"})
	child.context.Append(ContextEvent{Timestamp: time.Now(), Source: "sub/in", Input: filler})
	child.context.Append(ContextEvent{Timestamp: time.Now(), Source: "sub/in", Input: "最新的事件标记LATE"})

	child.io.InjectInput("sub/in", "text", map[string]interface{}{"content": "本轮输入"})

	// 等**新的一轮**请求（首轮可能已经发过，必须严格等到注入之后那次）。
	waitFor(t, "子发出新一轮 LLM 请求", func() bool {
		n, _ := provider.chatText()
		return n >= 2
	})
	_, got := provider.chatText()
	if !strings.Contains(got, "最早的事件标记EARLY") {
		t.Fatalf("传统上下文：更早的事件不得被预算裁掉（属于父 agent 的动态上下文能力）；实收消息长度=%d", len(got))
	}
	if !strings.Contains(got, "最新的事件标记LATE") {
		t.Fatal("最新事件必须在内")
	}
}

// 对照：完整内核（根 agent）仍走动态上下文（按预算裁时间线）。
func TestFullKernel_StillUsesDynamicContext(t *testing.T) {
	parent, _, _ := newRootForResidents(t)
	if parent.isLightKernel() {
		t.Fatal("根 agent 不是轻量内核")
	}
	b := ComputeTokenBudget(parent.provider, "sys")
	if got := parent.contextTokenBudget(b); got != b.ContextTokens {
		t.Fatalf("完整内核应使用动态上下文的 ContextTokens(%d)，实际 %d", b.ContextTokens, got)
	}
	if b.MaxContext <= b.ContextTokens {
		t.Fatalf("前提不成立：窗口(%d) 应大于动态上下文份额(%d)", b.MaxContext, b.ContextTokens)
	}
}
