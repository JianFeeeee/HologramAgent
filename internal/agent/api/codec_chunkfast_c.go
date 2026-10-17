//go:build cgo

package api

// codec_chunkfast_c.go —— parseOpenAICompatibleStreamChunkFull 的 C 快速路径
//
// ============================ 契约（务必先读） ============================
// 本文件是**纯优化**：它必须与 chunkParseGo 对**所有输入**产出完全相同的
// (StreamChunk, bool)。保证方式不是「小心写」，而是结构上的三条：
//
//  1. 任一环节判「不确定」⇒ **整体回退** chunkParseGo。没有任何分支
//     「尽力猜」或「部分采用」。
//  2. 每个「C 已判过合法」的子树，都用**与 Go 侧完全相同的 Go 类型**去
//     unmarshal ⇒ 类型检查语义天然一致，不靠 C 复刻类型规则。
//  3. 拼装（chunkAssemble）与工具调用归一化（normalizeStreamToolCall）
//     由两条路径**共用**，结构上无法分叉。
//
// 回退触发条件（穷举）：
//   · 顶层不是「恰好一个」良构对象（含尾部残留，见 ha_sse_root_object）
//   · 发现**重复键**（§5.1 字段级合并语义，C 不实现）
//   · content 是对象/数字/字面量（stringifyContent 需 json.Marshal 重新编码，§5.2）
//   · tool_calls 元素畸形 / 数组元素过多
//   · 任何子树畸形或缓冲不足
//
// 实测：真实负载三种块全部走快速路径；探针里的畸形、重复键、对象 content
// 等形态全部命中回退。
//
// ============================ ★ 当前默认**关闭**（实测比原实现慢） ============================
// 见 codec_chunkfast_bench_test.go 的实测：
//   content_ascii  Entry 2016ns/20allocs  vs  GoOnly 1325ns/13allocs
//   toolcall       Entry 5854ns/33allocs  vs  GoOnly 3270ns/21allocs
//   usage          Entry 3170ns/24allocs  vs  GoOnly 2832ns/12allocs
//
// 根因（已逐项测量，不是猜测）：
//  1. **每次键查找 205ns + 2 allocs**（out-params 逃逸到堆），
//     而**裸 cgo 边界就有 168ns**。一次解析需要 5+ 次查找
//     （choices→[0]→delta→content/reasoning/tool_calls→finish_reason）
//     ⇒ 边界成本 ≈ 1µs，恰好吃掉全部收益。
//  2. 每个字段还各自一次小 Unmarshal + 一次 decBuf 分配。
//  而 Go 侧是**一次** Unmarshal 遍历建整棵树。
//
// ⇒ 本架构是「**用很多次廉价调用换一次昂贵调用**」，在这个尺寸上不划算。
//   正确的前进方向是**减少边界次数**，而不是调优现有代码：
//     · 一次 C 调用返回**全部**字段的 span（批量，不逐字段往返）
//     · 结果写入**调用方栈上**的 C 结构体（消除 out-param 逃逸）
//     · 仅在 content/usage 确需重新编码时回退 Go
//   天花板实测：若边界成本归零，Go 侧代价 ≈ 505ns/7allocs
//   （对 1239ns/13allocs）⇒ **方向对，但当前实现没到**。
//
// ★ 保留本文件的理由：它同时是
//   ① 正确性基准（6 万+ 差分用例已钉死 C 与 Go 逐值等价）
//   ② 上述改造的**已验证起点**（field-locating 与回退判据都已验证正确）
//   ③ 一条**永不静默回退**的机制：若未来把它切回默认开启，
//      TestChunkFast_BenchGate 会立刻用基准把它按回去。

import "encoding/json"

const chunkFastEnabled = false

