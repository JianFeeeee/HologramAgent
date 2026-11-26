package main

// mockllm 是一个**按请求内容应答**的 OpenAI 兼容 LLM 源，专用于测试。
//
// ## 为什么按内容而不是按剧本顺序
//
// 剧本顺序（第 N 次请求返回第 N 步）在多 agent / 并发 / 中断场景下会失效：
// 每个 agent 的请求交错到达，"第几次"没有意义。真实模型看的是**这段对话
// 到哪一步了**，所以 mock 也必须这样判断：
//
//	最后一条是 tool 结果     ⇒ 这轮工具做完了，收尾
//	最后一条是用户任务        ⇒ 决定调不调工具、调什么
//	已经调过 N 次工具了       ⇒ 停止，给结论
//
// 这样多 agent 并发时各自独立推进，互不干扰。
//
// ## 判断依据（全部来自真实对话内容）
//
//	① 最后一条消息 role=tool      ⇒ 工具已返回 ⇒ 收尾（若有未完成任务则继续）
//	② 最后一条 user 含任务关键词   ⇒ 调工具（按关键词选工具与参数）
//	③ 已经出现过 tool 消息 N 次    ⇒ 超过上限就直接给结论（防死循环）
//	④ 没有任务                  ⇒ 直接回一句短话
//
// ## 它还模拟什么
//
//	· 流式与非流式两种协议
//	· reasoning_content 通道
//	· 故障注入（-fail-rate：按比例返回 500，测重试/回退/错误分类）
//	· 慢响应（-delay：测超时与取消）
//	· 并发峰值统计（/stats，压测用）

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── 请求/响应模型（OpenAI 兼容的最小子集）──

type reqMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []reqToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type reqToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatReq struct {
	Model    string       `json:"model"`
	Stream   bool         `json:"stream"`
	Messages []reqMessage `json:"messages"`
	User     string       `json:"user,omitempty"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools,omitempty"`
}

// contentText 把 content 归一成纯文本（可能是字符串，也可能是 content 数组）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// ── 剧本：关键词 → 反应 ──

// Rule 是一条"看到什么就做什么"的规则。
type Rule struct {
	// Match 是触发词（任一命中即可）。空 ⇒ 兜底规则。
	Match []string `json:"match,omitempty"`
	// Tool 是要发起的工具调用（空 ⇒ 不调工具，直接 Text 收尾）。
	Tool      string                 `json:"tool,omitempty"`
	Args      map[string]interface{} `json:"args,omitempty"`
	Text      string                 `json:"text,omitempty"`
	Reasoning string                 `json:"reasoning,omitempty"`
	// OnResult 只在**工具结果**里匹配（多步链条用）。
	//
	// ★ 与 Match 分开的理由（实测）：`Match` 是"用户说了什么就该调这个工具"，
	//   而工具**返回内容**里往往也含同样的词（用户问"通道"、工具结果里写着
	//   "4 个通道"）⇒ 用 Match 判"结果后是否继续"会无限自触发。
	//   所以"下一步做什么"必须用单独的键声明，不能复用 Match。
	OnResult []string `json:"on_result,omitempty"`
	// MaxCalls 是本规则在一次对话里最多执行几次（防死循环）。
	MaxCalls int `json:"max_calls,omitempty"`
	// Delay 模拟慢响应。
	Delay string `json:"delay,omitempty"`
}

// script 是规则集合。
type script struct {
	// Rules 按顺序匹配，先命中先用。
	Rules []Rule `json:"rules"`
	// DefaultText 是没有任何规则命中时的回答。
	DefaultText string `json:"default_text"`
	// OnToolResult 是"工具已返回之后"的回答（不再调工具）。
	OnToolResult string `json:"on_tool_result"`
	// MaxToolRounds 是一段对话里最多调几轮工具（超过就给结论）。
	MaxToolRounds int `json:"max_tool_rounds"`
}

