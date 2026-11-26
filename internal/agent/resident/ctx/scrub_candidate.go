package ctx

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi"

// ScrubMemoryCandidate 清洗记忆候选载荷（不经 context 的那条侧路）。
//
// ★ 变量库**由调用方传入**（原来是 `a.SecretStore()`）：本包不认识 Agent，
//
//	所以它需要一个显式入参。调用点（`agent/distill.go`）传 `a.SecretStore()`。
//
// 就地改写 map：调用方传进来的是一次性的快照，改它没有副作用。
// ★ 2026-11-04：从包级自由函数改为 Agent 方法（变量库走 a.SecretStore()）。
func ScrubMemoryCandidate(payload map[string]interface{}, s kernelapi.SecretStore) {
	if payload == nil {
		return
	}
	if s == nil {
		return
	}
	for _, key := range []string{"input", "response"} {
		str, ok := payload[key].(string)
		if !ok || str == "" {
			continue
		}
		if out, evts := s.Replace(str, "memory:"+key, nil); len(evts) > 0 {
			payload[key] = out
			logEvtHits("记忆候选 "+key, evts)
		}
	}
	switch trs := payload["tool_results"].(type) {
	case []ToolResultItem:
		for i := range trs {
			if trs[i].Output == "" {
				continue
			}
			if out, evts := s.Replace(trs[i].Output,
				"memory:tool:"+trs[i].Name, nil); len(evts) > 0 {
				trs[i].Output = out
				logEvtHits("记忆候选工具结果 "+trs[i].Name, evts)
			}
		}
		payload["tool_results"] = trs
	case []interface{}:
		// 有的调用方传的是未类型化的切片（如经 JSON 中转）。
		for i := range trs {
			m, ok := trs[i].(map[string]interface{})
			if !ok {
				continue
			}
			out, ok := m["output"].(string)
			if !ok || out == "" {
				continue
			}
			if scrubbed, evts := s.Replace(out, "memory:tool", nil); len(evts) > 0 {
				m["output"] = scrubbed
				logEvtHits("记忆候选工具结果", evts)
			}
		}
	}
}
