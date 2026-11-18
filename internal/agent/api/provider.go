package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// ContentBlock 定义多模态内容块，用于图片/音频等非文本输入。
type ContentBlock struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	AudioURL *AudioURL `json:"audio_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type AudioURL struct {
	URL string `json:"url"`
}

// MarshalJSON 把音频块序列化成 OpenAI「input_audio」多模态格式（base64 内嵌），
// 供支持音频的模型识别。audio_url 非 OpenAI 标准块；含 base64 数据时转
// input_audio，否则回落到原生 audio_url（透传）。
func (b ContentBlock) MarshalJSON() ([]byte, error) {
	if b.Type == "audio_url" && b.AudioURL != nil && b.AudioURL.URL != "" {
		if data, format, ok := parseAudioDataURL(b.AudioURL.URL); ok {
			return json.Marshal(map[string]interface{}{
				"type": "input_audio",
				"input_audio": map[string]string{
					"data":   data,
					"format": format,
				},
			})
		}
	}
	type alias ContentBlock
	return json.Marshal(alias(b))
}

// parseAudioDataURL 从 data:<mime>;base64,<data> 提取 base64 与 format。
// 非 base64（如 http url）返回 ok=false。
func parseAudioDataURL(url string) (data, format string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	mime := rest[:comma]
	data = rest[comma+1:]
	if mime == "" || data == "" {
		return "", "", false
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", "", false
	}
	format = audioFormatFromMIME(mime)
	return data, format, true
}

func audioFormatFromMIME(mime string) string {
	m := strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.Contains(m, "wav"):
		return "wav"
	case strings.Contains(m, "mp3"), strings.Contains(m, "mpeg"):
		return "mp3"
	case strings.Contains(m, "mp4"), strings.Contains(m, "m4a"):
		return "mp4"
	case strings.Contains(m, "ogg"), strings.Contains(m, "opus"):
		return "ogg"
	case strings.Contains(m, "flac"):
		return "flac"
	default:
		return "wav"
	}
}

// Message 表示对话消息。当 Blocks 不为空时 content 在 JSON 中序列化为数组（多模态格式）。
type Message struct {
	Role             string         `json:"role"`
	Content          string         `json:"content,omitempty"`
	Blocks           []ContentBlock `json:"-"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []ToolCall     `json:"-"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	raw := map[string]interface{}{
		"role": m.Role,
	}
	if len(m.Blocks) > 0 {
		raw["content"] = m.Blocks
	} else if m.Content != "" || len(m.ToolCalls) == 0 {
		raw["content"] = m.Content
	} else {
		raw["content"] = nil
	}
	if m.ReasoningContent != "" {
		raw["reasoning_content"] = m.ReasoningContent
	}
	if m.ToolCallID != "" {
		raw["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		apiTCs := make([]apiToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			argsBytes, _ := json.Marshal(tc.Arguments)
			apiTCs[i] = apiToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: apiFunction{
					Name:      tc.Name,
					Arguments: string(argsBytes),
				},
			}
		}
		raw["tool_calls"] = apiTCs
	}
	return json.Marshal(raw)
}

type CompletionRequest struct {
	Model           string                 `json:"model,omitempty"`
	Messages        []Message              `json:"messages"`
	Temperature     float64                `json:"temperature,omitempty"`
	MaxTokens       int                    `json:"max_tokens,omitempty"`
	Stream          bool                   `json:"stream,omitempty"`
	Tools           []interface{}          `json:"tools,omitempty"`
	ToolChoice      interface{}            `json:"tool_choice,omitempty"`
	DisableThinking bool                   `json:"disable_thinking"`
	ExtraBody       map[string]interface{} `json:"-"`
}

func (r *CompletionRequest) MarshalJSON() ([]byte, error) {
	type Alias CompletionRequest
	data, err := json.Marshal((*Alias)(r))
	if err != nil {
		return nil, err
	}
	if len(r.ExtraBody) == 0 {
		return data, nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	for k, v := range r.ExtraBody {
		raw[k] = v
	}
	return json.Marshal(raw)
}

type CompletionResponse struct {
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	FinishReason     string     `json:"finish_reason,omitempty"`
	TokenUsage       TokenUsage `json:"token_usage,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