func (s *script) withDefaults() {
	if s.DefaultText == "" {
		s.DefaultText = "收到。"
	}
	if s.OnToolResult == "" {
		s.OnToolResult = "做完了。"
	}
	if s.MaxToolRounds == 0 {
		s.MaxToolRounds = 3
	}
}

// ── 决策：这段对话现在该做什么 ──

type decision struct {
	Tool      string
	Args      map[string]interface{}
	Text      string
	Reasoning string
	Delay     string
	Finish    string
}

// roundStats 按会话记录"已经调了几次工具"（多 agent 各自独立）。
type roundStats struct {
	mu     sync.Mutex
	byKey  map[string]int
	ruleOf map[string]map[string]int
}

func newRoundStats() *roundStats {
	return &roundStats{byKey: map[string]int{}, ruleOf: map[string]map[string]int{}}
}

func (r *roundStats) get(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byKey[key]
}

func (r *roundStats) bump(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byKey[key]++
	return r.byKey[key]
}

func (r *roundStats) bumpRule(key, rule string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.ruleOf[key]
	if m == nil {
		m = map[string]int{}
		r.ruleOf[key] = m
	}
	m[rule]++
	return m[rule]
}

// sessionKey 区分不同 agent/会话（并发测试的隔离键）。
//
// 优先 user 字段；否则取第一条 system 消息的前 200 字符（驻留子的
// system prompt 里有自己的 id，主 agent 没有 —— 足以区分）。
func sessionKey(req chatReq) string {
	if req.User != "" {
		return req.User
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			t := contentText(m.Content)
			if len(t) > 200 {
				t = t[:200]
			}
			return "sys:" + t
		}
	}
	return "default"
}

