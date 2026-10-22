package memory

import (
	"fmt"
	"testing"
)

// ★ QuerySymbols：只提高信息量符号，泛词不进。
func TestQuerySymbols_泛词不进(t *testing.T) {
	got := QuerySymbols("本机 13010 端口对应什么")
	has := func(s string) bool {
		for _, g := range got {
			if g == s {
				return true
			}
		}
		return false
	}
	if !has("13010") {
		t.Errorf("必须提到精确数字串 13010，实际 %v", got)
	}
	// ★★ 「什么」是虚词，不该作为信号。
	//
	// 2026-10-04：原断言是「端口」「什么」都不该进。
	// 那个断言**部分正确** ——
	//
	//	「什么」是虚词  ⇒ 该滤（不变）
	//	「端口」是实词  ⇒ 不该滤
	//
	// 而「端口」此前之所以该滤，是因为滑窗会把它切成跨词伪词
	// （「本机服务监听哪些端口」→ 只有「本机服务」「监听哪些」）。
	// 换成 jieba 词典分词后「端口」是独立实词，
	// 滤掉它就等于**把目标词从符号集里删掉** ——
	// 生产探针 coexist 0/1 的根因正在于此。
	//
	// ★ 判据的价值就在于逼出这个区分：
	//   「泛词」是个模糊概念，而「虚词 vs 实词」是可判定的。
	if has("什么") {
		t.Errorf("虚词 %q 不该进符号集：%v", "什么", got)
	}
	// ★ 反向断言：实词必须留下，否则符号路失效。
	if !has("端口") {
		t.Errorf("★ 实词 %q 必须留在符号集（否则查询目标词被滤掉）：%v", "端口", got)
	}
}

// ★ SymbolScore：精确数字串命中 = 满分（这是符号路不可替代的能力）。
func TestSymbolScore_精确串命中满分(t *testing.T) {
	syms := QuerySymbols("本机 13010 端口对应什么")

	// 含精确串的块 ⇒ 满分
	s, matched, exact := SymbolScore(syms, "13010/13011 而非 12011")
	if !exact {
		t.Error("精确串命中必须标记 exact（它是布尔信号，不参与加权）")
	}
	if s < 1.0 {
		t.Errorf("精确串命中应给满分，实际 %.3f（matched=%v）", s, matched)
	}

	// 不含精确串的块 ⇒ 低分
	s2, _, _ := SymbolScore(syms, "本地网关8081")
	if s2 >= 1.0 {
		t.Errorf("不含 13010 的块不该满分，实际 %.3f", s2)
	}
	// ★ 实测里「本地网关8081」曾被向量排进 top8，但它不含 13010 ——
	//   符号路必须能把它压下去，否则融合没有意义。
	if s2 > 0.5 {
		t.Errorf("「本地网关8081」相对查询应低分，实际 %.3f", s2)
	}
}

// ★ 融合的核心判据：向量没召回但符号命中的块必须能进榜。
//
// 这条钉的是实测故障形态（生产快照 1391 块）：
//
//	查询「本机 13010 端口对应什么」
//	  向量 top8 → 「13000端口」「本地网关8081」…（都不含 13010）
//	  词法命中  → 「13010/13011 而非 12011」
func TestFuse_向量失手时符号能救回(t *testing.T) {
	target := MemoryBlock{ID: "b-target", Text: "13010/13011 而非 12011"}
	noise := MemoryBlock{ID: "b-noise1", Text: "本地网关8081"}
	noise2 := MemoryBlock{ID: "b-noise2", Text: "本机 443 按 SNI 透传到 192.168.2.106:3080"}

	// 向量路的候选里**没有** target —— 这正是实测形态
	vecHits := []BlockHit{
		{Block: noise, Score: 0.6435},
		{Block: noise2, Score: 0.6201},
	}
	out := fuseCandidates(vecHits, []MemoryBlock{target, noise, noise2},
		"本机 13010 端口对应什么", defaultWeights())

	if len(out) == 0 {
		t.Fatal("融合后不应为空")
	}
	if out[0].BlockID != "b-target" {
		t.Errorf("含精确串的块应排第一，实际第一是 %q（score=%.3f, why=%v）",
			out[0].BlockID, out[0].Score, out[0].Why)
	}
	if out[0].VectorHit != 0 {
		t.Errorf("它不该有向量分，实际 %.3f", out[0].VectorHit)
	}
	if out[0].SymbolHit < 1.0 {
		t.Errorf("应有满分符号分，实际 %.3f", out[0].SymbolHit)
	}
	// 归因必须可解释
	if len(out[0].Why) == 0 || out[0].Why[0] == "" {
		t.Error("必须给出 Why（模型需要知道为什么召回它）")
	}
}

