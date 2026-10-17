package api

// codec_pure.go —— 编解码层的**纯 Go 参考实现**。
//
// ★ 这**不是生产路径**。内核已「完全 C 化」：所有调用都走 C
//   （internal/agent/api/codec_cgo.go），本文件只服务两个目的：
//
//   1. **规格基准**：`codec_golden_test.go` 用同一组输入对比它与 C 实现，
//      断言逐值相等。C 侧的任何语义偏差（尤其畸形 UTF-8 的解码边界）
//      都由它抓出。没有它，「C 化没改错」就只是感觉。
//   2. **可读的规格**：C 是命令式字节游走，Go 版是直白的语义陈述。
//      两者并读时，改哪边都能立刻看出另一边该怎么改。
//
// 因此本文件**不带 build tag**，永远参与编译（测试要能引用）。
// 但没有任何生产代码路径调用它：编解码层要求 cgo 才能编译
// （CGO_ENABLED=0 下整包构建失败，见 codec_cgo.go 顶部）。
//
// ★ 零分配：本文件刻意不用 `len([]rune(s))` / `[]rune(s)`。
//   `[]rune(s)` 会分配 4×len 字节的临时切片（1KB 字符串就是 4KB 垃圾），
//   而 rune 计数与「前 keep 个 rune 的字节边界」都能用
//   utf8.RuneCountInString / utf8.DecodeRuneInString 游走完成，零分配。
//   实测这曾使纯 Go 的 TruncateByTokens 在 1KB 中文上分配 4208 B/2 allocs。

import (
	"strings"
	"unicode/utf8"
)

// defaultInferredContextWindow 是模型名无法推断窗口时的兜底。
//
// 32768 是个保守值，但它属于**静默降级**：模型名写 AUTO（网关自己选上游）时
// 匹配不到任何分支，内核就会拿着一份比真实小得多的窗口去算全部预算
// （实测：deepseek-v4.1-flash 能吞 990,034 token，而预算按 32768 算）。
// 兜底值本身不猜大：猜大会让请求直接撞上游 400。
const defaultInferredContextWindow = 32768

// contextWindowUnknown 是「模型名推断不出窗口」的哨兵值。
//
// 与 C 侧 HA_CODEC_CONTEXT_WINDOW_UNKNOWN 取值必须一致。
// 用哨兵而非直接返回兜底值：调用方要能区分「真推断出了」与
// 「推断不出、只能兜底」——后者必须记日志，让窗口被低估这件事可见。
const contextWindowUnknown = -1

// modelContextWindowPure 由模型名推断最大上下文窗口；推断不出返回哨兵。
// 标称窗口 ≠ 有效窗口：接近满时注意力涣散，调用方应取 70-80% 为目标利用率。
//
// ★ 分支顺序即语义：先匹配者胜出（例：gpt-4-turbo 必须先于裸 gpt-4）。
//
//	C 侧 ha_codec_model_context_window 必须保持同一顺序。
func modelContextWindowPure(model string) int {
	model = strings.ToLower(model)
	switch {
	case strings.Contains(model, "deepseek-v4") || strings.Contains(model, "deepseek-v3"):
		return 1048576
	case strings.Contains(model, "deepseek-r1") || strings.Contains(model, "deepseek-chat"):
		return 65536
	case strings.Contains(model, "gpt-4") && (strings.Contains(model, "turbo") || strings.Contains(model, "mini") || strings.Contains(model, "omni")):
		return 128000
	case strings.Contains(model, "gpt-4"):
		return 8192
	case strings.Contains(model, "gpt-3.5"):
		return 16384
	case strings.Contains(model, "claude-3.5") || strings.Contains(model, "claude-3"):
		return 200000
	case strings.Contains(model, "claude"):
		return 100000
	case strings.Contains(model, "gemini-1.5") || strings.Contains(model, "gemini-2"):
		return 1048576
	case strings.Contains(model, "gemini"):
		return 32768
	case strings.Contains(model, "qwen"):
		return 131072
	case strings.Contains(model, "glm") || strings.Contains(model, "chatglm"):
		return 131072
	case strings.Contains(model, "llama-3"):
		return 8192
	case strings.Contains(model, "llama-2"):
		return 4096
	case strings.Contains(model, "mistral") || strings.Contains(model, "mixtral"):
		return 32768
	case strings.Contains(model, "yi-") || strings.Contains(model, "零一"):
		return 200000
	case strings.Contains(model, "moonshot") || strings.Contains(model, "kimi"):
		return 131072
	default:
		return contextWindowUnknown
	}
}

// estimateTokensPure 粗略估算 token 数。
//
// 公式（2026-09-30 用真实 tokenizer 实测校准，样本含英/中/俄/日文、
// base64/hex/UUID、emoji 共 8 类）：取 min(字节数, 2×rune数)。
//
// 为何是这个形状：
//   - tokens ≤ 字节数恒成立（每个 token 至少覆盖 1 字节），
//     所以字节项是数学上界，对 base64/hex/UUID 这类高熵工具结果
//     （实测 1.3–1.7 字节/token）不会低估；
//   - 2×rune 项是实测校准：CJK ≈0.6 token/字、emoji 恰 2 token/rune，
//     纯按字节会把 CJK 过估到 5×，取 min 后与旧公式持平；
//   - 旧公式 runeCount×2 对英文散文过估 7×（实测 0.28 token/字符），
//     高熵 ASCII 过估 2.8× ⇒ 全部降到 1.4–3.5×。
//
// 已知失效模式（如实记录）：byte-fallback 型 tokenizer 对 CJK 可达 3 token/字
// ⇒ 2×rune 项低估 1.5×。这是旧公式同款风险（旧公式也是 2×rune），
// 且预算路径另有 0.8 系数兜底（ContextTokens = 0.8×窗口）。
//
// 用 RuneCountInString 而非 len([]rune(text))：后者会分配 4×len 字节。
// 两者对**畸形 UTF-8** 的计数一致（无效字节各计 1 个 rune）。
func estimateTokensPure(text string) int {
	if text == "" {
		return 0
	}
	runeCount := utf8.RuneCountInString(text)
	if runeCount == 0 {
		return 0
	}
	t := len(text) // 数学上界：tokens ≤ bytes
	if r := runeCount * 2; r < t {
		t = r // 实测校准（CJK/emoji）
	}
	if t < 1 {
		return 1
	}
	return t
}

// truncateByTokensPure 截断字符串至不超过 maxTokens 估计值。
//
// 语义（与 C 侧一致）：未超预算则原样返回；否则保留前 maxTokens/2 个 rune。
// 结果必然是输入的前缀，故直接按字节边界切片——无需构造 []rune。
func truncateByTokensPure(s string, maxTokens int) string {
	if maxTokens <= 0 || s == "" {
		return ""
	}
	runeCount := utf8.RuneCountInString(s)
	if runeCount*2 <= maxTokens {
		return s
	}
	keep := maxTokens / 2
	if keep >= runeCount {
		return s
	}
	// 游走到「前 keep 个 rune」的字节边界（零分配）。
	n := 0
	for count := 0; count < keep; count++ {
		_, size := utf8.DecodeRuneInString(s[n:])
		n += size
	}
	return s[:n]
}
