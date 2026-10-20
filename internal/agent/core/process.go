package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// continuationPlaceholder 是工具轮之后补的 user 占位内容。
//
// zen 兼容网关要求请求最后一条必须是 user（thinking 续写模式校验），工具轮
// 产出 assistant/tool 结尾会被 400 拒绝；首轮 system 结尾不补，否则会覆盖
// 真实用户输入。
//
// 用独立常量 + 精确等值判定，是因为这条消息是**核心自己插入的**、不是用户输入，
// 所以可以安全地按内容识别并在补位前移除上一条，保证至多一条。
const continuationPlaceholder = "请根据以上工具结果继续。"

// replyDeliveredPlaceholder 是「本批工具调用全部是输出通道发送」之后补的占位。
//
// 为何不能继续用通用的「请继续」：异步通道（qq/wechat）的回复**只能**经
// output_send__* 交付（纯文本不送达，见 buildSystemPrompt 的输出规则）。于是
// 模型「已经回复完了」的表达形式就是一个工具调用，而紧随其后的
// 「请根据以上工具结果继续。」会被读成「还要再做一步」——能做的「一步」恰好
// 还是再发一条消息。两者叠加成自我强化的发送循环：生产实测单轮 34 次
// output_send__qq、持续 514 秒，直到 QQ 插件自己的循环保险拒绝发送才停下。
//
// 所以这里换成一条明确的终止许可：已回复完就直接返回纯文本收尾。
const replyDeliveredPlaceholder = "若你的回复已完成，直接返回纯文本即可结束本轮，无需再调用任何工具。"

// continuationFor 选择工具轮之后补位的 user 占位文案。
// replyOnly 表示上一批工具调用全部是输出通道发送（即模型刚交付了回复）。
func continuationFor(replyOnly bool) string {
	if replyOnly {
		return replyDeliveredPlaceholder
	}
	return continuationPlaceholder
}

// isOutputDeliveryTool 判断工具是否是「向输出通道交付内容」。
// output_send__{channel}_help 只是查询用法，不算交付。
func isOutputDeliveryTool(name string) bool {
	return strings.HasPrefix(name, "output_send__") && !strings.HasSuffix(name, "_help")
}

// isContinuationPlaceholder 判断一条 user 消息是否是本机制插入的占位。
// 只按两个常量精确匹配，不碰任何真实用户消息。
func isContinuationPlaceholder(m agentAPI.Message) bool {
	return m.Role == "user" &&
		(m.Content == continuationPlaceholder || m.Content == replyDeliveredPlaceholder)
}

// toolOutputForQuery 返回用于相关性计算的工具输出**有效内容**。
//
// 为什么要过 Cleaner 而不是直接用原始 result：ContextPolicy=prune 的入参是
// **相关性查询向量**——它决定保留/归档哪些上下文事件。原始工具输出里混着
// ANSI 转义、base64、JSON 包装等噪声，直接拿去向量化会让打分失真。
// 而 ToolDef.Cleaner 的契约本就写着“仅在向量化/jieba/蒸馏时调用”，裁剪正是
// 在向量化，所以这里必须过它（此前只在构建事件向量时用了，裁剪查询漏了）。
//
// Cleaner 未注册或 RPC 失败时回退原文（清洗是计算层优化，不能因此丢内容）；
// 返回空串时也回退——空串会让查询向量退化成零向量，裁剪就失去判据。
func (a *Agent) toolOutputForQuery(toolName, raw string) string {
	if a.stageHost == nil {
		return raw
	}
	cleaner := a.stageHost.ToolDefCleaner(toolName)
	if cleaner == nil {
		return raw
	}
	if cleaned := cleaner(raw); cleaned != "" {
		return cleaned
	}
	return raw
}

// recallMsgMarker 是工具触发召回时注入的 system 消息前缀。
// 用它做去重与替换的识别标（与用户/中断的 system 消息区分开）。
const recallMsgMarker = "【记忆召回】"

