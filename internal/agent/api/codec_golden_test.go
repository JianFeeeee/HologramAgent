package api

// codec_golden_test.go —— 黄金对照测试：C 实现与纯 Go 参考实现必须逐值等价。
//
// 这是 C 化**最重要的验收**（见 docs/zh/c-core/llm-orchestration-c.md §五）。
// 没有它，「C 化没坏」就只是感觉，不是证据。
//
// 运行前提：**CGO_ENABLED=1**。内核已完全 C 化：本包**要求 cgo 才能编译**
// （无 !cgo 回退文件），故 CGO_ENABLED=0 时整包构建失败 —— 这是有意的
// 响亮失败，见 codec_cgo.go 顶部与 Makefile 的 check-codec-cgo-only。

import (
	"math/rand"
	"strings"
	"testing"
)

func TestGolden_ModelContextWindow(t *testing.T) {
	cases := []string{
		// 已覆盖的分支各取一个（含大小写、子公司前缀、带路径的模型名）
		"deepseek/deepseek-v4.1-flash", "deepseek-v4-flash", "DEEPSEEK-V3", "deepseek-r1",
		"deepseek-chat", "gpt-4-turbo", "gpt-4o-mini", "gpt-4-omni", "gpt-4", "gpt-4-0613",
		"gpt-3.5-turbo", "claude-3.5-sonnet", "claude-3-opus", "claude-opus-5", "claude-2",
		"gemini-1.5-pro", "gemini-2.0-flash", "gemini-pro", "qwen-max", "QWEN-MAX",
		"glm-4", "chatglm3", "llama-3-70b", "llama-2-7b", "mistral-large", "mixtral-8x7b",
		"yi-34b", "零一万物", "moonshot-v1-128k", "kimi-128k",
		// 推断不出（哨兵路径）
		"AUTO", "auto", "", "unknown-model", "some-local-model",
	}
	for _, model := range cases {
		c := modelContextWindowC(model)
		p := modelContextWindowPure(model)
		if c != p {
			t.Errorf("ModelContextWindow(%q): C=%d, pure=%d", model, c, p)
		}
	}
}

// TestGolden_EstimateTokens 覆盖 ASCII / 中文 / emoji / 空 / 长文本。
func TestGolden_EstimateTokens(t *testing.T) {
	cases := []string{
		"", "a", "ab", "abc", "hello world",
		"你好", "你好世界", "中文English混合", "a你b好c",
		"😀", "😀😀", "👨‍👩‍👧‍👦", // 含 ZWJ 组合序列（多 rune）
		strings.Repeat("x", 1000),
		strings.Repeat("你", 1000),
		"\n\t\r ", "{}[]()",
	}
	for _, s := range cases {
		c := estimateTokensC(s)
		p := estimateTokensPure(s)
		if c != p {
			t.Errorf("EstimateTokens(%q): C=%d, pure=%d", s, c, p)
		}
	}
}

// TestGolden_TruncateByTokens 覆盖边界：maxTokens 为 0/负/奇数/超限/恰好。
func TestGolden_TruncateByTokens(t *testing.T) {
	texts := []string{
		"", "a", "abc", "abcdefghij",
		"你好世界", "你好世界再见", "a你b好c世d界",
		"😀😀😀😀", strings.Repeat("x", 100), strings.Repeat("你", 100),
	}
	maxTokensList := []int{-1, 0, 1, 2, 3, 4, 5, 6, 7, 8, 20, 100, 200, 201, 1000}
	for _, s := range texts {
		for _, mt := range maxTokensList {
			c := truncateByTokensC(s, mt)
			p := truncateByTokensPure(s, mt)
			if c != p {
				t.Errorf("TruncateByTokens(%q, %d): C=%q, pure=%q", s, mt, c, p)
			}
			// 额外不变量：结果必须是原串前缀，且不超过预算
			if !strings.HasPrefix(s, c) && c != "" {
				t.Errorf("TruncateByTokens(%q, %d)=%q 不是原串前缀", s, mt, c)
			}
		}
	}
}