type TokenUsage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`

	// ── 缓存与推理归因（详见 tokenusage_cache_test.go 的说明）──
	//
	// 参考 llmsproxy 的 internal/types.TokenUsage 与
	// internal/gateway/chat.go:recordChatUsage：那边已把
	// 「OpenAI v2 的 prompt_tokens_details.cached_tokens」与
	// 「DeepSeek 遗留的 prompt_cache_hit_tokens」两条来源归一化好了，
	// 本结构沿用同一套语义，免得两个项目对同一份上游数据给出不同答案。

	// CacheRead 是命中缓存（跳过计算的）输入 token 数。
	CacheRead int `json:"cache_read,omitempty"`
	// CacheMiss 是未命中的输入 token 数（上游只给其一时另一侧留 0）。
	CacheMiss int `json:"cache_miss,omitempty"`
	// CacheReported 区分「上游报了缓存但命中为 0」与「上游根本没报缓存」。
	//
	// 为何必须分开：混为一谈会把「无数据」显示成 0% 命中率，
	// 让人去优化一个本来就没开的功能 —— 那是拿假数据做的决定。
	CacheReported bool `json:"cache_reported,omitempty"`
	// ReasoningTokens 是计费输出里属于「思考」的那部分。
	//
	// 为何要单独拎出来：缺了它，成本归因会把思考 token 算进「回答长度」，
	// 于是长思考被误读成啰嗦。
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Add 把另一次调用的用量并入本结构（求和）。
//
// 用途：一次用户请求（一个 TaskFrame）可能触发多轮 LLM 调用（工具回环），
// 对外的 usage 需要报**本次请求**的合计，与 OpenAI 的语义一致；
// 会话级累计另由 usageLedger 承担（链事件里的 usage_session）。
func (t *TokenUsage) Add(u TokenUsage) {
	t.Prompt += u.Prompt
	t.Completion += u.Completion
	t.Total += u.Total
	t.CacheRead += u.CacheRead
	t.CacheMiss += u.CacheMiss
	t.ReasoningTokens += u.ReasoningTokens
	// CacheReported 是「上游报过缓存」的标记，用或而非加：
	// 一轮里只要有一帧报了，本轮就算有缓存数据可算。
	t.CacheReported = t.CacheReported || u.CacheReported
}

// IsZero 报告该用量是否**完全没有数据**。
//
// 调用方据此决定「不报 usage」而不是「报 0」：把「不知道」画成 0
// 会让人去优化一个本来就没开的功能（与 UsageTotals.CacheHitRate 的
// ok=false 同一口径）。
func (t TokenUsage) IsZero() bool {
	return t.Prompt == 0 && t.Completion == 0 && t.Total == 0 &&
		t.CacheRead == 0 && t.CacheMiss == 0 && t.ReasoningTokens == 0
}

// DeriveCacheMiss 在**上游只报了命中侧**时补出未命中输入数。
//
// 这是「缓存命中率」这条规则的**唯一实现**（见文件头"单一实现"说明）：
// Go 侧的 chunkAssemble 与 Lua 适配器结果的落地处都调它，
// 不允许任何一方自己再算一遍 —— 两套实现迟早会漂移，
// 而漂移的表现是「同一份上游数据，配不配适配器给出不同命中率」。
//
// 为何必须补：OpenAI v2 只在 `prompt_tokens_details.cached_tokens` 里
// 给**命中侧**，不给未命中数。留 0 的后果不是"少一点"，而是：
// 命中率 = CacheRead/(CacheRead+0) = **恒 100%**。
// 2026-09-30 跑分实测（llmsproxy + AUTO，7 个任务）就报出了 100%，
// 而真实值约 53% —— 结构性假绿，且不会让任何地方报错。
//
// 补的依据是 `Prompt`（上游给的输入总数，权威）：
// 未命中输入 = 输入总数 - 命中数。DeepSeek 那种**两边都给**的上游
// （prompt_cache_hit_tokens + prompt_cache_miss_tokens）CacheMiss != 0，
// 直接不动 —— 上游明说的值永远优先于我们推的。
//
// 边界：
//   - 未报缓存（CacheReported=false）⇒ 不动，让消费方显示「—」；
//   - 报了但命中 0 ⇒ miss = prompt（全部未命中）。这是**有数据**的 0%，
//     与「不知道」不同，正是 CacheReported 存在的意义；
//   - 上游给的命中数大于输入总数（脏数据）⇒ 夹到 0，不让 miss 变负。
func (t *TokenUsage) DeriveCacheMiss() {
	if !t.CacheReported || t.CacheMiss != 0 || t.Prompt <= 0 {
		return
	}
	miss := t.Prompt - t.CacheRead
	if miss < 0 {
		miss = 0
	}
	t.CacheMiss = miss
}

type ToolCall struct {
	ID           string                 `json:"id"`
	Type         string                 `json:"type"`
	Name         string                 `json:"name"`
	Arguments    map[string]interface{} `json:"arguments"`
	RawArguments string                 `json:"raw_arguments,omitempty"` // 流式分片原始 JSON 字符串
	// StreamIndex 是上游流式 tool_call 的 OpenAI index 字段（并行多工具调用
	// 时同一轮的分片用它区分归属）。lua 适配器以 stream_index 键透传；
	// 仅内核流式累积内部使用，不序列化到对外 API。
	StreamIndex int `json:"stream_index,omitempty"`
}

type apiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type StreamChunk struct {
	Content          string      `json:"content"`
	ReasoningContent string      `json:"reasoning_content,omitempty"`
	Done             bool        `json:"done"`
	FinishReason     string      `json:"finish_reason,omitempty"`
	ToolCall         *ToolCall   `json:"tool_call,omitempty"`
	ToolCalls        []ToolCall  `json:"tool_calls,omitempty"`
	Usage            *TokenUsage `json:"usage,omitempty"`
}

type Provider interface {
	Name() string
	Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
	ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
	MaxContextTokens() int
}

// RoutableProvider 是支持精确模型路由/AUTO 优先级的 provider。
// 不与 Provider 强绑定，避免破坏第三方 Provider 实现。
type RoutableProvider interface {
	Provider
	Model() string // 该 provider 提供的模型名（可能为空表示 AUTO）
	Priority() int // AUTO 跨源选择的优先级，大者优先
}

// ModalProvider 声明自身的多模态能力。单独抽接口而不合进 Provider：
// 第三方 Provider 实现无需改动，未实现时按纯文本处理（保守侧）。
type ModalProvider interface {
	SupportsVision() bool
	SupportsAudio() bool
}

// ProviderSupportsVision 安全判定任意 Provider 能否看图。
// 未实现 ModalProvider 的一律返回 false：宁可多走一次文字回退，
// 也不能把图默默扔给一个会把它剥掉的上游。
func ProviderSupportsVision(p Provider) bool {
	if mp, ok := p.(ModalProvider); ok {
		return mp.SupportsVision()
	}
	return false
}

// ProviderSupportsAudio 安全判定任意 Provider 能否听音频。
func ProviderSupportsAudio(p Provider) bool {
	if mp, ok := p.(ModalProvider); ok {
		return mp.SupportsAudio()
	}
	return false
}

// ErrKind 是 provider 失败的**类别**（决定上层处置：重试 / 裁剪 / 换源）。
type ErrKind uint8

const (
	ErrUnknown ErrKind = iota
	ErrCredential
	ErrContextFull
	ErrTransient
)

// ProviderError 是带 HTTP 状态与类别的 provider 错误。
type ProviderError struct {
	StatusCode int
	Message    string
	Kind       ErrKind
}

func (e *ProviderError) Error() string { return e.Message }

// ToolOutput 是工具 handler 返回的结构化结果（支持多模态块）。
// 返回 string 时等价于 ToolOutput{Text: result}。
type ToolOutput struct {
	Text   string         `json:"text"`
	Blocks []ContentBlock `json:"blocks,omitempty"`
}

func (t ToolOutput) String() string { return t.Text }

// IsContextFull 报告一个 provider 错误是否表示「上下文装不下」。
//
// ## 归属：识别在上游调用的发出者，处置在类型
//
// `ErrContextFull` 是 provider 回给**发请求那一方**的事实。判定本身是纯协议
// 事实（错误码 → 类别），与任何 agent 类型无关，所以它住在 `api` 这个叶子包，
// 由内核的 LLM 设施与基座共用同一份实现 —— 不允许两处各写一遍。
//
// 处置（裁/剔页/抢占/终止/上报父/什么都不做）一律由该类型注册的
// `OverflowHandler` 决定。
func IsContextFull(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Kind == ErrContextFull
}