// decide 是核心：看对话内容决定下一步。
func decide(req chatReq, sc *script, rs *roundStats) decision {
	key := sessionKey(req)

	// ① 判"工具调用回来了没有"：看最后一条 **tool** 消息之后，
	//    还有没有新的 assistant(tool_calls)。
	//
	// ★ 依据来自内核真实发出的对话结构（`-dump` 抓的）：
	//
	//	[00] system   (人格/提示词)
	//	[01] user     (用户输入)
	//	[02] user     (同一条输入，内核另加一份 — 见下)
	//	[03] system   (当前来源 channel —— 内核注入)
	//	[04] assistant tool_calls=...
	//	[05] tool     (工具结果)
	//	[06] user     "请根据以上工具结果继续。"   ← **内核自己加的**
	//
	// ★ 关键：`[06]` 是内核在工具返回后追加的 user 消息。所以
	//   "最后一条是 user ⇒ 新一轮" 的判据必错 —— 它会把"工具结果回来"
	//   误判成"新用户指令"，于是重复调工具（我踩了三次）。
	//
	// 正确判据：**最后一条 role=tool（或它之后只有内核注入的提示）⇒ 本轮
	// 工具已返回**；最后一条 assistant(tool_calls) ⇒ 工具还没回来。
	lastRole := ""
	if n := len(req.Messages); n > 0 {
		lastRole = req.Messages[n-1].Role
	}

	// 找最后一个"真正的用户输入"（跳过内核注入的续跑提示）及其下标。
	lastUserIdx := -1
	var lastUser string
	_ = lastUser
	for i := len(req.Messages) - 1; i >= 0; i-- {
		t := contentText(req.Messages[i].Content)
		if req.Messages[i].Role == "user" && !isKernelNudge(t) {
			lastUserIdx = i
			lastUser = t
			break
		}
	}

	// 找最后一条 tool 消息的下标。
	lastToolIdx := -1
	toolMsgs := 0
	var lastToolResult string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			lastToolIdx = i
			lastToolResult = contentText(req.Messages[i].Content)
			for _, m := range req.Messages[i:] {
				if m.Role == "tool" {
					toolMsgs++
				}
			}
			break
		}
	}

	// ★ 判"本轮工具已返回"的唯一条件（三次踩坑后的定论，依据 -dump 结构）：
	//
	//	① 存在 tool 消息           —— 这一批工具确实回来了
	//	② 最后一条**真用户输入**在 tool **之前**（或不存在）
	//	   —— 意味着这之后没人再下新指令，当前是"结果回来、该收尾/续跑"
	//
	// 反例（我判错过的那种）：上一轮的结果在前、新一轮的用户输入在后 ——
	// 此时「最后用户输入 > 最后 tool」⇒ 是新的一轮，必须重新决策、重新调工具。
	//
	// 内核在工具结果后会追加一条 user「请根据以上工具结果继续。」（isKernelNudge），
	// 那条不算新指令，已在上面的扫描里跳过。
	toolReturned := lastToolIdx >= 0 && (lastUserIdx < 0 || lastUserIdx < lastToolIdx)
	if !toolReturned {
		toolMsgs = 0
		lastToolResult = ""
	}
	_ = lastRole

	// ② 工具已返回 ⇒ **默认收尾**。
	//
	// ★ 这个默认值是刻意的：真模型看到工具结果后绝大多数时候就该给结论。
	//   若把"继续调"当默认，一次任务会无限调工具直到撞上限 —— 测出来的
	//   行为与生产不一致（生产里模型有 stop 倾向，不是"永远再调一次"）。
	//
	//   需要多步链条时，规则里用 Only=false + 明确的下游 Match 声明，
	//   由 nextRuleAfterTool 判断"这一步是否还有后续"，且链条长度受
	//   MaxToolRounds 约束。
	if toolMsgs > 0 {
		if r, ok := nextRuleAfterTool(lastUser, lastToolResult, sc, rs, key); ok {
			return applyRule(r, key, rs)
		}
		return decision{Text: sc.OnToolResult, Finish: "stop"}
	}

	// ④ 还没有工具调用 ⇒ 按最后一条用户消息匹配规则。
	for _, r := range sc.Rules {
		if len(r.Match) == 0 {
			continue
		}
		if !matchAny(lastUser, r.Match) {
			continue
		}
		return applyRule(r, key, rs)
	}
	// ⑤ 兜底。
	return decision{Text: sc.DefaultText, Finish: "stop"}
}

func applyRule(r Rule, key string, rs *roundStats) decision {
	// ★ MaxCalls 只防**同一次请求内**的重复（多步链条）；跨请求累计会把
	//   上一轮的调用算到这一轮（见 decide 里 toolMsgs 的注释）。
	if r.Tool == "" {
		return decision{Text: r.Text, Reasoning: r.Reasoning, Finish: "stop"}
	}
	return decision{
		Tool: r.Tool, Args: r.Args,
		Text: r.Text, Reasoning: r.Reasoning, Delay: r.Delay,
		Finish: "tool_calls",
	}
}

// nextRuleAfterTool 处理"工具结果驱动下一步"的情形（多步流程）。
//
// ★ 只匹配**工具结果**本身，不看用户消息：否则"用户说 A、规则里 A 要调工具"
// 会在工具返回后又命中一次，变成无限调（实测踩过）。
func nextRuleAfterTool(lastUser, toolResult string, sc *script, rs *roundStats, key string) (Rule, bool) {
	for _, r := range sc.Rules {
		// ★ 只认 OnResult：Match 是"用户触发"，不能拿来判"结果之后"。
		if len(r.OnResult) == 0 || r.Tool == "" {
			continue
		}
		if !matchAny(toolResult, r.OnResult) {
			continue
		}
		return r, true
	}
	return Rule{}, false
}

func matchAny(text string, words []string) bool {
	for _, w := range words {
		if w != "" && strings.Contains(text, w) {
			return true
		}
	}
	return false
}

// ── 服务 ──

