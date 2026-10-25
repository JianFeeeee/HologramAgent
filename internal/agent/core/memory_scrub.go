package core

// 持久化侧路的明文拦截（「记忆防火墙」）。
//
// ## 为什么必须单独拦（而不是只靠 provider 装饰器）
//
// 出向拦截（`api/provider.go` 的装饰器）只改 `f.Msgs` —— 那是**要发出去的
// 那一份**。但内核把同一批内容**另存副本**送往持久化存储，那些副本
// 不经过 provider：
//
//	ContextEvent.Input        ← f.CleanInput      （用户输入原文）
//	ContextEvent.Response     ← f.Response        （模型回复）
//	ContextEvent.ToolResults  ← f.ToolResults     （工具输出）
//	emitMemoryCandidate.payload.tool_results      ← 同上，走 EmitOutput 不经 context
//
// 实测确认（变异实验 C6-M1 / C6-M2 / 输入侧探针）：
// 只拦 `f.Msgs` 时，明文会完整地写进 `context.json` **与长期记忆**，
// 而后者跨会话保留、每轮被召回 —— 泄漏面比"重放一次"大得多。
//
// ## 为什么放在 Append/InsertByTimestamp 而不是各调用点
//
// 写 context 的地方有 5 处（task.go 3 处、toolcall.go 1 处，以及插入变体）。
// 逐个补必然漏 —— 本文件的前身就是这么漏掉 `f.CleanInput` 的。
// `Append` / `InsertByTimestamp` 是全部写入的唯一咽喉，拦这里一处即全覆盖。
//
// `emitMemoryCandidate` 不经 context（走 `EmitOutput`），所以单独处理。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// scrubEventSecrets 就地清洗一条待持久化事件里的明文。
//
// 失败策略：Store 未装配（未启用安全模块）时**原样返回**，零成本。
func scrubEventSecrets(evt *ContextEvent) {
	if evt == nil {
		return
	}
	s := security.Global()
	if s == nil {
		return
	}
	if out, evts := s.Replace(evt.Input, "ctx_input", security.Patterns); len(evts) > 0 {
		evt.Input = out
		logEvtHits("上下文输入", evts)
	}
	if out, evts := s.Replace(evt.Response, "ctx_response", security.Patterns); len(evts) > 0 {
		evt.Response = out
		logEvtHits("上下文回复", evts)
	}
	for i := range evt.ToolResults {
		if evt.ToolResults[i].Output == "" {
			continue
		}
		if out, evts := s.Replace(evt.ToolResults[i].Output,
			"ctx_tool:"+evt.ToolResults[i].Name, security.Patterns); len(evts) > 0 {
			evt.ToolResults[i].Output = out
			logEvtHits("上下文工具结果 "+evt.ToolResults[i].Name, evts)
		}
	}
	// 一等记忆块：**文本**块同样可能带凭据（截图描述、文档正文、转写稿）。
	// 媒体块（image/video/audio 的 Data）不处理 —— 那是二进制负载，
	// 正则扫它既无意义也会损坏数据。
	for i := range evt.Blocks {
		if evt.Blocks[i].Text == "" {
			continue
		}
		if out, evts := s.Replace(evt.Blocks[i].Text, "ctx_block", security.Patterns); len(evts) > 0 {
			evt.Blocks[i].Text = out
			logEvtHits("上下文记忆块", evts)
		}
	}
}

// scrubMemoryCandidate 清洗记忆候选载荷（不经 context 的那条侧路）。
//
// 就地改写 map：调用方传进来的是一次性的快照，改它没有副作用。
func scrubMemoryCandidate(payload map[string]interface{}) {
	if payload == nil {
		return
	}
	s := security.Global()
	if s == nil {
		return
	}
	for _, key := range []string{"input", "response"} {
		str, ok := payload[key].(string)
		if !ok || str == "" {
			continue
		}
		if out, evts := s.Replace(str, "memory:"+key, security.Patterns); len(evts) > 0 {
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
				"memory:tool:"+trs[i].Name, security.Patterns); len(evts) > 0 {
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
			if scrubbed, evts := s.Replace(out, "memory:tool", security.Patterns); len(evts) > 0 {
				m["output"] = scrubbed
				logEvtHits("记忆候选工具结果", evts)
			}
		}
	}
}

func logEvtHits(where string, evts []security.Evt) {
	for _, e := range evts {
		log.Printf("[security] %s blocked %s (pattern %s, %d bytes, merged=%v)",
			where, e.Alias, e.Kind, e.Len, e.Merged)
	}
}

// ---------------- 两处拦截的关系（重要，别删任一处） ----------------
//
// 实测确认：`Append` 里的 `scrubEventSecrets` 会**连带改掉** `f.ToolResults` ——
// 因为 `turnEvt.ToolResults = f.ToolResults` 是值切片，共享底层数组，
// 而 scrub 对 `evt.ToolResults[i].Output` 是就地赋值。
//
// 于是 `emitMemoryCandidate` 收到时往往已是别名 ⇒ 单独去掉
// `scrubMemoryCandidate` 的变异实验**不会报红**（已验证）。
//
// ## 那为什么还要保留 scrubMemoryCandidate
//
// 因为「隐式依赖别名」是**脆弱**的：只要有人
//   · 把 `turnEvt.ToolResults` 改成深拷贝（很自然的"防御性拷贝"改动），
//   · 或调整 Append 与 emitMemoryCandidate 的先后顺序，
//   · 或新增一条不经过 Append 的写入路径，
// 记忆侧就会静默恢复泄漏，而没有任何判据会报红。
//
// `scrubMemoryCandidate` 是**显式契约**：无论上游怎么排版，记忆候选本身
// 必须自己清洗。幂等（已是别名的不会再命中），代价只是一次正则扫描。
//
// 同理 `Append` 与 `InsertByTimestamp` 两处都要调 `scrubEventSecrets`：
// 它们是两个独立入口，不能只保一处。
