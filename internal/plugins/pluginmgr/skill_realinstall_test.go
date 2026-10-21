package pluginmgr

// ★★★ 真实包 → 安装 → skill 移交 的端到端判据。
//
// 用**真实的 commondev bundle**（含内嵌 node+pi + 随包 skill）跑一遍
// installFromData，验证 skill_detected 事件真的带着**可加载的路径**发出来。
//
// 这条把三段串起来：打包（hmapdev 的 skill/ 约定）→ 安装（解压保留路径）
// → 移交（pluginmgr 发事件）。任何一段断了，随包 skill 就到不了用户手上。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

func TestRealBundleHandsOutSkill(t *testing.T) {
	src := os.Getenv("HA_RAWFILE_E2E_HMAP")
	if src == "" {
		t.Skip("设 HA_RAWFILE_E2E_HMAP=<hmap路径> 以运行（需先构建 commondev bundle）")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读包失败: %v", err)
	}
	t.Logf("真实包 %.1f MiB", float64(len(data))/(1<<20))

	dir := t.TempDir()
	p, bus := newTestPluginWithBus(t, dir)
	var got []string
	bus.Subscribe(events.EventSkillDetected, func(e *events.Event) {
		pa, _ := e.Payload["path"].(string)
		got = append(got, pa)
	})

	res, err := p.installFromData(data, false)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if m, ok := res.(map[string]interface{}); ok {
		if e, bad := m["error"]; bad {
			t.Fatalf("安装报错: %v (%v)", e, m["details"])
		}
		if n, _ := m["skills_registered"].(int); n != 1 {
			t.Errorf("★ skills_registered 应为 1，实际 %v", m["skills_registered"])
		}
	}

	if len(got) != 1 {
		t.Fatalf("★ 应移交 1 个 skill，实际 %d: %v", len(got), got)
	}
	path := got[0]
	if !strings.HasSuffix(path, "commondev-discipline") {
		t.Errorf("移交的 skill 名不对: %s", path)
	}
	// ★ 关键：路径必须真的可加载（指向解压后的真实文件）
	md := filepath.Join(path, "SKILL.md")
	b, err := os.ReadFile(md)
	if err != nil {
		t.Fatalf("★ 移交路径不可读（skillmgr 会失败）: %v", err)
	}
	if len(b) < 1000 {
		t.Errorf("SKILL.md 内容过短（%d 字节），可能没打进包", len(b))
	}
	t.Logf("移交成功：%s（SKILL.md %d 字节）", path, len(b))
}