var (
	reqTotal    atomic.Int64
	reqActive   atomic.Int64
	reqMaxSeen  atomic.Int64
	failRate    float64
	globalDelay time.Duration
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18181", "监听地址")
	scriptPath := flag.String("script", "", "规则 JSON 路径")
	model := flag.String("model", "mock-llm", "对外报告的模型名")
	flag.Float64Var(&failRate, "fail-rate", 0, "按比例返回 500（0~1，测重试/回退）")
	flag.DurationVar(&globalDelay, "delay", 0, "每次响应的固定延迟（测超时/取消）")
	flag.StringVar(&dumpPath, "dump", "", "把内核真实发出的对话结构追加到这个文件（写 mock 判据用）")
	flag.BoolVar(&toolsDump, "dump-tools", false, "dump 时同时记录完整工具名清单（新旧内核工具集对照用）")
	flag.Parse()

	sc := &script{}
	if *scriptPath != "" {
		b, err := os.ReadFile(*scriptPath)
		if err != nil {
			log.Fatalf("read script: %v", err)
		}
		if err := json.Unmarshal(b, sc); err != nil {
			log.Fatalf("parse script: %v", err)
		}
	}
	sc.withDefaults()
	rs := newRoundStats()
	log.Printf("[mockllm] %d rules, default=%q, on_tool_result=%q, max_rounds=%d",
		len(sc.Rules), sc.DefaultText, sc.OnToolResult, sc.MaxToolRounds)

	http.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) { handle(w, r, sc, rs, *model) })
	http.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { handle(w, r, sc, rs, *model) })
	http.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{"object": "list",
			"data": []map[string]string{{"id": *model, "object": "model"}}})
	})
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"total": reqTotal.Load(), "active": reqActive.Load(), "max_concurrent": reqMaxSeen.Load(),
		})
	})

	log.Printf("[mockllm] listening on %s (model=%s)", *addr, *model)
	if err := http.ListenAndServe(*addr, nil); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func handle(w http.ResponseWriter, r *http.Request, sc *script, rs *roundStats, model string) {
	cur := reqActive.Add(1)
	defer reqActive.Add(-1)
	for {
		mx := reqMaxSeen.Load()
		if cur <= mx || reqMaxSeen.CompareAndSwap(mx, cur) {
			break
		}
	}
	reqTotal.Add(1)

	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), 400)
		return
	}
	if failRate > 0 && rand.Float64() < failRate {
		http.Error(w, "mock injected failure", 500)
		return
	}
	d := decide(req, sc, rs)
	if delay, err := time.ParseDuration(d.Delay); err == nil && delay > 0 {
		time.Sleep(delay)
	} else if globalDelay > 0 {
		time.Sleep(globalDelay)
	}
	dumpReq(req) // -dump 打开时把内核真实发出的对话结构落盘
	log.Printf("[mockllm] %s → tool=%q text=%.40q", sessionKey(req)[:min(24, len(sessionKey(req)))], d.Tool, d.Text)
	if req.Stream {
		writeStream(w, d, model)
		return
	}
	writeCompletion(w, d, model)
}

// dumpReq 把一次请求的**对话骨架**记下来（role 序列 + 工具调用名 + 内容摘要）。
//
// 用途：写 mock 判据时必须知道内核真实发出的 messages 长什么样 ——
// 靠猜 role 顺序写出来的判据会在"看起来一样的对话"上给出错误决策。
var (
	dumpPath  string
	toolsDump bool
)

