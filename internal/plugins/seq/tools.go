package seq

import "strings"

// seqToolDefs 返回本插件导出的六个工具定义。
//
// ⚠️ 全部**不声明** ParallelSafe：seq_run / seq_call 会执行**一串**工具，
// 其中可能含写操作。标成并发安全会让内核把两条 seq_run 并发跑起来，
// 两个序列的执行顺序交错、变量表互相污染。
//
// 为独立真相源：注册、判据、文档都从这里取，避免三处各写一份。
func seqToolDefs() []toolDefInfo {
	return []toolDefInfo{
		{
			Name: "seq_create",
			Description: "创建/更新一条工具序列。**groups 与 file 二选一**：传 groups 直接给结构，" +
				"或用 file 加载你已写好的序列文件（适合长序列——长参数会被 max_tokens 截断，写文件更稳）。\n" +
				"序列由若干 group 组成：**组内并行、组间串行**；group 拥有独立签名（in 入参 / out 出参），" +
				"可被 seq_call 按名调用。\n" +
				"保存时会做静态校验：as 必须已在 out 声明、$args.x 必须已在 in 声明、组名不重复、" +
				"组内 ; 分隔符必须完整。任一项不满足都会报错并说明原因。\n" +
				"格式：JSON；每个 group 的 tools 是一个字符串，内含若干以 ' ; ' 分隔的 JSON 对象，" +
				"形如 {\"tool\":\"cmd_run\",\"args\":{...},\"as\":\"槽名\"}。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string", "description": "序列名（唯一）",
					},
					"description": map[string]interface{}{
						"type": "string", "description": "一句话说明这条序列做什么",
					},
					"groups": map[string]interface{}{
						"type": "array",
						"description": "组数组，按数组顺序执行；与 file 二选一。" +
							"⚠️ 每个 group 的 in 必须是**对象**（无入参写 {}），写成空字符串会被拒绝；" +
							"tools 是字符串（内容为 ';' 分隔的 JSON 对象，每个 tool 后都要有 ';'，含最后一个）。" +
							"格式细节先用 seq_help 查。",
						"items": map[string]interface{}{"type": "object"},
					},
					"file": map[string]interface{}{
						"type":        "string",
						"description": "序列文件路径（与 groups 二选一）。目录内文件优先用 files 工具查看",
					},
					"boot": map[string]interface{}{
						"type": "boolean",
						"description": "标记为开机序列：下次系统就绪时自动执行一次。" +
							"适合「每次启动都要做的那套固定动作」（自检、预热、拉取状态）。" +
							"注意是**每次进程启动一次**，不是定时任务。",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name: "seq_help",
			Description: "查看工具序列的完整格式说明与可照抄的示例。**写序列前先查这个** —— " +
				"tools 字段的分隔符、in/out 的写法、as 与 out 的关系都在这里。",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name: "seq_list",
			Description: "列出全部可用序列：名称、描述、组数与各组的签名（in/out）。" +
				"按名调用前先用它确认名称与签名。",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		{
			Name:        "seq_delete",
			Description: "删除一条序列。序列不存在时会报错（不会静默成功）。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string", "description": "要删除的序列名",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name: "seq_run",
			Description: "执行一条序列：按 groups 数组的顺序逐组执行，组内并行、组间串行。" +
				"每组先求值 when 条件，为真才执行。返回逐组摘要与最终变量槽快照。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string", "description": "要执行的序列名",
					},
					"args": map[string]interface{}{
						"type":        "object",
						"description": "传给各组的入参（组内用 $args.<键> 读取）",
					},
				},
				"required": []string{"name"},
			},
		},
		{
			Name: "seq_call",
			Description: "按名调用一条序列里的 group：target 写组名（限本序列内）或 " +
				"#序列名（跨序列）。返回该组的 out 槽。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string", "description": "所属序列名（组名调用时必填）",
					},
					"target": map[string]interface{}{
						"type":        "string",
						"description": "组名，或 #序列名 表示跨序列调用",
					},
					"args": map[string]interface{}{
						"type":        "object",
						"description": "传给该组的入参",
					},
				},
				"required": []string{"target"},
			},
		},
		{
			Name: "seq_when_call",
			Description: "条件按名调用：when 表达式为真才执行，否则整次调用跳过（不产出任何槽）。" +
				"when 只可读传入的 args（如 $args.flag == true）。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type": "string", "description": "所属序列名（组名调用时必填）",
					},
					"target": map[string]interface{}{
						"type":        "string",
						"description": "组名，或 #序列名 表示跨序列调用",
					},
					"args": map[string]interface{}{
						"type":        "object",
						"description": "传给该组的入参",
					},
					"when": map[string]interface{}{
						"type":        "string",
						"description": "条件表达式，如 $args.flag == true",
					},
				},
				"required": []string{"target", "when"},
			},
		},
	}
}

// 内部小工具：把 args 里的字符串字段取出来并 trim。
func argString(args map[string]interface{}, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return strings.TrimSpace(s)
}
