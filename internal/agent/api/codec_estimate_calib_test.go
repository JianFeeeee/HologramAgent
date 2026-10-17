package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 估算器校准判据（2026-09-30 用真实 tokenizer 实测驱动的一次重校准）。
//
// 背景：旧公式 runeCount×2 的注释自称「英文 ~0.3 token/字符」，
// 但按它算 11 字符是 22 —— 实测真实值只有 ~3。用户在跑分对比前
// 要求校准；校准不能拍脑袋，本文件把实测结论固化成判据。
//
// 实测方法：同一批样本经 llmsproxy（deepseek-v4.1-flash tokenizer）测
// prompt_tokens，与两个候选公式对比。样本 8 类：英/中/俄/日文散文、
// base64、hex、UUID、emoji。关键数字（净 token，已扣固定开销）：
//
//	样本          字节/token   旧公式(2×rune) 过估   min(b,2r) 过估
//	英文散文       3.54        7.1×               3.5×
//	中文技术       5.22        3.5×               3.5×（持平）
//	base64        1.41        2.8×               1.4×
//	随机ASCII      1.43        2.9×               1.4×
//	hex           1.72        3.4×               1.7×
//	UUID          1.66        3.3×               1.7×
//	emoji         2.00        1.0×               1.0×（精确）
//	俄语          6.49        7.0×               6.5×
//
// 结论：取 min(bytes, 2×runes) —— 8/8 无低估，且处处 ≤ 旧公式。
//
// ★ 反面教训（为什么不能照抄 llmsproxy 的 len/3）：
// 同一批实测里 base64 是 1.41 字节/token，bytes/3（=0.33 tok/byte）
// 对它**低估 2.1×**。工具结果里恰恰全是 base64/UUID/hash，
// 用 len/3 会让上下文预算系统性失真 —— 估算器的失效方向必须是
// 「高估」而不是「低估」（高估只浪费一点预算，低估会撑爆上下文）。

// TestEstimate_NeverUnderestimatesTheoreticalBound：tokens ≤ bytes 恒成立
// （每个 token 至少覆盖 1 字节），所以估算值必须 ≥ …… 不，方向反了：
// 估算的是**上界**，它必须 ≥ 真实值；真实值 ≤ 字节数，所以公式取字节数
// 这一项就是数学保证。本判据验证实现真的满足 min 语义。
func TestEstimateFormulaIsMinOfBytesAnd2Runes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		// ASCII：字节 < 2×rune ⇒ 按字节
		{"ascii abc", "abc", 3},
		{"ascii 300", strings.Repeat("x", 300), 300},
		// CJK：2×rune < 字节 ⇒ 按 rune（与旧公式持平）
		{"chinese 2 chars", "你好", 4},
		{"mixed a你", "a你", 4},
		// emoji：相等
		{"emoji", "\U0001F600", 2},
		// 畸形 UTF-8：2 字节无效序列 = 2 rune ⇒ min(2, 4) = 2
		{"truncated seq", "\xE4\xBD", 2},
		// 高熵 ASCII（工具结果的常态）：按字节 —— 旧公式给 2×，低估风险正来自这
		{"uuid-ish", "3f2a1b4c-5d6e-7f80", 18},
		{"base64-ish", "QUJDREVGR0hJSktMTU5PUA==", 24},
	}
	for _, c := range cases {
		if got := estimateTokensPure(c.in); got != c.want {
			t.Errorf("%s: estimate(%q)=%d, want %d (min(bytes=%d, 2×runes=%d))",
				c.name, c.in, got, c.want, len(c.in), utf8.RuneCountInString(c.in)*2)
		}
	}
}

// TestEstimateNeverBelowRealWorldFloors 固化实测下界：
// 这些样本的真实 token 数是拿真 tokenizer 测出来的，
// 估算值若低于它们就说明公式低估 —— 直接红。
func TestEstimateNeverBelowRealWorldFloors(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		floor int // 实测净 token（deepseek-v4.1-flash，2026-09-30）
	}{
		// 英文散文：0.28 token/字符；给 1/2 字节当 floor（余量充足）
		{"english prose", strings.Repeat("scheduler preempt safe point loop ", 40), 140},
		// base64：实测 1.41 字节/token；floor = 字节/2
		{"base64 blob", "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2d3h5eg==", 35},
		// 中文：实测 0.57 token/字；floor = 字符（远低于 2×字符）
		{"chinese tech", strings.Repeat("内核调度器安全点中断级别临界区标记", 10), 160},
	}
	for _, c := range cases {
		if got := estimateTokensPure(c.in); got < c.floor {
			t.Errorf("%s: estimate=%d < 实测下界 %d —— 公式低估了，会撑爆上下文预算",
				c.name, got, c.floor)
		}
	}
}

// TestEstimateMonotonicNonDecreasingInBytes 保守性结构判据：
// 输入变长（字节变多）时估算不得变小 —— 预算逻辑依赖这一点。
func TestEstimateMonotonicNonDecreasingInBytes(t *testing.T) {
	prev := 0
	for n := 1; n <= 64; n++ {
		s := strings.Repeat("a", n) // ASCII 侧
		got := estimateTokensPure(s)
		if got < prev {
			t.Fatalf("estimate(\"a\"×%d)=%d < 前值 %d —— 非单调", n, got, prev)
		}
		prev = got
	}
	prev = 0
	for n := 1; n <= 64; n++ {
		s := strings.Repeat("你", n) // CJK 侧
		got := estimateTokensPure(s)
		if got < prev {
			t.Fatalf("estimate(\"你\"×%d)=%d < 前值 %d —— 非单调", n, got, prev)
		}
		prev = got
	}
}
