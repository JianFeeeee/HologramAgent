package mainagent

// memoryToolDefs 声明**主 agent 私有**的记忆族工具（stablemem_recall / commit / introspect）。
//
// ## 为什么声明在这里而不是 internal/memory
//
// 用户裁定（2026-11-19）：渲染归类型。`stablemem_*` 读的是**主 agent 的私有状态**
// （它的图记忆 `*GraphDB` / 索引器），执行体也在这个包里（`toolcall.go`）——
// 声明留在 `internal/memory` 等于「schema 在一个包、执行体在另一个包」，
// 声明的门控与执行的可用性会各自漂移。
//
// ★ `internal/memory.Indexer.GetToolDefinitions()` 仍保留，但它的用途降级为
//
//	**插件只读展示**（webui `/api/v1/memory/tools`、cli `/memory tools`）：
//	那是"给插件看的工具清单"，不是"给模型看的下发 schema"。两个消费面分开，
//	就不会再出现「渲染源有两处」。
func memoryToolDefs() []interface{} {
	return []interface{}{
		toolDef("stablemem_recall",
			"检索图记忆。输入查询意图关键词，返回相关实体和关系。"+
				"问『某个具体东西是什么/是多少』用默认相关性排序；"+
				"问『最近/最新/现在是什么』必须传 sort=recent —— "+
				"运维场景里答案常是新值覆盖旧值，相关性排序分不出新旧。",
			map[string]interface{}{
				"query_intent": map[string]interface{}{
					"type":        "string",
					"description": "查询意图，支持逗号分隔多个关键词",
				},
				"depth": map[string]interface{}{
					"type":        "integer",
					"description": "遍历深度，默认2",
					"default":     2,
				},
				"sort": map[string]interface{}{
					"type": "string",
					"description": "排序方式。relevance=相关性（默认，问具体是什么/多少）；" +
						"recent=时间倒序最新在前（问最近/最新/现在）。",
					"enum": []string{"relevance", "recent"},
				},
			}, "query_intent"),

		toolDef("stablemem_commit",
			"写入图记忆。将三元组列表写入长期记忆。",
			map[string]interface{}{
				"triples": map[string]interface{}{
					"type":        "array",
					"description": "三元组列表",
					"items": map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"subject":  map[string]interface{}{"type": "string"},
							"relation": map[string]interface{}{"type": "string"},
							"object":   map[string]interface{}{"type": "string"},
							"sentence_text": map[string]interface{}{
								"type":        "string",
								"description": "可选：这条三元组的原始句子。填了才能日后从图谱回到原文。",
							},
							"media_digests": map[string]interface{}{
								"type":        "array",
								"description": "可选：这条记忆关联的媒体 digest（对话或 memory_recall 的「关联媒体」里显示的十六进制串，短的即可）。填了以后从这条记忆能取回原图/音频。",
								"items":       map[string]interface{}{"type": "string"},
							},
							"scene": map[string]interface{}{
								"type":        "string",
								"description": "可选：这条记忆所属的场景键（如 chan:qq、chan:qq/peer:group_123、tool:qq_get_message）。「在什么场合该怎么做」这类条件性约定/规则必须填：场面重现时它会被直接召回，与用户措辞无关。",
							},
						},
						"required": []string{"subject", "relation", "object"},
					},
				},
			}, "triples"),

		toolDef("stablemem_introspect",
			"查看图记忆统计信息：实体数量、关系数量、热点实体。",
			map[string]interface{}{}),
	}
}
