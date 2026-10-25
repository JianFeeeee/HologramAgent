package seq

import (
	"strings"
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

// ── 使用边界必须写清楚（B 方案的判据）──
//
// 决策背景：曾试图"让模型多用 seq"（加系统提示词块、加推销式措辞），
// e2e 实测证明那是错的方向 —— 对一次性的多文件读，模型并行调
// files_read 就是最优解，包成序列纯亏。模型不用它是对的。
//
// 所以文档改为**直说边界**：多数时候不该用，只在"要反复跑同一套流程"时才划算。
// 这条判据守的就是这个立场不被后续"优化"悄悄推回推销腔。
func TestSkillStatesNonUseCases(t *testing.T) {
	body := New("seq").BuiltinSkills()[builtinSkillName]

	// 必须明确写"多数时候不该用"这个立场
	for _, want := range []string{"多数时候不该用", "不划算", "只做一次"} {
		if !strings.Contains(body, want) {
			t.Fatalf("SKILL 必须写清不适用情形（缺 %q）——否则会重新变成推销", want)
		}
	}
	// 必须点出"并行调工具已经最优"这个真实反例（e2e 实测结论）
	if !strings.Contains(body, "并行") {
		t.Fatal("SKILL 应点明「几个工具可同批并行时，包进序列只是多一层壳」这个反例")
	}
	// 适用判据必须是「要反复跑」而不是「操作多」
	if !strings.Contains(body, "反复") {
		t.Fatal("SKILL 的核心判据应是「这套流程要反复跑」，不是「操作数量多」")
	}
}

// 索引行（模型每轮唯一能看到的一行）也必须带边界，不能只有卖点。
func TestSkillIndexLineStatesBoundary(t *testing.T) {
	body := New("seq").BuiltinSkills()[builtinSkillName]
	// 取正文首段（内核 extractDescription 的取值方式）
	lines := strings.Split(body, "\n")
	inFM, started := false, false
	desc := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if i == 0 && t == "---" {
			inFM = true
			continue
		}
		if inFM {
			if t == "---" {
				inFM, started = false, true
			}
			continue
		}
		if started && t != "" && !strings.HasPrefix(t, "#") {
			desc = t
			break
		}
	}
	if desc == "" {
		t.Fatal("取不到索引行（正文首段为空）")
	}
	if !strings.Contains(desc, "反复") || !strings.Contains(desc, "一次性") {
		t.Fatalf("索引行必须同时说清适用与不适用，实际：%q", desc)
	}
}
