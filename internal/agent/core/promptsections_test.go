package core

// 判据：**插件声明的系统提示词段对所有 agent 可见**（2026-11-04）。
//
// ## 这组判据守的是哪类缺陷
//
// 生产故障：A2A 分诊助手（驻留子）拿 `terminal_create` 当一次性命令执行器，
// 每条 `cd … && echo … && grep …` 开一个新终端，命令结束终端就退出，
// `terminal_read` 立刻撞 `read /dev/ptmx: input/output error`。
//
// 链路拆开是四层：
//
//	① cmd 插件 auto_start=false ⇒ 工具表里没有 cmd_run
//	② 需要 cmd-tools skill 告诉它"先 plugin_start cmd"
//	③ 但 skill 索引**没进那个 agent 的提示词**
//	④ 因为 skillIndex 只是根 agent 的字段，由 bootstrap.go 手工接线
//
// 本文件钉 ③ 与 ④：**声明表挂在统一挂载点上，任何 agent 读同一份**。
//
// ## 为什么判据必须测"子 agent"而不是只测根
//
// 只测根 agent 会全绿 —— 那正是它当初上线时的状态。缺陷恰好在"子 agent
// 拿不到"这一层，所以判据必须**逐个类型**验证：rtchild / 驻留子 / 工作式子。
//
// ## 为什么测生成的 prompt 而不是测字段
//
// 断言 `carrier.promptSections != nil` 是结构判据：字段接上了，但
// renderSystemPrompt 里读取那段被删掉照样绿。所以直接看渲染结果。

import (
	"strings"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/promptsec"
)

// mountWithSection 装一个带指定段的挂载点，返回清理函数。
//
// ★ 用 MountForTest 而不是 Mount：并行测试会互相覆盖挂载点，
// 表现为随机红（本仓已有教训：判据在 -race 下才暴露）。
func mountWithSection(t *testing.T, section, content string) {
	t.Helper()
	reg := promptsec.NewRegistry()
	if err := reg.Declare(promptsec.Section{
		Section: section,
		Owner:   "skillmgr",
		Order:   100,
		Content: content,
	}); err != nil {
		t.Fatalf("声明段失败: %v", err)
	}
	restore := kstate.MountForTest(&kstate.State{PromptSections: reg})
	t.Cleanup(restore)
}

// rtHostWithIO 造一个父 agent + 成对 rt 通道（不带 promptSections 字段）。
func rtHostWithIO(t *testing.T) *Agent {
	t.Helper()
	pio := agentIO.NewIOManager()
	host := New(AgentConfig{ID: "ps-host", IO: pio, Provider: &rtBlockingProvider{}})
	if err := pio.RegisterDevice(&rtRecordingDevice{name: "psout"}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{"psout", "psin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "pstest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
	return host
}

// TestRTChildSeesPromptSections 钉住 rtchild 能看到插件声明的段。
//
// 变红条件：renderSystemPrompt 改回读 `a.promptSections` 字段（子没有）⇒ 红。
func TestRTChildSeesPromptSections(t *testing.T) {
	const marker = "TERMINAL-SKILL-MARKER: always read the terminal skill first"
	mountWithSection(t, "skillmgr.skills", marker)

	host := rtHostWithIO(t)
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{ID: "ps-rt", Input: "psin", Output: "psout"})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	prompt := host.rtChildOf(id).agent.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "TERMINAL-SKILL-MARKER") {
		t.Fatalf("★ rtchild 的提示词里没有插件声明的段 ——\n"+
			"它看不到任何 skill 说明，只能凭工具名猜用法（%d 字符）", len(prompt))
	}
}

// TestResidentSeesPromptSections 钉住驻留子能看到（**生产实际中招的那一类**）。
//
// A2A 的分诊助手就是驻留子。这条变红 = 故障复现。
func TestResidentSeesPromptSections(t *testing.T) {
	const marker = "CMD-SKILL-MARKER: run plugin_start cmd before cmd_run"
	mountWithSection(t, "skillmgr.skills", marker)

	parent, _, dir := newRootWith(t, &countingProvider{})
	spawnTestResident(t, parent, dir, "ps-res")

	rc := parent.residentOf("ps-res")
	if rc == nil {
		t.Fatal("驻留子没创建成功")
	}
	prompt := rc.agent.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "CMD-SKILL-MARKER") {
		t.Fatalf("★ 驻留子的提示词里没有插件声明的段 ——\n"+
			"这正是生产上'拿 terminal_create 当 cmd_run 用'的根因（%d 字符）", len(prompt))
	}
}

// TestRootAgentSeesPromptSections 确认根 agent 路径没被改坏（保守侧）。
func TestRootAgentSeesPromptSections(t *testing.T) {
	const marker = "ROOT-MARKER: skills are declared, not pulled"
	mountWithSection(t, "skillmgr.skills", marker)

	a := New(AgentConfig{ID: "ps-root"})
	if got := a.buildSystemPrompt("", "hi"); !strings.Contains(got, "ROOT-MARKER") {
		t.Fatal("根 agent 看不到插件声明的段 —— 本次改造改坏了原有路径")
	}
}

// TestNoPromptSectionsMount 钉住"没装挂载点时不能 panic、也不能凭空出现段"。
//
// 单测与独立工具（memgc 等）不装挂载点，这是常态而非异常。
func TestNoPromptSectionsMount(t *testing.T) {
	restore := kstate.MountForTest(nil)
	defer restore()

	a := New(AgentConfig{ID: "ps-none"})
	prompt := a.buildSystemPrompt("", "hi")
	if prompt == "" {
		t.Fatal("无挂载点时提示词为空 —— 基础人格丢了")
	}
	if strings.Contains(prompt, "[Skills]") {
		t.Fatal("没装注册表，提示词里却出现了 [Skills] 段")
	}
}