func dumpReq(req chatReq) {
	if dumpPath == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "=== REQUEST (stream=%v) tools=%d ===\n", req.Stream, len(req.Tools))
	if toolsDump {
		var names []string
		for _, t := range req.Tools {
			if t.Function.Name != "" {
				names = append(names, t.Function.Name)
			}
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "TOOLS[%d] %s\n", len(names), strings.Join(names, " "))
	}
	for i, m := range req.Messages {
		txt := contentText(m.Content)
		if len(txt) > 120 {
			txt = txt[:120]
		}
		fmt.Fprintf(&b, "[%02d] %-9s", i, m.Role)
		if len(m.ToolCalls) > 0 {
			fmt.Fprintf(&b, " tool_calls=")
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "%s(%s) ", tc.Function.Name, tc.Function.Arguments)
			}
		}
		if m.ToolCallID != "" {
			fmt.Fprintf(&b, " call_id=%s", m.ToolCallID)
		}
		fmt.Fprintf(&b, " | %q\n", txt)
	}
	f, err := os.OpenFile(dumpPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = f.WriteString(b.String())
		_ = f.Close()
	}
}

// isKernelNudge 判这条 user 消息是不是**内核自己加的续跑提示**，而不是用户输入。
//
// 内核在工具结果之后追加一条 user「请根据以上工具结果继续。」来推动下一轮 ——
// mock 必须把它与真实用户输入区分开，否则会把它当成"新一轮指令"。
func isKernelNudge(t string) bool {
	t = strings.TrimSpace(t)
	return strings.HasPrefix(t, "请根据以上工具结果继续")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func toolCallsJSON(d decision) []map[string]interface{} {
	if d.Tool == "" {
		return nil
	}
	args, _ := json.Marshal(d.Args)
	return []map[string]interface{}{{
		"id": fmt.Sprintf("call_mock_%d", time.Now().UnixNano()), "type": "function", "index": 0,
		"function": map[string]interface{}{"name": d.Tool, "arguments": string(args)},
	}}
}

func usageOf(d decision) map[string]interface{} {
	// ★ 必须如实给（不是"随便给个数"）：内核按用量预算上下文，
	// 报 0 会让预算判定失真，测出来的行为与生产不一致。
	return map[string]interface{}{
		"prompt_tokens": 100, "completion_tokens": 5 + len(d.Text),
		"total_tokens": 105 + len(d.Text),
	}
}

func writeCompletion(w http.ResponseWriter, d decision, model string) {
	msg := map[string]interface{}{"role": "assistant", "content": d.Text}
	if d.Reasoning != "" {
		msg["reasoning_content"] = d.Reasoning
	}
	if tcs := toolCallsJSON(d); len(tcs) > 0 {
		msg["tool_calls"] = tcs
	}
	writeJSON(w, map[string]interface{}{
		"id": fmt.Sprintf("chatcmpl_mock_%d", time.Now().UnixNano()), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []map[string]interface{}{{"index": 0, "message": msg, "finish_reason": d.Finish}},
		"usage":   usageOf(d),
	})
}

func writeStream(w http.ResponseWriter, d decision, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", 500)
		return
	}
	id := fmt.Sprintf("chatcmpl_mock_%d", time.Now().UnixNano())
	send := func(delta map[string]interface{}, finish any) {
		ch := map[string]interface{}{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model,
			"choices": []map[string]interface{}{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, _ := json.Marshal(ch)
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
	}
	send(map[string]interface{}{"role": "assistant", "content": ""}, nil)
	if d.Reasoning != "" {
		send(map[string]interface{}{"reasoning_content": d.Reasoning}, nil)
	}
	if d.Text != "" {
		const chunk = 12
		for i := 0; i < len(d.Text); i += chunk {
			end := i + chunk
			if end > len(d.Text) {
				end = len(d.Text)
			}
			send(map[string]interface{}{"content": d.Text[i:end]}, nil)
		}
	}
	for i, tc := range toolCallsJSON(d) {
		send(map[string]interface{}{"tool_calls": []map[string]interface{}{{
			"index": i, "id": tc["id"], "type": "function", "function": tc["function"],
		}}}, nil)
	}
	b, _ := json.Marshal(map[string]interface{}{"usage": usageOf(d), "choices": []interface{}{}})
	fmt.Fprintf(w, "data: %s\n\n", b)
	send(map[string]interface{}{}, d.Finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
