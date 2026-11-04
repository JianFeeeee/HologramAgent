package core

// 判据：**子 agent 必须继承技能索引**（2026-11-04 生产缺陷）。
//
// ## 缺陷原貌
//
// `SetSkillIndexProvider` 只在 `cmd/homed/bootstrap.go` 里对**根 agent** 调了一次：
//
//	if sp := pluginReg.Get("skillmgr"); sp != nil {
//	    if prov, ok := sp.(agentCore.SkillIndexProvider); ok {
//	        agent.SetSkillIndexProvider(prov)
//	    }
//	}
//
// 而 rtchild / 驻留子 / 转投驻留子 各自 `New(AgentConfig{...})` 构造载体，
// 都**没有**传 `SkillIndexProvider`。于是子的 system prompt 里
// **完全没有 skill 索引**。
//
// ## 生产症状（真实日志）
//
// A2A 会话由「临时分诊助手」（驻留子）接手，它手上没有 `cmd_run`
// （`cmd` 插件 `auto_start=false`），而 `cmd-tools` skill 正文里写着
// 「先 plugin_start cmd」。它看不到这条 skill，于是把 `terminal_create`
// 当成命令执行器：每条 `cd … && echo … && grep …` 开一个新终端，
// 终端随命令结束而退出，`terminal_read` 立刻撞
// `read /dev/ptmx: input/output error`。
//
// ## 与 Security 那次同型
//
// 父有的能力，子没继承。`Security` 那次也是"只修了主 agent"，
// 后来补成"构造期落位 + 各子构造点显式传"。
//
// ## 判据为什么测 prompt 而不是测字段
//
// 断言 `carrier.skillIndex != nil` 是结构判据：字段接上了，但
// `buildSystemPrompt` 里注入那段被删了照样绿。所以直接看**生成的提示词**。

import (
	"strings"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// fakeSkillIndex 是最小 SkillIndexProvider 替身。
type fakeSkillIndex struct{ content string }

func (f *fakeSkillIndex) SkillIndex() string { return f.content }

// rtHostWithSkills 造一个带技能索引的父 agent + 成对 rt 通道。
func rtHostWithSkills(t *testing.T, idx SkillIndexProvider) *Agent {
	t.Helper()
	pio := agentIO.NewIOManager()
	host := New(AgentConfig{
		ID: "si-host", IO: pio, Provider: &rtBlockingProvider{},
		SkillIndexProvider: idx,
	})
	if err := pio.RegisterDevice(&rtRecordingDevice{name: "siout"}); err != nil {
		t.Fatalf("注册输出设备: %v", err)
	}
	for _, n := range []string{"siout", "siin"} {
		if err := pio.ChannelRegistry().Register(agentIO.InputChannel{
			Name: n, Plugin: "sitest",
			Capabilities: agentIO.CapText | agentIO.CapRT,
		}); err != nil {
			t.Fatalf("注册通道 %s: %v", n, err)
		}
	}
	return host
}

// TestRTChild_InheritsSkillIndex 钉住 rtchild 继承技能索引。
//
// 变红条件：从 SpawnRTChild 的 AgentConfig 删掉 SkillIndexProvider ⇒ 红。
func TestRTChild_InheritsSkillIndex(t *testing.T) {
	const marker = "kernel-terminal v1.0.0 - Terminal waits for input"
	host := rtHostWithSkills(t, &fakeSkillIndex{content: marker})
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{ID: "si-rt", Input: "siin", Output: "siout"})
	if err != nil {
		t.Fatalf("SpawnRTChild: %v", err)
	}
	carrier := host.rtChildOf(id).agent

	prompt := carrier.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "kernel-terminal") {
		t.Fatalf("★ rtchild 的 system prompt 里没有技能索引 —— "+
			"它看不到任何 skill，只能凭工具名猜用法（%d 字符的 prompt）", len(prompt))
	}
	if !strings.Contains(prompt, "/dev/ptmx") && !strings.Contains(prompt, "Terminal waits for input") {
		t.Fatalf("★ 索引内容没进 prompt（只匹配到了 skill 名？）")
	}
}

// TestResident_InheritsSkillIndex 钉住驻留子继承技能索引。
//
// 这是**生产实际中招的那一类**：A2A 的临时分诊助手就是驻留子。
//
// 变红条件：从 SpawnResident 的 AgentConfig 删掉 SkillIndexProvider ⇒ 红。
func TestResident_InheritsSkillIndex(t *testing.T) {
	const marker = "cmd-tools v1.0.0 - on-demand: plugin_start cmd first"
	parent, _, dir := newRootWith(t, &countingProvider{})
	parent.skillIndex = &fakeSkillIndex{content: marker}

	spawnTestResident(t, parent, dir, "si-res")
	rc := parent.residentOf("si-res")
	if rc == nil {
		t.Fatal("驻留子没创建成功")
	}
	prompt := rc.agent.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "cmd-tools") {
		t.Fatalf("★ 驻留子的 system prompt 里没有技能索引 —— "+
			"这正是生产上'拿 terminal_create 当 cmd_run 用'的根因"+
			"（%d 字符的 prompt）", len(prompt))
	}
}

// TestRootAgent_SkillIndexStillInjected 钉住根 agent 不受本次改动影响。
//
// 保守侧：确认修复没有把根 agent 的路径改坏。
func TestRootAgent_SkillIndexStillInjected(t *testing.T) {
	const marker = "kernel-memory v1.0.0 - read before memory tools"
	a := New(AgentConfig{ID: "root-si", SkillIndexProvider: &fakeSkillIndex{content: marker}})
	prompt := a.buildSystemPrompt("", "hi")
	if !strings.Contains(prompt, "kernel-memory") {
		t.Fatal("根 agent 的技能索引丢了 —— 本次改动改坏了原有路径")
	}
}

// TestChildWithoutSkillIndexIsNotBroken 钉住"父没有索引时子也不炸"。
//
// 父可能没接 skillmgr（单测、独立工具）。那种情况下子照常工作，
// 只是 prompt 里没有索引段 —— 不能因为 nil 就 panic。
func TestChildWithoutSkillIndexIsNotBroken(t *testing.T) {
	host := rtHostWithSkills(t, nil) // 显式不接
	defer host.StopRTChildren()

	id, err := host.SpawnRTChild(RTChildOptions{ID: "si-none", Input: "siin", Output: "siout"})
	if err != nil {
		t.Fatalf("父无技能索引时 SpawnRTChild 不该失败: %v", err)
	}
	prompt := host.rtChildOf(id).agent.buildSystemPrompt("", "hi")
	if strings.Contains(prompt, "[Skills]") {
		t.Fatal("父没有技能索引，子的 prompt 却出现了 [Skills] 段")
	}
}
