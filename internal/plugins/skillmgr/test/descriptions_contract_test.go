package test

// skillmgr 黑盒判据：技能目录输入 → SkillDescriptions 可观察文本。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugins/skillmgr"
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
