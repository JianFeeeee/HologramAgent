package test

// skillmgr 黑盒判据：技能目录输入 → SkillDescriptions 可观察文本。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/skillmgr"
	"github.com/JianFeeeee/HologramAgent/internal/promptsec"
	sdkpkg "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func TestSkillDescriptionsListsScannedSkillsInStableOrder(t *testing.T) {
	dir := t.TempDir()
	for name, desc := range map[string]string{"zeta": "处理 zeta 的工作流。", "alpha": "处理 alpha 的工作流。"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "# " + name + "\n\n" + desc + "\n"
		if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := skillmgr.New("contract-skillmgr", dir)
	// Start performs the public directory scan; provide only the two public
	// registrar callbacks required by the plugin SDK and no hidden test hooks.
	sdk := sdkpkg.New("contract-skillmgr", sdkpkg.SDKConfig{
		RegTool: func(string, sdkpkg.ToolDef, sdkpkg.ToolHandler) error { return nil },
	})
	if err := p.Start(sdk); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	got := p.SkillDescriptions()
	if !strings.Contains(got, "alpha: 处理 alpha 的工作流。") || !strings.Contains(got, "zeta: 处理 zeta 的工作流。") {
		t.Fatalf("scanned descriptions missing: %q", got)
	}
	if strings.Index(got, "alpha:") > strings.Index(got, "zeta:") {
		t.Fatalf("descriptions not in stable name order: %q", got)
	}
	if p.Name() != "contract-skillmgr" {
		t.Fatalf("Name=%q", p.Name())
	}
}

func TestSkillDescriptionsEmptyBeforeStart(t *testing.T) {
	p := skillmgr.New("empty", t.TempDir())
	if got := p.SkillDescriptions(); got != "" {
		t.Fatalf("unexpected descriptions: %q", got)
	}
}

// ---- 判据 3：索引声明（模型可见性）----

// TestSkillIndexDeclaredToPromptAndRevokedOnStop：Start 后技能索引必须进入
// 系统提示词注册表，Stop 后必须撤回。
//
// ★ 这条为什么必须存在：`publishIndex` 失败时只打一行 WARN，工具照常注册。
// 后果是"skill_list 能列出、模型却不知道" —— 最难看的一类降级，因为它
// 表现为模型行为异常而不是报错。
//
// 断言只看注册表里的可观察结果（Snapshot 有没有这段、正文有没有技能名），
// 不去读 publishIndex 的实现细节。
func TestSkillIndexDeclaredToPromptAndRevokedOnStop(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "处理 alpha 的工作流。")
	reg := promptsec.NewRegistry()

	p := skillmgr.New("skillmgr", dir)
	s := sdkpkg.New("skillmgr", sdkpkg.SDKConfig{
		RegTool:        func(string, sdkpkg.ToolDef, sdkpkg.ToolHandler) error { return nil },
		PromptSections: reg,
	})
	if err := p.Start(s); err != nil {
		t.Fatalf("Start: %v", err)
	}

	secs := reg.Snapshot()
	var content string
	for _, sec := range secs {
		if sec.Section == "skillmgr.skills" {
			content = sec.Content
		}
	}
	if content == "" {
		t.Fatalf("★ Start 后系统提示词里没有 skillmgr.skills —— 模型看不到任何技能索引（现有段: %v）", reg.Names())
	}
	if !strings.Contains(content, "alpha") {
		t.Fatalf("★ 索引里没有已加载的技能名，模型无法知道该读哪个: %q", content)
	}

	p.Stop()
	for _, name := range reg.Names() {
		if name == "skillmgr.skills" {
			t.Fatalf("★ Stop 后段仍在（热重载时旧索引会与新插件抢同一段名）: %v", reg.Names())
		}
	}
}

// 注：本批**不写**「零技能 ⇒ 不声明空段」判据。
//
// 实测：`skillsDir` 为空目录时并没有零技能 —— 内嵌的 kernel-memory /
// kernel-plugins 等 6 个 builtin skill 会被 materializeBuiltinSkills 落盘，
// 所以 buildIndex 一定非空。构造"零技能"只能传 skillsDir=""，
// 那是生产装配（factory 从 data_dir 推导）不可达的形态，
// 为它写判据就是为不存在的场景造断言。

// writeSkill 在 dir 下建一个标准 skill 目录。
func writeSkill(t *testing.T, dir, name, desc string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# " + name + "\n\n" + desc + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
