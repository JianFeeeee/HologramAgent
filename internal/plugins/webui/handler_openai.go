package webui

import (
	"context"
	"fmt"
	"time"

	"encoding/json"
	"net/http"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// OpenAI 兼容面：/v1/chat/completions（含流式）。

func (h *Handler) handleOpenAICompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if h.sdk == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "IO manager not available"})
		return
	}

	var req struct {
		Model       string          `json:"model"`
		Messages    []openAIMessage `json:"messages"`
		Stream      bool            `json:"stream"`
		Temperature float64         `json:"temperature"`
		MaxTokens   int             `json:"max_tokens"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "messages is required"})
		return
	}

	lastMsg := req.Messages[len(req.Messages)-1]
	if lastMsg.Role != "user" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "last message must be from user"})
		return
	}

	// 带超时的上下文（同 handleChat：客户端断开立即取消；300s 约束长生成）
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	// ★ 流式与非流式是**两条不同的实现路径**，不能共用「先同步等完再分帧」。
	//
	// 原实现两条路都走 InjectTextSyncNoMemory（同步等完整回复），流式只是
	// 把已拼好的全文切成 3 个 chunk 吐出去 —— 实测首字节 7.79s、随后
	// 整段到达，客户端的「生成中」/取消/进度条全部失效。
	//
	// 真流式必须**订阅增量事件边收边转**：先写 SSE 头（把响应状态锁定为
	// 200，之后再出错也无法改状态码），随后把 EventContentDelta 逐块转成
	// chunk，最后用同步调用拿到的完整回复收尾（补 usage、发 finish）。
	if req.Stream {
		h.streamOpenAI(ctx, w, req.Model, lastMsg.Content)
		return
	}

	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		// NoMemory：本端点（OpenAI 兼容 /v1/chat/completions）的调用方是
		// IDE、工具与脚本，送来的是**固定的提示词模板**（"请分析这段代码"之类），
		// 不是人类在对话。记进记忆会把真实对话挤掉，而且同一模板会反复刷屏。
		// 原文仍进上下文，模型照旧看得到；只是不参与向量化/关键词提取/蒸馏。
		respCh <- h.sdk.InjectTextSyncNoMemory("http", "http", lastMsg.Content)
	}()

	var response *agentIO.OutputEvent
	select {
	case response = <-respCh:
	case <-ctx.Done():
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "agent timeout (300s)"})
		return
	}

	if response == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no response from agent"})
		return
	}

	content, _ := response.Payload["content"].(string)
	reasoningContent, _ := response.Payload["reasoning_content"].(string)
	usage, _ := response.Payload["usage"].(map[string]interface{})

	resp := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"message": map[string]interface{}{
					"role":    "assistant",
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
	}
	if reasoningContent != "" {
		resp["choices"].([]map[string]interface{})[0]["message"].(map[string]interface{})["reasoning_content"] = reasoningContent
	}
	if usage != nil {
		resp["usage"] = usage
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

// streamOpenAI 是 /v1/chat/completions 的真流式实现。
//
// 结构与 handleSSE 一致：订阅增量 → 收集 → 写 writer goroutine → 收尾。
// 关键差异：SSE 端点是在「最终回复已就绪」后才开始收尾，本端点必须**边收边发**。
func (h *Handler) streamOpenAI(ctx context.Context, w http.ResponseWriter, model, userText string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}

	// 先锁状态码：SSE 一旦写出第一个字节，之后上游再出错也只能以
	// 「在流里报错」的方式告知（data: {"error":...}），无法改成 4xx/5xx。
	// 这是所有 SSE 实现的固有限制，写明以免后人误以为能改。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关掉 nginx/frp 的缓冲，否则流式白做
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	completionID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// delta 收集通道。缓冲 2048 与 handleSSE 同理：LLM token 级高频小包，
	// 缓冲过小会溢出丢帧。
	deltaCh := make(chan deltaItem, 2048)
	done := make(chan struct{})

	// 写 goroutine：批量合并（16ms 窗口）后 flush，降 syscall 次数。
	// 不 close(deltaCh) —— 订阅回调可能在 handler 返回后仍被总线异步触发，
	// close 后再发会 panic（生产日志里出现过单日数千次）。
	//
	// ★★ 并发契约：本函数内 w 只有一个写者 = 下面这个 writer goroutine。
	// 此前 handler goroutine 在收尾处直写 w（finish chunk/[DONE]/Flush），
	// 与这里的写 goroutine 并发写同一个 http.ResponseWriter —— gzip 中间件
	// 的 mode/wroteHeader/buf 被两个 goroutine 同时读写，-race 单跑即可
	// 稳定复现 6~8 条 DATA RACE。修法选「单写者模型」而非给
	// gzipResponseWriter 加锁：加锁只哄住 race detector（治标），且持锁跨
	// 网络 Write/Flush 会把整条 SSE 卡在一个串行点上；「同一时刻只有一个
	// goroutine 写 w」才是 SSE 帧完整性的正确约束。于是终止帧也必须交给
	// 这个唯一的写者：见 finishCh / sendFinish。
	finishCh := make(chan finishItem, 1)
	writerExit := make(chan struct{})
	// 用 safeclose 而不是裸 close：本仓 close 纪律测试
	// （internal/safeclose/TestNoBareCloseOutsideSafeclose）禁止裸 close(chan)，
	// 理由是「今天只关一次的 channel，明天多一条退出路径就变成关两次 ⇒ panic」。
	var writerExitOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// 终止帧全部写完（含 Flush）后才关闭，handler 侧据此确认已出网。
		defer safeclose.Safe(&writerExitOnce, writerExit)
		pending := make([]string, 0, 64)
		flushPending := func() {
			if len(pending) == 0 {
				return
			}
			for _, line := range pending {
				fmt.Fprintf(w, "%s\n", line)
			}
			flusher.Flush()
			pending = pending[:0]
		}
		flushTicker := time.NewTicker(16 * time.Millisecond)
		defer flushTicker.Stop()
		for {
			select {
			case d := <-deltaCh:
				if d.reset {
					pending = pending[:0]
				}
				if d.content != "" {
					pending = append(pending, openAIChunkLine(completionID, created, model,
						map[string]interface{}{"content": d.content}, nil, nil))
				}
				if d.reasoning != "" {
					pending = append(pending, openAIChunkLine(completionID, created, model,
						map[string]interface{}{"content": "", "reasoning_content": d.reasoning}, nil, nil))
				}
				if len(pending) >= 128 {
					flushPending()
				}
			case <-flushTicker.C:
				flushPending()
			case f := <-finishCh:
				// 先把攒着的尾部 delta 发完，再发终止帧 —— 顺序不能反，
				// 否则客户端会先看到 [DONE] 再看到内容（旧写法的缺陷）。
				flushPending()
				if f.timedOut {
					fmt.Fprintf(w, "data: %s\n\n", `{"error":{"message":"agent timeout (300s)"}}`)
				} else {
					fmt.Fprintf(w, "%s\n", openAIChunkLine(completionID, created, model,
						map[string]interface{}{}, "stop", f.usage))
				}
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			case <-done:
				flushPending()
				return
			}
		}
	}()

	// sendFinish 把「终止」交给唯一写者，并等它把终止帧真正写完（含 Flush）。
	// 此后 handler goroutine 不再碰 w 一个字节。
	//
	// 必须**先发再等**：writer goroutine 只在「收到 finishCh」或「done 关闭」
	// 时退出，而 done 的关闭在 defer 里（本函数返回之后）——先等退出后发
	// 终止就是死锁。select 的 <-done 是防御：done 已关时 writer 可能已不在，
	// 此时不能再等一个永不到来的接收者。
	sendFinish := func(usage map[string]interface{}, timedOut bool) {
		select {
		case finishCh <- finishItem{usage: usage, timedOut: timedOut}:
		case <-done:
		}
		<-writerExit
	}

	send := func(d deltaItem) {
		select {
		case deltaCh <- d:
		case <-done:
		}
	}

	// 订阅增量事件。必须在启动注入**之前**订阅，否则会漏掉开头几个分片。
	var unsubs []func()
	deltaSub := func(typ sdk.EventType, pick func(payload map[string]interface{}) (string, bool)) {
		unsub := h.sdk.Subscribe(typ, func(evt *sdk.Event) {
			text, ok := pick(evt.Payload)
			if !ok {
				return
			}
			send(deltaItem{content: text, reset: isResetMarked(evt.Payload)})
		})
		unsubs = append(unsubs, unsub)
	}
	deltaSub(sdk.EventContentDelta, func(p map[string]interface{}) (string, bool) {
		v, _ := p["content"].(string)
		return v, v != ""
	})
	deltaSub(sdk.EventReasoningDelta, func(p map[string]interface{}) (string, bool) {
		v, _ := p["reasoning_content"].(string)
		return v, v != ""
	})
	var doneOnce sync.Once
	defer func() {
		safeclose.Safe(&doneOnce, done)
		wg.Wait()
		for _, u := range unsubs {
			u()
		}
	}()

	// 同步拿最终回复：用于补 usage、发 finish chunk。
	// 此时增量早已转发完毕，所以这次等待不会拖慢首字节。
	respCh := make(chan *agentIO.OutputEvent, 1)
	go func() {
		respCh <- h.sdk.InjectTextSyncNoMemory("http", "http", userText)
	}()
	var response *agentIO.OutputEvent
	select {
	case response = <-respCh:
	case <-ctx.Done():
		// 超时也走同一个唯一写者出口：在这里直写 w 正是竞争的另一半。
		sendFinish(nil, true)
		return
	}

	var usage map[string]interface{}
	if response != nil {
		usage, _ = response.Payload["usage"].(map[string]interface{})
	}

	// finish chunk + [DONE]：交给唯一写者发。
	sendFinish(usage, false)
}

// openAIChunkLine 把一个 chunk 序列化成一行 SSE。
func openAIChunkLine(id string, created int64, model string, delta map[string]interface{}, finishReason interface{}, usage map[string]interface{}) string {
	choice := map[string]interface{}{
		"index": 0,
		"delta": delta,
	}
	if finishReason != nil {
		choice["finish_reason"] = finishReason
	} else {
		choice["finish_reason"] = nil
	}
	chunk := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]interface{}{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	data, _ := json.Marshal(chunk)
	return "data: " + string(data)
}

// finishItem 是流终止消息：由 handler goroutine 投递、由唯一的 writer
// goroutine 消费并写出 finish chunk 与 [DONE]。
type finishItem struct {
	// usage 进 finish chunk（OpenAI 协议里 usage 只在收尾帧出现）。
	usage map[string]interface{}
	// timedOut 为真表示走 ctx 超时收尾（在流里报错而不是发 stop）。
	timedOut bool
}

type deltaItem struct {
	content   string
	reasoning string
	// reset 为真表示上游要求清空已显示内容（流式作废回退），前端需要丢弃累积。
	reset bool
}

// isResetMarked 判定增量事件是否要求重置。
//
// 内核在「流式失败、回退到非流式」时会发一个 content="" + reset=true 的
// 事件（见 internal/agent/core/process.go）。若忽略它，客户端会看到
// 半截内容后又接上完整内容（重复且自相矛盾）。
func isResetMarked(payload map[string]interface{}) bool {
	reset, _ := payload["reset"].(bool)
	return reset
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// handleOpenAIModels 实现 GET /v1/models。
//
// 为什么本端点「返回什么模型」并不重要，重要的是**结构合法**：
// OpenAI 客户端把它当作能力探测（probe）。请求方要的不是模型清单本身，
// 而是一个「这个端点讲 OpenAI 协议」的确认。
//
// 所以这里返回 HomeAgent 自身作为唯一条目：调用方无论填哪个 id 都会
// 被路由到同一个 agent（本端点的 model 参数确实只是透传）。
// 不谎报 GPT 之类的名字 —— 那会让用户以为能选模型，而实际不能。
func (h *Handler) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := openAIModelID()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data": []map[string]interface{}{
			{
				"id":       id,
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "homeagent",
			},
		},
	})
}

// openAIModelID 返回对外暴露的模型标识。
//
// 与 /v1/chat/completions 接受任意 model 值保持一致：本端点不做模型选择，
// 请求里的 model 只是回显。给一个稳定的名字，便于客户端写死在配置里。
func openAIModelID() string { return "homeagent" }