// TestGolden_Randomized 随机输入对拍：抓前面手写用例没想到的组合。
// 固定 seed，失败可复现。
func TestGolden_Randomized(t *testing.T) {
	rng := rand.New(rand.NewSource(20260924))
	alphabet := []rune("abcXYZ019 你好世界😀-_./")

	for i := 0; i < 2000; i++ {
		n := rng.Intn(40)
		var sb strings.Builder
		for j := 0; j < n; j++ {
			sb.WriteRune(alphabet[rng.Intn(len(alphabet))])
		}
		s := sb.String()

		if c, p := estimateTokensC(s), estimateTokensPure(s); c != p {
			t.Fatalf("EstimateTokens(%q): C=%d, pure=%d", s, c, p)
		}

		mt := rng.Intn(60) - 5
		if c, p := truncateByTokensC(s, mt), truncateByTokensPure(s, mt); c != p {
			t.Fatalf("TruncateByTokens(%q, %d): C=%q, pure=%q", s, mt, c, p)
		}

		// 模型名：拼一段 ASCII 再随机插入已知子串
		models := []string{"deepseek-v4", "gpt-4-turbo", "claude-3", "qwen", "llama-3", "kimi", "zzz"}
		m := models[rng.Intn(len(models))]
		if rng.Intn(2) == 0 {
			m = strings.ToUpper(m)
		}
		if c, p := modelContextWindowC(m), modelContextWindowPure(m); c != p {
			t.Fatalf("ModelContextWindow(%q): C=%d, pure=%d", m, c, p)
		}
	}
}

// TestGolden_InvalidUTF8 用**任意字节**（含畸形序列）对比 C 与纯 Go。
//
// 为什么必须有：C 侧的解码必须与 Go 的 utf8.DecodeRuneInString 完全同语义
// ——尤其是「无效/截断序列只前进 1 字节」（Go 返回 RuneError 且 size=1）。
// 若 C 侧放宽校验，两侧 rune 计数就会分叉，而合法 UTF-8 的测试**抓不到**这个。
// 这是 C 化最容易出错、也最容易被漏测的地方。
func TestGolden_InvalidUTF8(t *testing.T) {
	// 覆盖各类边界字节：续字节、过长编码、代理对、超出 U+10FFFF、截断序列。
	seed := []byte{
		0x00, 0x41, 0x7F, 0x80, 0xBF, 0xC0, 0xC1, 0xC2, 0xDF, 0xE0, 0xE1,
		0xED, 0xEF, 0xF0, 0xF1, 0xF4, 0xF5, 0xF8, 0xFE, 0xFF,
		0xE4, 0xBD, 0xA0, // 你
		0xF0, 0x9F, 0x98, 0x80, // 😀
		0xED, 0xA0, 0x80, // 0xED 0xA0 0x80 = UTF-16 代理对，非法
		0xC0, 0x80, // 过长编码 NUL，非法
		0xF4, 0x90, 0x80, 0x80, // > U+10FFFF，非法
	}
	rng := rand.New(rand.NewSource(20260925))

	for i := 0; i < 3000; i++ {
		n := rng.Intn(24)
		b := make([]byte, n)
		for j := range b {
			if rng.Intn(3) == 0 {
				b[j] = byte(rng.Intn(256)) // 完全随机字节
			} else {
				b[j] = seed[rng.Intn(len(seed))]
			}
		}
		s := string(b)

		if c, p := estimateTokensC(s), estimateTokensPure(s); c != p {
			t.Fatalf("EstimateTokens(%q) 畸形输入: C=%d, pure=%d", b, c, p)
		}
		// 截断也必须落在同一字节边界上（不得切在字符中间，且两侧一致）
		mt := rng.Intn(40) - 2
		if c, p := truncateByTokensC(s, mt), truncateByTokensPure(s, mt); c != p {
			t.Fatalf("TruncateByTokens(%q, %d): C=%q, pure=%q", b, mt, c, p)
		}
	}
}

// TestGolden_TruncateAlwaysPrefix 不变量：截断结果必须是原串前缀，且 <= 原长。
func TestGolden_TruncateAlwaysPrefix(t *testing.T) {
	inputs := []string{
		"", "a", "abc", "你好世界", "a你b好c", "😀😀😀", strings.Repeat("x", 300),
		strings.Repeat("中", 300), "\xe4\xbd", "a\xed\xa0\x80b",
	}
	for _, s := range inputs {
		for mt := -2; mt <= 60; mt++ {
			got := truncateByTokensC(s, mt)
			if !strings.HasPrefix(s, got) {
				t.Fatalf("TruncateByTokens(%q, %d)=%q 不是原串前缀", s, mt, got)
			}
			if len(got) > len(s) {
				t.Fatalf("TruncateByTokens(%q, %d) 结果长于输入", s, mt)
			}
			if got != truncateByTokensPure(s, mt) {
				t.Fatalf("TruncateByTokens(%q, %d): C=%q, pure=%q", s, mt, got, truncateByTokensPure(s, mt))
			}
		}
	}
}
