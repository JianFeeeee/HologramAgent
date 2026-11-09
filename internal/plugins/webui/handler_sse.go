package webui

import (
	"fmt"
	"log"
	"sync"
	"time"

	"encoding/json"
)

// SSE 环形缓冲：/api/v1/chat/events 断线重连时按 Last-Event-ID 补发。

// sseEventRecord 保存一条 SSE 事件元数据，供断线重连时按 Last-Event-ID 重放遗漏事件。
type sseEventRecord struct {
	id        string          // SSE 事件 id 值（如 "1234567890-5"）
	eventType string          // 事件类型（agent_output, reasoning 等）
	data      json.RawMessage // 序列化后的 payload JSON
}

// sseEventRing 是一个固定大小的环状缓冲区，保持最近 cap 条 SSE 事件。

type sseEventRing struct {
	mu  sync.Mutex
	buf []sseEventRecord
	cap int
}

func newSSEEventRing(cap int) *sseEventRing {
	return &sseEventRing{cap: cap}
}

// Append 追加一条事件，超过容量时丢弃最旧条目。
func (r *sseEventRing) Append(id, eventType string, data json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, sseEventRecord{id: id, eventType: eventType, data: data})
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

// After 返回所有在指定 id 之后的事件（按写入顺序），若 id 不在缓冲区中则返回全部。
func (r *sseEventRing) After(id string) []sseEventRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.buf) - 1; i >= 0; i-- {
		if r.buf[i].id == id {
			result := make([]sseEventRecord, len(r.buf)-i-1)
			copy(result, r.buf[i+1:])
			return result
		}
	}
	// ID 不在缓冲区（可能是太旧或从未收到），返回全部
	result := make([]sseEventRecord, len(r.buf))
	copy(result, r.buf)
	return result
}

// sendSSE 向 writeCh 发送一条 SSE 事件；队列满时等 100ms 再试，
// 比立即 drop 更友好，避免密集 tool_call/delta 期间前端丢帧。
func sendSSE(writeCh chan string, id, eventType, data string) {
	line := fmt.Sprintf("id: %s\nevent: %s\ndata: %s\n", id, eventType, data)
	select {
	case writeCh <- line:
		return
	default:
	}
	// 队列满：等 100ms 让 writer flush，再试一次
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case writeCh <- line:
	case <-timer.C:
		log.Printf("[SSE] DROPPED %s id=%s (writeCh full 100ms, len=%d)", eventType, id, len(writeCh))
	}
}