// ★ 反向：符号不能压过明显更相关的向量命中。
func TestFuse_不误伤强向量命中(t *testing.T) {
	strong := MemoryBlock{ID: "b-strong", Text: "从零开发 HologramAgent QQ 插件：使用 plugindev 工具链"}
	other := MemoryBlock{ID: "b-other", Text: "AgentMail 投递规则"}

	vecHits := []BlockHit{
		{Block: strong, Score: 0.80},
		{Block: other, Score: 0.30},
	}
	out := fuseCandidates(vecHits, []MemoryBlock{strong, other},
		"从零开发 QQ 插件用什么工具链", defaultWeights())

	if out[0].BlockID != "b-strong" {
		t.Errorf("强向量+符号双命中应第一，实际 %q", out[0].BlockID)
	}
	if out[0].Score <= out[1].Score {
		t.Errorf("排序错误：%.3f vs %.3f", out[0].Score, out[1].Score)
	}
}

// ★ 零符号查询必须退化成纯向量序，且标注 why。
func TestFuse_无符号退化为向量序(t *testing.T) {
	a := MemoryBlock{ID: "a", Text: "随便一块"}
	b := MemoryBlock{ID: "b", Text: "另一块"}
	vecHits := []BlockHit{{Block: a, Score: 0.9}, {Block: b, Score: 0.5}}
	out := fuseCandidates(vecHits, []MemoryBlock{a, b}, "嗯", defaultWeights())

	if len(out) != 2 {
		t.Fatalf("退化为向量序时不该增删候选，实际 %d", len(out))
	}
	if out[0].BlockID != "a" {
		t.Errorf("应保持向量序，实际 %q", out[0].BlockID)
	}
	if len(out[0].Why) == 0 {
		t.Error("退化时也要给 Why，便于调试「为什么没走符号路」")
	}
}

// ★ 稳定排序：同分保持向量原序，不引入额外不确定性。
func TestFuse_同分保持向量序(t *testing.T) {
	a := MemoryBlock{ID: "a", Text: "AAA"}
	b := MemoryBlock{ID: "b", Text: "BBB"}
	vecHits := []BlockHit{{Block: a, Score: 0.5}, {Block: b, Score: 0.5}}
	out := fuseCandidates(vecHits, []MemoryBlock{a, b}, "AAA BBB", defaultWeights())
	if out[0].BlockID != "a" {
		t.Errorf("同分应保持原序，实际 %q", out[0].BlockID)
	}
}

// ★ 变体自证：把精确串从查询里去掉，救回能力必须消失。
//
//	—— 证明救回来自符号路，而不是「融合什么都返回」。
func TestFuse_变异自证_去掉精确串则失效(t *testing.T) {
	target := MemoryBlock{ID: "b-target", Text: "13010/13011 而非 12011"}
	noise := MemoryBlock{ID: "b-noise", Text: "本地网关8081"}

	vecHits := []BlockHit{{Block: noise, Score: 0.64}}

	withNum := fuseCandidates(vecHits, []MemoryBlock{target, noise},
		"本机 13010 端口对应什么", defaultWeights())
	withoutNum := fuseCandidates(vecHits, []MemoryBlock{target, noise},
		"本机端口对应什么", defaultWeights())

	posWith, posWithout := -1, -1
	for i, f := range withNum {
		if f.BlockID == "b-target" {
			posWith = i
		}
	}
	for i, f := range withoutNum {
		if f.BlockID == "b-target" {
			posWithout = i
		}
	}
	if posWith != 0 {
		t.Errorf("有精确串时应救回并排第一，实际位置 %d", posWith)
	}
	fmt.Printf("    变异自证: 有数字串→位置 %d，无数字串→位置 %d\n", posWith, posWithout)
	if posWithout != -1 && posWithout < posWith {
		t.Error("变异后不该比原形更好")
	}
}