// chunkParseGo 是原始实现（整块 json.Unmarshal），作为快速路径的**唯一判据**
// 与回退目标。
func chunkParseGo(data string) (StreamChunk, bool) {
	var raw struct {
		Choices []struct {
			Delta struct {
				Content          interface{}      `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []openAIToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		UpstreamUsage chunkUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return StreamChunk{}, false
	}
	// 只把 choices[0] 转成装配用的形态 —— 与原实现一致（原实现只读 [0]，
	// 但 len() 判空用的是整个切片长度）。
	var choices []chunkChoice
	if len(raw.Choices) > 0 {
		c := raw.Choices[0]
		choices = []chunkChoice{{
			content:   stringifyContent(c.Delta.Content),
			reasoning: c.Delta.ReasoningContent,
			toolCalls: normalizeStreamToolCalls(c.Delta.ToolCalls),
			finishPtr: c.FinishReason,
		}}
	} else if len(raw.Choices) == 0 {
		choices = nil
	}
	return chunkAssemble(choices, raw.UpstreamUsage)
}

// chunkUsage 镜像 Go 侧的 UpstreamUsage 匿名结构。
type chunkUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	Prompt              int `json:"prompt"`
	Completion          int `json:"completion"`
	Total               int `json:"total"`
	PromptCacheHit      int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss     int `json:"prompt_cache_miss_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// chunkChoice 是装配用的形态：Content 已过 stringifyContent。
type chunkChoice struct {
	content   string
	reasoning string
	toolCalls []ToolCall
	// finishPtr 保留三态区分：缺失/null ⇒ nil；"" ⇒ 非 nil 但空串
	//（空串**不算**终止信号，sensenova 每块都发 ""）。
	finishPtr *string
}

// tokenUsageFromChunkUsage 把上游 usage 归一成 TokenUsage ——
// **字段映射与缓存规则的唯一落点**。返回 nil 表示上游没报用量。
//
// 为何必须只有一处：此前 chunkAssemble（流式）与
// parseOpenAICompatibleResponse（非流式）各写了一份字段清单与缓存归属判断。
// 两份实现必然漂移，而漂移的表现是「同一份上游数据，流式与非流式给出
// 不同命中率」—— 2026-09-30 跑分实测的「命中率恒 100%」（真实 53%）
// 就属于这一类：只有一边补了未命中数。
//
// 字段优先级与 llmsproxy 的 recordChatUsage 同序：
// 两个项目对同一份上游数据必须给同一答案。
func tokenUsageFromChunkUsage(usage chunkUsage) *TokenUsage {
	if usage.Total == 0 && usage.TotalTokens == 0 &&
		usage.Prompt == 0 && usage.PromptTokens == 0 {
		return nil
	}
	// 缓存归属：优先 OpenAI v2 的 prompt_tokens_details.cached_tokens，
	// 回退 DeepSeek 遗留的 prompt_cache_hit_tokens。
	//
	// ★ PromptTokensDetails 非 nil 即表示「上游报了缓存细节」——
	// 即使 CachedTokens 为 0 也要置 CacheReported，否则
	// 「报了但 0 命中」会被当成「没报」，看着成了「无数据」。
	var cacheRead int
	cacheReported := false
	if d := usage.PromptTokensDetails; d != nil {
		cacheRead = d.CachedTokens
		cacheReported = true
	} else if usage.PromptCacheHit > 0 {
		cacheRead = usage.PromptCacheHit
		cacheReported = true
	}
	u := &TokenUsage{
		Prompt:        pickFirstInt(usage.PromptTokens, usage.Prompt),
		Completion:    pickFirstInt(usage.CompletionTokens, usage.Completion),
		Total:         pickFirstInt(usage.TotalTokens, usage.Total),
		CacheRead:     cacheRead,
		CacheMiss:     usage.PromptCacheMiss,
		CacheReported: cacheReported,
	}
	if d := usage.CompletionTokensDetails; d != nil {
		u.ReasoningTokens = d.ReasoningTokens
	}
	// 上游只报命中侧时补出未命中数（规则单一实现见 DeriveCacheMiss）。
	u.DeriveCacheMiss()
	return u
}

// chunkAssemble 把已备好的选择与 usage 拼成 StreamChunk。
// **两条路径共用**它 ⇒ 拼装逻辑不可能分叉。
//
// 这也是缓存/推理字段的**唯一**落地点：快速路径（C 导航）与回退路径
// （encoding/json）都在这里汇合，所以只需在这里提取一次。
func chunkAssemble(choices []chunkChoice, usage chunkUsage) (StreamChunk, bool) {
	// 用量构造统一走 tokenUsageFromChunkUsage（字段映射与缓存规则的唯一落点）。
	u := tokenUsageFromChunkUsage(usage)
	if len(choices) == 0 {
		// 纯 usage 心跳块：有 usage 就透传，否则丢弃
		if u != nil {
			return StreamChunk{Usage: u}, true
		}
		return StreamChunk{}, false
	}
	c := choices[0]
	ck := StreamChunk{
		Content:          c.content,
		ReasoningContent: c.reasoning,
		ToolCalls:        c.toolCalls,
		Usage:            u,
	}
	if c.finishPtr != nil && *c.finishPtr != "" {
		ck.Done = true
		ck.FinishReason = *c.finishPtr
	}
	return ck, true
}

// ---------------------------------------------------------------------
// C 快速路径
// ---------------------------------------------------------------------

// chunkParseFast 尝试 C 快速路径。
//
// ============================ 第三刀的重做：一次 cgo 调用 ============================
// 上一版逐字段往返（5+ 次 findKey，每次 ~168ns 边界 + 2 allocs）造成固定成本
// 约 1µs，比原实现更慢。本版把全部定位压进**一次** C 调用
// （ha_sse_chunk_locate），并在同一趟里完成键分派与字符串解码。
//
// 返回 (chunk, handled, decided)。handled=false ⇒ 调用方用 chunkParseGo。
func chunkParseFast(data string) (StreamChunk, bool, bool) {
	loc := locateChunkBatch(data)
	switch loc.status {
	case chunkTypeFail:
		// C 已判定「与 Go 一致的整块作废」⇒ 直接给答案，无需回退
		return StreamChunk{}, false, true
	case chunkOK:
		// 继续
	default:
		return StreamChunk{}, false, false
	}

	// ---- usage：整棵子树交给 encoding/json（9 个字段 + 类型规则）----
	var usage chunkUsage
	switch loc.usageKind {
	case kindAbsent, kindNull:
		// 零值
	case kindObject:
		if err := json.Unmarshal(loc.usageSpan.bytes(), &usage); err != nil {
			return StreamChunk{}, false, true // 类型不符 ⇒ 整块作废
		}
	default:
		return StreamChunk{}, false, false
	}

	// ---- delta 非对象 ⇒ 与 Go 的 Unmarshal 失败一致 ----
	if loc.deltaKind == kindOther {
		return StreamChunk{}, false, true
	}

	// ---- tool_calls：整段 unmarshal 成 []openAIToolCall ----
	// ★ 用与 Go 完全相同的类型 ⇒ arguments 的 interface{} 形态与类型检查
	//   全部由 encoding/json 负责；归一化共用 normalizeStreamToolCall。
	var toolCalls []ToolCall
	switch loc.toolCallsKind {
	case kindAbsent, kindNull:
		// nil
	case kindArray:
		var raw []openAIToolCall
		if err := json.Unmarshal(loc.toolCallsSpan.bytes(), &raw); err != nil {
			return StreamChunk{}, false, true // 元素类型不符 ⇒ 整块作废
		}
		toolCalls = normalizeStreamToolCalls(raw)
	default:
		return StreamChunk{}, false, true
	}

	// ---- 拼装（与 Go 路径共用 chunkAssemble）----
	var choices []chunkChoice
	if loc.choicesPresent && loc.choicesCount > 0 {
		ch := chunkChoice{
			content:   loc.content,
			reasoning: loc.reasoning,
			toolCalls: toolCalls,
		}
		if loc.finishKind == kindString {
			// 保留三态：缺失/null ⇒ nil；"" ⇒ 非 nil 空串（不算终止信号）
			f := loc.finish
			ch.finishPtr = &f
		}
		choices = []chunkChoice{ch}
	}
	ck, ok := chunkAssemble(choices, usage)
	return ck, true, ok
}
