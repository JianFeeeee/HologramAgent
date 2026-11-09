package test

// internal/plugin 的黑盒判据：公开内容校验与 SKILL 文件加载。
// 测试只通过 ValidateSKILLContent/LoadSKILL 及 Plugin 接口观察结果。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

func TestValidateSKILLContent(t *testing.T) {
	valid := "# Demo\n\n这是一个用于测试的技能。\n\n## greet\n向用户打招呼。\n- name: 用户名\n"
	if err := plugin.ValidateSKILLContent(valid); err != nil {
		t.Fatalf("合法 SKILL 被拒：%v", err)
	}
	for _, tc := range []struct{ name, content, want string }{
		{"empty", "", "empty"},
		{"no description", "# Only heading", "no description"},
		{"bad tool name", "正文\n\n## bad/name\n描述", "invalid tool name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := plugin.ValidateSKILLContent(tc.content); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误内容的报错不符：%v", err)
			}
		})
	}
}

func TestLoadSKILLDirectoryExposesMetadataAndTools(t *testing.T) {
	dir := t.TempDir()
	content := "---\nversion: 2.1\nauthor: test\nio_type: text\nio_route: chat\nio_output_caps: text,markdown\n---\n# Demo\n\n用于回归的技能。\n\n## greet\n向用户打招呼。\n- name: 用户名\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := plugin.LoadSKILL(dir)
	if err != nil {
		t.Fatalf("LoadSKILL: %v", err)
	}
	if p.Name() != filepath.Base(dir) || p.Version() != "2.1" || p.Description() != "用于回归的技能。" {
		t.Fatalf("公开元数据不符：name=%q version=%q description=%q", p.Name(), p.Version(), p.Description())
	}
	if p.PluginType() != plugin.PluginTypeSKILL || !p.Enabled() || p.RawContent() != content {
		t.Fatalf("公开属性不符：type=%q enabled=%v", p.PluginType(), p.Enabled())
	}
	if len(p.Tools()) != 1 || p.Tools()[0].Name != "greet" {
		t.Fatalf("工具定义不符：%+v", p.Tools())
	}
	io := p.IOConfig()
	if io == nil || io.Type != "text" || io.InputRoute != "chat" || io.OutputRoute != "chat" || len(io.OutputCaps) != 2 {
		t.Fatalf("IO 配置不符：%+v", io)
	}
}

func TestLoadSKILLMissingFileReturnsError(t *testing.T) {
	if _, err := plugin.LoadSKILL(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("不存在的 SKILL 路径却加载成功")
	}
}
