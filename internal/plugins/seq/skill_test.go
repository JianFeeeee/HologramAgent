package seq

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

// 编译期断言：seq 必须实现 plugin.SkillProvider，
// 否则内核的 InstallBuiltinSkill 会因类型断言失败而静默跳过。
var _ plugin.SkillProvider = (*Plugin)(nil)

func TestSkillProviderContract(t *testing.T) {
	p := New("seq")
	skills := p.BuiltinSkills()
	if len(skills) == 0 {
		t.Fatal("BuiltinSkills 返回空 —— model 永远看不到 seq")
	}
	body, ok := skills[builtinSkillName]
	if !ok {
		t.Fatalf("缺 %q 条目", builtinSkillName)
	}
	if len(body) < 200 {
		t.Fatalf("SKILL 正文过短（%d 字节），不足以让模型知道怎么用", len(body))
	}
}