// appendOrReplaceRecall 把一段召回文本作为 system 消息挂到消息末尾。
//
// 同一任务内多次触发（如模型多次调用 qq_get_message）时**替换**上一条召回，
// 而不是累加：否则召回会线性叠进 prompt，把上下文与 token 预算越挤越紧。
// 替换位置固定在末尾，不影响 tool/assistant 消息的配对。
func appendOrReplaceRecall(msgs []agentAPI.Message, recallText string) []agentAPI.Message {
	if recallText == "" {
		return msgs
	}
	full := recallMsgMarker + "\n" + recallText
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "system" && strings.HasPrefix(msgs[i].Content, recallMsgMarker) {
			msgs[i].Content = full
			return msgs
		}
	}
	return append(msgs, agentAPI.Message{Role: "system", Content: full})
}

// dropContinuationPlaceholders 移除此前由本机制插入的 user 占位。
//
// 为什么必须移除而不仅仅是“不再追加”：`msgs` 在循环外创建、循环内只增不减，
// 占位是核心自己插的、不是用户说的话。不移除的话，prompt 里就会线性叠上
// N 条一模一样的“继续”，把前缀上下文（含记忆注入）往后挤。
func dropContinuationPlaceholders(msgs []agentAPI.Message) []agentAPI.Message {
	out := msgs[:0]
	for _, m := range msgs {
		if isContinuationPlaceholder(m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// chatStreamWithFallback 优先流式调用 provider，失败时回退非流式 Chat()。
//
// 流式路径：ChatStream 拿到 chunk channel，逐块累积 content/reasoning_content，
// 并发布 EventReasoningDelta / EventContentDelta 增量事件（新订阅者可选订，
// 旧订阅者不认识自然忽略）。流结束后拼出与 Chat() 等价的 CompletionResponse
// 返回——process() 的后续逻辑（stageCtx/聚合事件/工具循环）完全不变。
//
// 回退条件：ChatStream 返回错误（连接失败、provider 不支持流式）。
// 已收到部分 chunk 后出错则不回退（避免重复生成），直接返回已累积内容。
//
// 超时收益：首包 ~1-3s 到达即建立活性，后续只要 token 在流动就不会触发
// 空闲超时；总生成时长不再受限於 180s 整体超时。
func chatStreamWithFallback(ctx context.Context, p agentAPI.Provider, req *agentAPI.CompletionRequest, a *Agent, channel string) (*agentAPI.CompletionResponse, error) {
	ch, err := p.ChatStream(ctx, req)
	if err != nil {
		// 上下文已取消/超时：**绝不能**回退到非流式 Chat。
		//
		// 回退意味着再发一次完整请求，而这时用户已经按了停止（或请求已超时），
		// 结果是“按了停止又跑了一遍”——停止按钮看起来毫无反应的一个真实成因。
		//
		// 判据取 **ctx.Err()** 而不是“错误是不是 context.Canceled”：很多 provider
		// 不支持流式时也回 Canceled 表示“请走非流式”（仓里大量假 provider 即如此），
		// 那种情况必须继续回退，否则会把“不支持流式”误当成“已被取消”。
		if ctx.Err() != nil {
			if err == nil {
				err = ctx.Err()
			}
			return nil, err
		}
		log.Printf("[agent] stream connect failed (%v), falling back to non-stream chat", err)
		return p.Chat(ctx, req)
	}

	resp, accErr := accumulateStream(ctx, ch, a, channel, req.MaxTokens)

	// 中断/超时取消必须保持取消语义传给调用方（与原 Chat() 行为一致：
	// 被 cancel 时丢弃已收内容返回 err），让 process() 的 continue 分支
	// 重启轮次并以 [中断消息] 注入打断内容。绝不能把部分内容当成功返回，
	// 否则用户打断会被无视、继续执行工具/输出。
	if errors.Is(accErr, context.Canceled) || errors.Is(accErr, context.DeadlineExceeded) {
		// 通知客户端：本轮流式作废，清空 delta 累积并定格已显示内容
		if a != nil {
			a.publishEvent(events.EventContentDelta, map[string]interface{}{
				"content": "",
				"channel": channel,
				"reset":   true,
			})
		}
		return resp, accErr
	}

	if accErr == nil {
		return resp, nil
	}

	// 其他错误（网络中断等）：已累积到实质内容则返回部分结果，否则回退非流式。
	if resp != nil && (resp.Content != "" || len(resp.ToolCalls) > 0) {
		log.Printf("[agent] stream interrupted mid-way (%v), returning partial result", accErr)
		return resp, nil
	}
	// 取消类错误不能回退（否则等于再跑一遍完整的非流式请求）。
	// 同样以 ctx.Err() 为准：真取消才拦，provider 探活不算。
	if ctx.Err() != nil {
		return resp, accErr
	}
	log.Printf("[agent] stream failed before content (%v), falling back to non-stream chat", accErr)
	return p.Chat(ctx, req)
}

// toolCallAcc 累积流式 tool call 的各个分片。OpenAI 风格：每个 index 的
// id/name/arguments 跨多个 chunk 增量到达，arguments 是 JSON 字符串分片。
type toolCallAcc struct {
	id      string
	name    string
	argsRaw strings.Builder
}

// truncatedArgsError 把「参数被 MaxTokens 截断」变成模型能看懂的一句话。
//
// 背景（实测 2026-09-19）：core.llm.max_tokens=4096 会在长参数（整段脚本/
// 大 JSON）写到一半时从中间切断，上游回 finish_reason="length"。旧实现把这个
// 信号整个丢掉，残缺 JSON 解析失败后静默降级成空 map，工具只看到参数为空
// （files_write → "path is required"），模型因此完全看不出是被截断，原样重试
// 四遍、次次撞同一堵墙（日志里 4 次 files_write 失败即此）。
//
// 这里改成**显式报错 + 可执行指引**：告诉模型参数不完整、要拆小或改分批写。
// 调用方（executeToolCallInner）据此短路，不再拿空参数去调工具。
func truncatedArgsError(name string, rawLen, maxTokens int) string {
	return fmt.Sprintf(
		"工具 %s 调用被截断：参数 JSON 不完整（收到 %d 字节），"+
			"原因是本轮流式输出达到了 max_tokens=%d 的上限（上游 finish_reason=length），"+
			"不是网络或工具的问题。请改用更小的参数重试：把长内容拆成多次调用"+
			"（例如先写文件的前半部分，再用追加/编辑的方式补后半部分），"+
			"或先用更少的字段完成本次调用。请勿原样重复上一次的调用。",
		name, rawLen, maxTokens)
}

// malformedArgsError 把「参数 JSON 写坏了」变成模型能自己改对的一句话。
//
// 实测最常见的一种：带单位的值忘了加引号 —— `{"command": "ls", "timeout": 20s}`。
// 工具 schema 把这类参数声明为 string、示例又写成 “10s, 1m, 30s”，模型容易照抄格式。
// 旧实现丢整条参数，模型只看到 “command is required”，永远不知道坏在 timeout。
func malformedArgsError(name, raw string) string {
	return fmt.Sprintf(
		"工具 %s 的参数不是合法 JSON，本次调用未执行（这是参数格式问题，不是工具故障）。"+
			"请重新生成完整参数并注意：所有字符串值必须带引号 —— 特别是超时/时长这种"+
			"带单位的值，要写成 \"20s\" 而不是 20s。收到 %d 字节，开头是：%s",
		name, len(raw), truncateStr(raw, 160))
}

// accumulateStream 消费 chunk channel，累积为完整 CompletionResponse，
// 同时发布增量事件。返回的 response 与非流式 Chat() 的返回等价。
//
// maxTokens 是本轮请求发送的输出上限，只参与错误文案（把「参数被截断」说成
// 模型能执行的话），不参与解析逻辑。
func accumulateStream(ctx context.Context, ch <-chan agentAPI.StreamChunk, a *Agent, channel string, maxTokens int) (*agentAPI.CompletionResponse, error) {
	resp := &agentAPI.CompletionResponse{
		ToolCalls: make([]agentAPI.ToolCall, 0),
	}
	accs := make(map[int]*toolCallAcc) // index → 累积中的 tool call
	var lastFinish string

	flushToolCall := func(idx int) {
		acc := accs[idx]
		if acc == nil {
			return
		}
		if acc.name == "" {
			log.Printf("[agent] stream tool_call idx=%d flushed with EMPTY name (args=%q) — dropped", idx, truncateStr(acc.argsRaw.String(), 120))
			delete(accs, idx)
			return
		}
		args, argsOK := parseToolArgsJSON(acc.argsRaw.String())
		raw := strings.TrimSpace(acc.argsRaw.String())
		// 空参诊断：区分「上游没发分片」(raw="")、「混拼污染」(解析失败) 与「合法空对象」({})。
		if !argsOK {
			log.Printf("[agent] stream tool_call %s (idx=%d) argument fragments invalid JSON: %q", acc.name, idx, truncateStr(raw, 200))
		} else if raw == "" {
			log.Printf("[agent] stream tool_call %s (idx=%d) received NO argument fragments", acc.name, idx)
		}
		// 参数没法解析时**绝不能**静默降级成空 map：工具只能报 “xxx is required”，
		// 那与真因（参数 JSON 写坏了）毫无关系，模型据此重试只会再撞一次
		// （实测 2026-09-19：cmd_run 失败 34 次、某任务 48% 时间耗在这上面）。
		// 分两种成因给出可执行的指引：
		//   - finish_reason=length  → 被输出上限截断，需拆小参数
		//   - 其他               → JSON 写坏了（常见：带单位的值忘了引号）
		if !argsOK {
			if lastFinish == "length" {
				log.Printf("[agent] stream tool_call %s (idx=%d) TRUNCATED by max_tokens=%d (%d bytes of args) — surfacing to model",
					acc.name, idx, maxTokens, len(raw))
				args = map[string]interface{}{"__arg_error": truncatedArgsError(acc.name, len(raw), maxTokens)}
			} else {
				log.Printf("[agent] stream tool_call %s (idx=%d) args unparseable (%d bytes) — surfacing to model instead of calling with empty args",
					acc.name, idx, len(raw))
				args = map[string]interface{}{"__arg_error": malformedArgsError(acc.name, raw)}
			}
		}
		tc := agentAPI.ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: args,
		}
		resp.ToolCalls = append(resp.ToolCalls, tc)
		delete(accs, idx)
	}

	// flushAll 按 index 升序 flush 所有累积中的 tool call。
	//
	// ❗必须排序，不能直接 `for idx := range accs`：Go 的 map 迭代顺序是**随机化**的，
	// 同一批并行 tool_call 因此会以任意顺序进入 resp.ToolCalls。对 output_send__
	// 这类**用户可见消息**通道，后果是分段消息的到达顺序每次运行都可能不同
	// （不可复现的外部行为）；对 tool_call_id 配对虽无影响（靠 id 而非位置），
	// 但让「同一输入产生同一执行序列」这条最基本的可复现性失守。
	//
	// 排序也让判据可写：stream_flush_order_test 断言输出严格按 index 升序。
	flushAll := func() {
		idxs := make([]int, 0, len(accs))
		for idx := range accs {
			idxs = append(idxs, idx)
		}
		sort.Ints(idxs)
		for _, idx := range idxs {
			flushToolCall(idx)
		}
	}

	for {
		select {
		case ck, ok := <-ch:
			if !ok {
				flushAll()
				if lastFinish != "" {
					resp.FinishReason = lastFinish
				}
				return resp, nil
			}

			if ck.ReasoningContent != "" {
				resp.ReasoningContent += ck.ReasoningContent
				if a != nil {
					a.publishEvent(events.EventReasoningDelta, map[string]interface{}{
						"content": ck.ReasoningContent,
						"channel": channel,
					})
				}
			}
			if ck.Content != "" {
				resp.Content += ck.Content
				if a != nil {
					a.publishEvent(events.EventContentDelta, map[string]interface{}{
						"content": ck.Content,
						"channel": channel,
					})
				}
			}

			// 增量 tool call 分片：OpenAI 风格按 index 字段拼接 id/name/arguments。
			// 注意必须用分片自带的 StreamIndex（上游 JSON "index"），不能用 Go
			// range 序号：每个 SSE chunk 通常只含一个 tool_call 元素，slice 序号
			// 恒为 0，并行多工具调用（index=0,1,2...）的分片会全部污染到同一个桶，
			// 导致 name 相互覆盖、args 碎片混拼解析失败（空参数工具调用）。
			for _, tc := range ck.ToolCalls {
				idx := tc.StreamIndex
				if idx == 0 && tc.Name == "" && tc.RawArguments == "" {
					continue
				}
				acc := accs[idx]
				if acc == nil {
					acc = &toolCallAcc{}
					accs[idx] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Name != "" {
					acc.name = tc.Name
				}
				// arguments 以 JSON 字符串分片到达（OpenAI 标准），拼接后最终解析
				if tc.RawArguments != "" {
					acc.argsRaw.WriteString(tc.RawArguments)
				}
			}

			if ck.Done && ck.FinishReason != "" {
				lastFinish = ck.FinishReason
				if lastFinish == "length" {
					log.Printf("[agent] stream finished with finish_reason=length (output hit max_tokens) — tool args may be truncated")
				}
			}
			if ck.Usage != nil {
				// ★ 合并而非覆盖。
				//
				// 为何是缺陷而不是风格问题：上游会把用量**分帧**给。
				// Anthropic 就是典型 —— input/cache 在 message_start，
				// output_tokens 在 message_delta。若后帧直接覆盖前帧，
				// 最终只剩 completion，prompt 与缓存计数全归零。
				// 而这一切**不在任何地方报错**：只是数字变小了一个量级，
				// 看上去像“这个模型的输入真的很短”。
				//
				// 合并规则：新帧非零的字段赢，零值视为「本帧没携带」而不当“真的为 0”。
				// CacheReported 用或——它一旦为真就不该被后续帧抹掉。
				u := *ck.Usage
				old := resp.TokenUsage
				if u.Prompt == 0 {
					u.Prompt = old.Prompt
				}
				if u.Completion == 0 {
					u.Completion = old.Completion
				}
				if u.Total == 0 {
					u.Total = old.Total
				}
				if u.CacheRead == 0 {
					u.CacheRead = old.CacheRead
				}
				if u.CacheMiss == 0 {
					u.CacheMiss = old.CacheMiss
				}
				if u.ReasoningTokens == 0 {
					u.ReasoningTokens = old.ReasoningTokens
				}
				u.CacheReported = u.CacheReported || old.CacheReported
				resp.TokenUsage = u
			}

		case <-ctx.Done():
			flushAll()
			return resp, ctx.Err()
		}
	}
}

// parseToolArgsJSON 将经过完整拼接的 tool call arguments JSON 字符串解析为 map。
// 第二个返回值 ok=false 表示分片拼接结果不是合法 JSON（分片污染/丢失）。
//
// ❗解析失败时**不要**直接返回空 map 就完事：调用方会拿着空参数去调工具，
// 工具只能报 “xxx is required”这类与真因无关的错（实测 2026-09-19：
// cmd_run 失败率 34%，全部同一个成因）。解析失败时先用 repairToolArgsJSON
// 试着把「一个可选字段写坏」与「整段截断」分开。
func parseToolArgsJSON(s string) (map[string]interface{}, bool) {
	if strings.TrimSpace(s) == "" {
		return map[string]interface{}{}, true
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err == nil && m != nil {
		return m, true
	}
	if repaired, ok := repairToolArgsJSON(s); ok {
		return repaired, true
	}
	return map[string]interface{}{}, false
}

// unitNumberRe 匹配**未加引号的带单位数字**，如 20s / 1m / 500ms。
//
// 这是模型最常见的写法（工具 schema 里 timeout 的示例就是“10s, 1m, 30s”，
// 于是它把值原样写进 JSON，忘了声明里写的是 string 类型）。
var unitNumberRe = regexp.MustCompile(`:\s*(-?\d+(?:\.\d+)?(?:ms|s|m|h|d))\s*([,}])`)

// repairToolArgsJSON 试着修复**单字段值格式错**导致的 JSON 非法。
//
// 为什么值得修而不是直接报错（实测 2026-09-19）：11/11 个真 invalid JSON 都是
// `{"command": "…完好的长命令…", "timeout": 20s}` —— command 一字节没错，
// 只因 timeout 少了引号。旧行为把**整个参数**丢掉，模型看到 “command is required”
// 后只能原样重试，实测 cmd_run 失败率 34%（34 败 / 64 成）。
//
// 修复只做一件很窄的事：给未加引号的带单位数字补上引号。宁可保守也不能猜错：
//   - 只动“值位置”上的 `数字+单位`，且后面紧跟着 `,` 或 `}`
//   - 修完必须真的能解析成功才接受（否则返回 ok=false，行为同旧）
//
// 因此它不会把合法 JSON 改坏，也不会凭空造出字段。
func repairToolArgsJSON(s string) (map[string]interface{}, bool) {
	if !strings.Contains(s, ":") {
		return nil, false
	}
	fixed := unitNumberRe.ReplaceAllString(s, `: "$1"$2`)
	if fixed == s {
		return nil, false
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

func convertToolCalls(tcs []agentAPI.ToolCall) []sdk.ToolCall {
	if tcs == nil {
		return nil
	}
	result := make([]sdk.ToolCall, len(tcs))
	for i, tc := range tcs {
		result[i] = sdk.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	return result
}

func convertBackToolCalls(tcs []sdk.ToolCall) []agentAPI.ToolCall {
	if tcs == nil {
		return nil
	}
	result := make([]agentAPI.ToolCall, len(tcs))
	for i, tc := range tcs {
		result[i] = agentAPI.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments}
	}
	return result
}

func (a *Agent) docStoreSize() int {
	if a.docStore == nil {
		return 0
	}
	s := a.docStore.Stats()
	if n, ok := s["doc_count"]; ok {
		if ni, ok := n.(int); ok {
			return ni
		}
	}
	return 0
}

func (a *Agent) formatMergedTimeline(maxTokens int) string {
	a.context.mu.Lock()
	events := make([]*ContextEvent, len(a.context.events))
	copy(events, a.context.events)
	a.context.mu.Unlock()

	if len(events) == 0 {
		return ""
	}

	// 第一轮：从最新到最旧，计算在预算内能放多少条
	headerTokens := EstimateTokens("【对话时序】\n")
	remaining := maxTokens - headerTokens
	include := 0
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		// ❗单位必须与 EstimateTokens 一致（rune×2）。这里曾用 `len()`（**字节**）再 ×2：
		// CJK 一字 3 字节 ⇒ 中文事件被高估 3 倍，窗口还有余量也会提前 break，
		// 把更早的事件整段丢掉（实测：2384 字的中文事件被估成 14398 token > 8192）。
		estTokens := EstimateTokens(e.Source) + EstimateTokens(e.Input) + 40
		if e.Response != "" {
			estTokens += 120
		}
		if remaining-estTokens < 0 && include > 0 {
			break
		}
		remaining -= estTokens
		include++
	}
	if include == 0 && len(events) > 0 {
		include = 1
	}

	// 第二轮：按时间正序渲染
	start := len(events) - include
	if start < 0 {
		start = 0
	}
	var sb strings.Builder
	// 头部自带语义说明（**块自我描述**）：此前这句解释写在人格/系统提示词里，
	// 提示词一精简就丢语义 ⇒ 模型把历史摘要当成当前待办。说明跟数据贴在一起
	// 才不会漂移。
	sb.WriteString("【对话时序】以下是过去发生的事件摘要（**不是当前任务**；不要把其中内容当成用户此刻的新要求）。\n")
	for _, e := range events[start:] {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s",
			e.Timestamp.Format("15:04:05"), e.Source, e.Input))
		if len(e.ToolsUsed) > 0 {
			sb.WriteString(fmt.Sprintf(" → 调用工具: %s", strings.Join(e.ToolsUsed, ", ")))
		}
		if e.Response != "" {
			sb.WriteString(fmt.Sprintf(" → %s", truncateStr(e.Response, 120)))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (a *Agent) buildMessages(sysPrompt, input string, ctxTokens int) []agentAPI.Message {
	msgs := []agentAPI.Message{{Role: "system", Content: sysPrompt}}

	if ctxTok := a.formatMergedTimeline(ctxTokens); ctxTok != "" {
		msgs = append(msgs, agentAPI.Message{Role: "system", Content: ctxTok})
	}

	msgs = append(msgs, agentAPI.Message{Role: "user", Content: input})
	return msgs
}
