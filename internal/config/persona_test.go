package config

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 人格模板的契约：**不得写死版本号**。
//
// 来历：线上人格卡（personal/personal.md）曾写死「当前版本 v0.9.0（C ABI v2）」，
// 而 C ABI 早在 v1.0.0 就被删除。结果是内核自身日志/接口都报 1.2.0，
// agent 被问版本时却按人格卡自述旧版本（v1.2.0 压测发现）。
// 版本应来自运行时快照，不来自任何会被发版落下的文本。
func TestDefaultPersonaPromptHasNoVersionLiterals(t *testing.T) {
	re := regexp.MustCompile(`\bv?\d+\.\d+\.\d+\b`)
	if m := re.FindAllString(DefaultPersonaPrompt, -1); len(m) > 0 {
		t.Fatalf("默认人格模板含版本号字面量 %v —— 发版后必然腐坏，"+
			"被问版本时应要求 agent 读运行时快照", m)
	}
	// ★ 判据锚在**语义**而不是某个中文词：内核提示词已英文化
	//（中文提示会让模型行为漂移）。这里同时接受两种表述，
	// 但要求"版本/构建信息必须来自运行时快照"这个事实**明确出现**。
	if !strings.Contains(DefaultPersonaPrompt, "runtime snapshot") &&
		!strings.Contains(DefaultPersonaPrompt, "运行时快照") {
		t.Fatal("默认人格模板必须显式要求「版本以运行时快照为准」，否则模型会凭记忆编造版本")
	}
}

// 人格配置项必须注册、默认值就是 DefaultPersonaPrompt（单一事实源），
// 且全新安装时会被播种进 DB。
func TestPersonaPromptRegisteredWithDefault(t *testing.T) {
	dir := t.TempDir()
	r := NewConfigRegistry(filepath.Join(dir, "config.db"))
	r.SeedDefaults(dir)
	defer r.Close()

	def := r.GetDef("core.agent.personal_prompt")
	if def == nil {
		t.Fatal("core.agent.personal_prompt 未注册")
	}
	if def.Default != DefaultPersonaPrompt {
		t.Fatalf("默认值与 DefaultPersonaPrompt 不一致：%q", def.Default)
	}
	if def.Type != "text" {
		t.Fatalf("人格设定应为多行文本类型，实际 %q", def.Type)
	}
	if got := r.GetString("core.agent.personal_prompt", ""); got != DefaultPersonaPrompt {
		t.Fatalf("播种未写入默认人格（长度 %d）", len(got))
	}
}
