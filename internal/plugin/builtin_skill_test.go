package plugin

// 内置插件随包 SKILL 生命周期的判据。
//
// ## 为什么这些判据必须存在
//
// skill 没装上/没摘掉，**插件本身照常工作**（工具都在），只是模型的行为变了：
//   · 缺 skill ⇒ 模型看不到能力摘要，几乎不会用（"seq 很少被使用"的直接原因）
//   · 停用后 skill 还在 ⇒ 模型看得到却调不到，拿到一串"工具不存在"且不知原因
//
// 两种都是**静默失效**：没有任何报错，只能靠判据钉住。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// fakeSkillPlugin 是一个实现 SkillProvider 的最小插件替身。
type fakeSkillPlugin struct {
	name   string
	skills map[string]string
}

func (f *fakeSkillPlugin) Name() string                     { return f.name }
func (f *fakeSkillPlugin) Start(*sdk.PluginSDK) error       { return nil }
func (f *fakeSkillPlugin) Stop() error                      { return nil }
func (f *fakeSkillPlugin) BuiltinSkills() map[string]string { return f.skills }

// plainPlugin 不实现 SkillProvider —— 绝大多数插件是这样，必须不受影响。
type plainPlugin struct{ name string }

func (p *plainPlugin) Name() string               { return p.name }
func (p *plainPlugin) Start(*sdk.PluginSDK) error { return nil }
func (p *plainPlugin) Stop() error                { return nil }

// testDataDir 读回注入的 data_dir（同包测试可直接读私有字段）。
func (r *Registry) testDataDir() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dataDir
}

func newTestRegistryForSkills(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	r.SetDataDir(t.TempDir())
	r.SetEventBus(events.NewBus())
	return r
}

const sampleSkill = `---
name: demo-tools
description: 演示用 skill。
---

演示用 skill 的正文段落，模型会读到这一行作为摘要。
`

// ── 安装：写到约定位置（skillmgr 的扫描口径）──

func TestBuiltinSkill_InstallsToConventionPath(t *testing.T) {
	r := newTestRegistryForSkills(t)
	dataDir := r.testDataDir()

	p := &fakeSkillPlugin{name: "demo", skills: map[string]string{"demo-tools": sampleSkill}}
	r.InstallBuiltinSkill("demo", p)

	file := filepath.Join(dataDir, "skills", "demo-tools", "SKILL.md")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("SKILL.md 未落到约定位置 %s: %v（skillmgr 扫不到它）", file, err)
	}
	if string(data) != sampleSkill {
		t.Fatal("落盘内容与插件提供的不一致")
	}
}

// 不实现 SkillProvider 的插件必须被安静跳过（不能报错、不能创建目录）。
func TestBuiltinSkill_NonProviderIsNoop(t *testing.T) {
	r := newTestRegistryForSkills(t)
	r.InstallBuiltinSkill("plain", &plainPlugin{name: "plain"})

	skillsDir := filepath.Join(r.testDataDir(), "skills")
	if entries, err := os.ReadDir(skillsDir); err == nil && len(entries) > 0 {
		t.Fatalf("无 skill 的插件不应产生任何目录，实际 %d 项", len(entries))
	}
}

// 幂等：重复安装不报错，且不覆盖用户已改过的内容。
func TestBuiltinSkill_IdempotentNoOverwrite(t *testing.T) {
	r := newTestRegistryForSkills(t)
	dataDir := r.testDataDir()
	p := &fakeSkillPlugin{name: "demo", skills: map[string]string{"demo-tools": sampleSkill}}

	r.InstallBuiltinSkill("demo", p)
	file := filepath.Join(dataDir, "skills", "demo-tools", "SKILL.md")
	const edited = "# 用户改过的内容\n"
	if err := os.WriteFile(file, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}

	r.InstallBuiltinSkill("demo", p)
	got, _ := os.ReadFile(file)
	if string(got) != edited {
		t.Fatalf("重复安装覆盖了用户内容：期望 %q，实际 %q", edited, string(got))
	}
}

// 空正文必须跳过（空的 SKILL 会让模型读到无内容，等于装了个假 skill）。
func TestBuiltinSkill_EmptyBodySkipped(t *testing.T) {
	r := newTestRegistryForSkills(t)
	r.InstallBuiltinSkill("demo", &fakeSkillPlugin{
		name:   "demo",
		skills: map[string]string{"empty": "   \n  "},
	})
	dir := filepath.Join(r.testDataDir(), "skills", "empty")
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("空正文不应创建 skill 目录")
	}
}

// ── 卸载：停用时必须摘掉 ──

func TestBuiltinSkill_RemoveOnStop(t *testing.T) {
	r := newTestRegistryForSkills(t)
	dataDir := r.testDataDir()
	p := &fakeSkillPlugin{name: "demo", skills: map[string]string{"demo-tools": sampleSkill}}

	r.InstallBuiltinSkill("demo", p)
	dir := filepath.Join(dataDir, "skills", "demo-tools")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("前置：skill 应已安装: %v", err)
	}

	r.RemoveBuiltinSkill("demo", p)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("停用后 skill 目录必须移除（否则模型看得到却调不到工具）: err=%v", err)
	}
}

// ── 事件：必须主动通告（启动全扫有时序空窗）──

func TestBuiltinSkill_AnnouncesDetected(t *testing.T) {
	r := newTestRegistryForSkills(t)
	bus := events.NewBus()
	r.SetEventBus(bus)

	var got []*events.Event
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) { got = append(got, e) })

	r.InstallBuiltinSkill("demo", &fakeSkillPlugin{
		name:   "demo",
		skills: map[string]string{"demo-tools": sampleSkill},
	})

	if len(got) != 1 {
		t.Fatalf("安装必须通告一次 skill_detected，实际 %d 次", len(got))
	}
	if p, _ := got[0].Payload["path"].(string); !strings.Contains(p, "demo-tools") {
		t.Fatalf("payload.path 应指向 skill 目录，实际 %q", p)
	}
	if got[0].Source != "demo" {
		t.Fatalf("Source 应为插件名，实际 %q", got[0].Source)
	}
}

// 卸载时通告带 removed 标记：让 skillmgr 能从索引里摘掉。
func TestBuiltinSkill_AnnouncesRemoved(t *testing.T) {
	r := newTestRegistryForSkills(t)
	bus := events.NewBus()
	r.SetEventBus(bus)

	var got []*events.Event
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) { got = append(got, e) })

	p := &fakeSkillPlugin{name: "demo", skills: map[string]string{"demo-tools": sampleSkill}}
	r.InstallBuiltinSkill("demo", p)
	r.RemoveBuiltinSkill("demo", p)

	if len(got) != 2 {
		t.Fatalf("安装+卸载应各通告一次，实际 %d 次", len(got))
	}
	removed, _ := got[1].Payload["removed"].(bool)
	if !removed {
		t.Fatal("卸载通告必须带 removed=true（否则 skillmgr 分不清是新增还是移除）")
	}
}
