package mainagent

// extslot_graph.go —— 主 agent 交给内核的**图记忆工具**（docs §6.4）。
//
// ## 设计（用户裁定 2026-11-20）
//
// 主 agent 在 Initialize 里注册**两个普通工具**：
//
//	graph_recall  {keywords, depth}  → map
//	graph_commit  {triples}          → map
//
// 它们是**给 agent 调的 tool**，与 `output_send__*`、`stablemem_*` 完全同类：
// 出现在工具表里、模型自己决定调用、走同一条执行链（`RunToolBatch` →
// `toolexec` → `DispatchTool` → 本文件的闭包）。
//
// ## 权限与隔离**在闭包里实现**，不是开个口子透出去
//
// 收到调用时把**调用方身份**（`ToolCtx.Host`）取出来，由闭包自己决定：
//
//	读（graph_recall）  所有 agent 都可以（只读，不改变任何数据）
//	写（graph_commit）  落进主库，但**按调用方打归属标记**（sessionID）
//	                    —— 于是谁能写什么可审计、可过滤，而子 agent
//	                    不需要、也不可能拿到库句柄
//
// ★ 所以**没有** handle 参数、没有 graph_open / graph_close / graph_export：
//   生命周命与数据全在主 agent 这边，暴露生命周期函数毫无意义。
//   子 agent 也不需要 GraphDB / LightMemory / OpenLight / ExportTriples ——
//   那些整套逻辑是**要移除的**，不是要改成经工具调一次。

import (
	"encoding/json"
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// graphSlotTools 返回主 agent 要交给内核的图记忆工具。
//
// 只在**主 agent 的 Initialize** 里调用（docs §6.2 规则 1：只有被选中为
// 主 agent 的类型才注册）。
func (m *Module) graphSlotTools() []registry.Tool {
	return []registry.Tool{
		{
			Name:        "graph_recall",
			Description: "从长期图记忆中按关键词召回实体与关系。只读，不改变任何数据。",
			// ★ SideChild：这两个工具是**交出去给子 agent 用**的。
			//   主 agent 自己读图库走它的私有族（`stablemem_*`），不需要
			//   绕一圈调自己交出去的钩子 —— 那会让同一能力在它的工具表里
			//   有两套名字（stablemem_recall 与 graph_recall），模型会选错。
			//
			// "哪些子 agent 能看"由父在 create 时划定（docs §6.2 规则 3），
			// 那一步是**实例声明事实**，不是这里的静态 Side。
			Side: registry.SideChild,
			Properties: map[string]interface{}{
				"keywords": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "string"},
					"description": "召回关键词",
				},
				"depth": map[string]interface{}{
					"type": "integer", "description": "关系展开深度（默认 1）",
				},
			},
			Required: []string{"keywords"},
			Parallel: true,
			NoMemory: true,
			Handle: func(c registry.ToolCtx, tc api.ToolCall) string {
				return m.graphRecall(c, tc.Arguments)
			},
		},
		{
			Name:        "graph_commit",
			Description: "把三元组写入长期图记忆。写入会按调用方标记归属，便于审计与回溯。",
			Side:        registry.SideChild,
			Properties: map[string]interface{}{
				"triples": map[string]interface{}{
					"type": "array", "items": map[string]interface{}{"type": "object"},
					"description": "三元组数组（subject / relation / object / confidence / sentence_text）",
				},
			},
			Required: []string{"triples"},
			Handle: func(c registry.ToolCtx, tc api.ToolCall) string {
				return m.graphCommit(c, tc.Arguments)
			},
		},
	}
}

// callerID 取**调用方身份**（权限判定的唯一依据）。
//
// ★ 由闭包自己判，而不是让子 agent 自报：`ToolCtx.Host` 是执行链传下来的
//
//	真实宿主，调用方改不了它。
func callerID(c registry.ToolCtx) string {
	a, ok := c.Host.(*agent.Agent)
	if !ok || a == nil {
		return ""
	}
	return string(a.ID())
}

// graphRecall 是 `graph_recall` 的实现（只读，任何 agent 都可以）。
func (m *Module) graphRecall(c registry.ToolCtx, args map[string]interface{}) string {
	if m.sv == nil || m.sv.Memory == nil {
		return "图记忆未初始化"
	}
	var keywords []string
	if raw, ok := args["keywords"].([]interface{}); ok {
		for _, v := range raw {
			if s, ok := v.(string); ok && s != "" {
				keywords = append(keywords, s)
			}
		}
	}
	if len(keywords) == 0 {
		return "graph_recall 需要非空 keywords"
	}
	depth := 1
	if d, ok := args["depth"].(float64); ok && int(d) > 0 {
		depth = int(d)
	}
	res, err := m.sv.Memory.Recall(keywords, nil, depth, "")
	if err != nil {
		return fmt.Sprintf("召回失败: %v", err)
	}
	if res == nil {
		return `{"entities":[],"relations":[]}`
	}
	b, err := json.Marshal(map[string]interface{}{
		"entities":  res.Entities,
		"relations": res.Relations,
	})
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}

// graphCommit 是 `graph_commit` 的实现。
//
// ★ 隔离在这里实现：写入**按调用方打归属标记**（sessionID）。
//
//	主库只有一份（数据落在主 agent 这边），但"这条是谁写的"永远可查，
//	于是"某个子 agent 写进来一堆噪声"是可定位、可清理的事实，
//	而不是一个查不出来的副作用。
func (m *Module) graphCommit(c registry.ToolCtx, args map[string]interface{}) string {
	if m.sv == nil || m.sv.Memory == nil {
		return "图记忆未初始化"
	}
	triples := parseTriplesArg(args["triples"])
	if len(triples) == 0 {
		return "graph_commit 需要非空 triples（每条要有 subject / relation / object）"
	}
	who := callerID(c)
	if who == "" {
		// 没有调用方身份 ⇒ 不写。这是"权限控制在闭包里"的直接体现：
		// 不知道是谁写的就不落库，而不是默认为主 agent。
		return "graph_commit 无法确定调用方身份，已拒绝写入"
	}
	ec, rc, err := m.sv.Memory.Commit(triples, "agent:"+who, 0)
	if err != nil {
		return fmt.Sprintf("写入失败: %v", err)
	}
	return fmt.Sprintf(`{"entities_written":%d,"relations_written":%d,"written_by":%q}`, ec, rc, who)
}

// parseTriplesArg 把工具参数解析成三元组（模型给的是 map 列表）。
func parseTriplesArg(v interface{}) []model.Triple {
	raw, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]model.Triple, 0, len(raw))
	for _, item := range raw {
		mm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		t := model.Triple{}
		t.Subject, _ = mm["subject"].(string)
		t.Relation, _ = mm["relation"].(string)
		t.Object, _ = mm["object"].(string)
		if cf, ok := mm["confidence"].(float64); ok {
			t.Confidence = cf
		}
		t.SentenceText, _ = mm["sentence_text"].(string)
		if t.Subject == "" || t.Relation == "" || t.Object == "" {
			continue
		}
		out = append(out, t)
	}
	return out
}