// TestDeclareTwiceRendersOnce 钉住**幂等**（回应用户"会不会多份"的担心）。
//
// 这条不是理论风险：system prompt 每轮重建，而一次任务里 stepPrepare 可能
// 被重入（上下文裁剪后回到该步）。若声明是"追加"而不是"覆盖"，
// 索引会越堆越多，最终挤掉真正的对话历史。
//
// 变红条件：promptsec.Registry 用 slice 存段（追加语义）⇒ 出现两份。
func TestDeclareTwiceRendersOnce(t *testing.T) {
	reg := promptsec.NewRegistry()
	for i := 0; i < 5; i++ { // 反复声明同一段 5 次
		if err := reg.Declare(promptsec.Section{
			Section: "skillmgr.skills",
			Owner:   "skillmgr",
			Order:   100,
			Content: "IDEMPOTENT-MARKER",
		}); err != nil {
			t.Fatalf("declare: %v", err)
		}
	}
	restore := kstate.MountForTest(&kstate.State{PromptSections: reg})
	defer restore()

	prompt := New(AgentConfig{ID: "ps-idem"}).buildSystemPrompt("", "hi")
	if n := strings.Count(prompt, "IDEMPOTENT-MARKER"); n != 1 {
		t.Fatalf("★ 同一段声明 5 次后渲染出 %d 份（应为 1）——\n"+
			"注册表必须用**覆盖**语义，否则每轮重建都会累积", n)
	}
}

// TestSectionRemovalTakesEffect 钉住"撤销后段消失"（插件 Stop 的路径）。
//
// 不撤销的后果：插件已停用，提示词里还留着"用 skill_info 读某 skill"，
// 模型于是去调不存在的工具 —— 比没有说明更糟。
func TestSectionRemovalTakesEffect(t *testing.T) {
	reg := promptsec.NewRegistry()
	if err := reg.Declare(promptsec.Section{
		Section: "skillmgr.skills", Owner: "skillmgr",
		Order: 100, Content: "REMOVABLE-MARKER",
	}); err != nil {
		t.Fatal(err)
	}
	restore := kstate.MountForTest(&kstate.State{PromptSections: reg})
	defer restore()

	a := New(AgentConfig{ID: "ps-rm"})
	if !strings.Contains(a.buildSystemPrompt("", "hi"), "REMOVABLE-MARKER") {
		t.Fatal("声明后应能看到")
	}

	reg.Remove("skillmgr.skills")
	if strings.Contains(a.buildSystemPrompt("", "hi"), "REMOVABLE-MARKER") {
		t.Fatal("撤销后段仍在 —— 插件 Stop 后模型会去调已不存在的工具")
	}
}

// TestRuntimeUpdateVisibleToExistingAgent 钉住"可运行时更新"（用户裁定）。
//
// 关键：**同一个 agent 实例**在注册表更新后，下一轮重建就应看到新内容。
// 若 Agent 在构造期拷贝了注册表或其内容，这条会红。
func TestRuntimeUpdateVisibleToExistingAgent(t *testing.T) {
	reg := promptsec.NewRegistry()
	restore := kstate.MountForTest(&kstate.State{PromptSections: reg})
	defer restore()

	a := New(AgentConfig{ID: "ps-live"})

	// 初始无内容
	if strings.Contains(a.buildSystemPrompt("", "hi"), "v1-of-skill") {
		t.Fatal("初始不该有内容")
	}

	// 运行时声明
	if err := reg.Declare(promptsec.Section{
		Section: "skillmgr.skills", Owner: "skillmgr",
		Order: 100, Content: "v1-of-skill",
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.buildSystemPrompt("", "hi"), "v1-of-skill") {
		t.Fatal("运行时声明后，已存在的 agent 应立刻看到")
	}

	// 运行时更新（覆盖）
	if err := reg.Declare(promptsec.Section{
		Section: "skillmgr.skills", Owner: "skillmgr",
		Order: 100, Content: "v2-of-skill",
	}); err != nil {
		t.Fatal(err)
	}
	prompt := a.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "v2-of-skill") {
		t.Fatal("更新后应看到新内容")
	}
	if strings.Contains(prompt, "v1-of-skill") {
		t.Fatal("旧内容仍在 —— 声明必须是覆盖语义")
	}
}

// TestSectionOrderIsRespected 钉住排序（插件的 Order 生效，且稳定）。
func TestSectionOrderIsRespected(t *testing.T) {
	reg := promptsec.NewRegistry()
	if err := reg.Declare(promptsec.Section{
		Section: "skillmgr.late", Owner: "skillmgr", Order: 450, Content: "LATE-BLOCK",
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Declare(promptsec.Section{
		Section: "skillmgr.early", Owner: "skillmgr", Order: 100, Content: "EARLY-BLOCK",
	}); err != nil {
		t.Fatal(err)
	}
	restore := kstate.MountForTest(&kstate.State{PromptSections: reg})
	defer restore()

	prompt := New(AgentConfig{ID: "ps-order"}).buildSystemPrompt("", "hi")
	early := strings.Index(prompt, "EARLY-BLOCK")
	late := strings.Index(prompt, "LATE-BLOCK")
	if early < 0 || late < 0 {
		t.Fatalf("两段都应出现（early=%d late=%d）", early, late)
	}
	if early > late {
		t.Fatalf("★ Order 小的必须在前（early 在 %d，late 在 %d）", early, late)
	}
}
