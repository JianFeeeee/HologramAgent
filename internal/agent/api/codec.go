package api

// codec.go —— 编解码层的**统一出口**（无论 CGO 开关如何，调用方只认这里）。
//
// 分层：
//   codec_cgo.go   —— C 实现绑定（要求 cgo；CGO_ENABLED=0 下整包构建失败）
//   codec_pure.go  —— 纯 Go **参考实现**：只作黄金对照的规格基准，
//                     不是生产路径（不带 build tag，永远参与编译）
//   codec.go       —— 本文件：对外的稳定 API，含兜底与日志
//
// 这样调用方（provider.go / core）不需要写任何 build tag 分支。
//
// ★ 编解码层已「完全 C 化」：C 是唯一实现，不存在 CGO_ENABLED=0 回退。
//   理由（防两条语义分叉的实现同时跑）见 codec_cgo.go 顶部。

import (
	"log"
	"strings"
)

// ModelContextWindow 返回模型的最大上下文窗口（token 数）。
//
// 推断不出时（如 model="AUTO"）记一行日志并回退 defaultInferredContextWindow：
// 窗口被低估必须可见，部署方用 per-source
// core.llm.sources.<name>.context_window 显式声明真实值即可覆盖。
//
// 标称窗口 ≠ 有效窗口：接近满时注意力涣散，调用方应取 70-80% 为目标利用率。
func ModelContextWindow(model string) int {
	if w := modelContextWindowC(model); w != contextWindowUnknown {
		return w
	}
	log.Printf("[provider] cannot infer context window for model %q, falling back to %d; "+
		"if the real window is larger, set core.llm.sources.<name>.context_window",
		strings.ToLower(model), defaultInferredContextWindow)
	return defaultInferredContextWindow
}

// EstimateTokens 粗略估算 token 数。
//
// 注意：这是**高频热路径**（上下文裁剪对每个事件都调）。已完全 C 化，
// 但 cgo 边界固有成本约 30ns ⇒ 极短串上比直调纯 Go 慢（纳秒级，见
// codec_bench_test.go 的实测与 docs/zh/c-core/llm-orchestration-c.md §7.1）。
// 若某循环对极短串高频调用，正确应对是**把该循环 C 化（批量传一次）**，
// 而不是按长度分派回 Go —— 那会引入第二条可能分叉的实现。
func EstimateTokens(text string) int { return estimateTokensC(text) }

// TruncateByTokens 截断字符串至不超过 maxTokens 估计值。
func TruncateByTokens(s string, maxTokens int) string { return truncateByTokensC(s, maxTokens) }
