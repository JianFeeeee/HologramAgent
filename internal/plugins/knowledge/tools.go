package knowledgeplugin

// tools.go —— 知识库工具由**插件自己注册**（用户裁定 2026-11-18）。
//
// ## 为什么不再是内核的 tooldefs.go 两段 + toolcall.go 的 executeKnowledgeTool
//
// 用户裁定（§九）：
//
//	「knowledge 不应该作为一个内核模块，而是应该作为一个内部插件，
//	  因为它并不必须依附内核内部接口实现，而是可以通过 sdk 完美实现功能」
//
// 所以它连**工具面**也一起搬走：
//
//	基座 tooldefs.go   两段 knowledge_* 的渲染  → 删（改为插件注册后从注册表渲染）
//	基座 toolcall.go   executeKnowledgeTool 128 行 → 删
//	toolroute           `knowledge_` 前缀分支     → 删
//
// ## 为什么声明 NoMemory / ParallelSafe 是插件自己的责任
//
// 这两个标志的**语义只有工具作者知道**：
//
//	NoMemory      检索结果来自知识库本身 ⇒ 再把它喂回向量化就是自激回路
//	ParallelSafe  只读检索可并发；create/import/delete 是写，必须串行
//
// 内核无从推断（它不认识"知识库"），所以声明权归插件 —— 与文件插件声明
// `PathArgs` 同一纪律。

import (
	"fmt"
	"strings"
)

// RegisterTools 注册 knowledge_* 工具族（插件 Start 时调一次）。
//
// 返回注册了多少个（供启动日志）。单个注册失败不掩盖其余 —— 注册表的错误
// 会原样返回给调用方，由插件决定是否致命。
func RegisterTools(reg ToolRegistrar, store *Store) (int, error) {
	if reg == nil || store == nil {
		return 0, fmt.Errorf("knowledge: 工具注册需要 ToolRegistrar 与 Store")
	}
	n := 0

	// ── 只读：检索 / 列举（可并发）──
	add := func(def ToolDef, h ToolHandler) error {
		if err := reg.RegisterTool(def, h); err != nil {
			return err
		}
		n++
		return nil
	}

	if err := add(ToolDef{
		Name: "knowledge_search",
		Description: "搜索知识库。输入查询关键词，返回相关知识内容。" +
			"可用 category 把搜索限定在某个分类子树内。",
		// ★ 检索结果来自知识库本身 ⇒ 不再参与记忆计算（否则自激）。
		NoMemory:     true,
		ParallelSafe: true,
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query":    map[string]interface{}{"type": "string", "description": "查询关键词"},
				"top_k":    map[string]interface{}{"type": "integer", "description": "返回数量", "default": 5},
				"category": map[string]interface{}{"type": "string", "description": "可选：限定在某个分类内（前缀匹配子树，如 tech 会搜 tech/go、tech/rust）。留空则搜全库"},
			},
			"required": []string{"query"},
		},
	}, func(args map[string]interface{}) (any, error) {
		query, _ := args["query"].(string)
		if strings.TrimSpace(query) == "" {
			return nil, fmt.Errorf("query is required")
		}
		topK := intArg(args, "top_k")
		if topK <= 0 {
			topK = 5
		}
		category, _ := args["category"].(string)
		results := store.SearchIn(query, category, topK)
		if len(results) == 0 {
			return "no relevant knowledge found", nil
		}
		var parts []string
		for i, k := range results {
			if i >= topK {
				break
			}
			// Name 已是含分类的规范名（"tech/go/并发"），不要再拼 Category
			//（曾经拼过一次，输出成 "tech/go/tech/go/并发"）。
			parts = append(parts, fmt.Sprintf("[%s]\n%s", k.Name, truncateRunes(k.Content, 200)))
		}
		return strings.Join(parts, "\n---\n"), nil
	}); err != nil {
		return n, err
	}

	if err := add(ToolDef{
		Name:         "knowledge_list",
		Description:  "列出知识库中所有知识分类。",
		NoMemory:     true,
		ParallelSafe: true,
		Parameters:   map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}, func(args map[string]interface{}) (any, error) {
		return formatTree(store.BuildTree(), 0), nil
	}); err != nil {
		return n, err
	}

	// ── 写：create / import / delete（**不可并发**，零值 false 就是串行）──
	if err := add(ToolDef{
		Name: "knowledge_create",
		Description: "创建新知识。将知识写入知识库，自动向量化索引。" +
			"可关联已入库媒体（附图/音视频）使该知识能被图本身检索到。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name":    map[string]interface{}{"type": "string", "description": "知识名称（用作目录名）"},
				"content": map[string]interface{}{"type": "string", "description": "知识内容，支持 Markdown"},
				"media_digests": map[string]interface{}{
					"type":        "array",
					"items":       map[string]interface{}{"type": "string"},
					"description": "可选：关联的媒体 digest（可传前缀）。媒体作为一等节点参与跨模态检索——知识能按图本身被搜到，而不依赖生成的描述文本",
				},
			},
			"required": []string{"name", "content"},
		},
	}, func(args map[string]interface{}) (any, error) {
		name, _ := args["name"].(string)
		content, _ := args["content"].(string)
		if name == "" || content == "" {
			return nil, fmt.Errorf("name and content are required")
		}
		media := mediaRefs(stringSlice(args, "media_digests"))
		if err := store.AddWithMedia(name, content, media); err != nil {
			return nil, fmt.Errorf("knowledge create failed: %w", err)
		}
		if len(media) > 0 {
			return fmt.Sprintf("Knowledge %q created and indexed (%d chars, %d media items linked for cross-modal recall)",
				name, len(content), len(media)), nil
		}
		return fmt.Sprintf("Knowledge %q created and indexed (%d chars)", name, len(content)), nil
	}); err != nil {
		return n, err
	}

	if err := add(ToolDef{
		Name: "knowledge_import_dir",
		Description: "从目录批量导入知识（复制，不是引用）。递归扫描该目录，把文档复制进知识库并自动完成索引与向量化。" +
			"\n目录约定（自动适配，无需改造资料）：含 content.md 的目录整体作为一条知识；否则目录里的 .md/.txt 等文件各成一条，**目录路径即分类**。" +
			"\n注意：dir 必须是绝对路径；同名条目会跳过而不覆盖。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"dir":      map[string]interface{}{"type": "string", "description": "源目录的绝对路径"},
				"category": map[string]interface{}{"type": "string", "description": "可选：分类前缀"},
				"include_media": map[string]interface{}{
					"type": "boolean", "description": "可选：是否把目录里的图片/音视频也导入", "default": false,
				},
				"dry_run": map[string]interface{}{
					"type": "boolean", "description": "可选：只报告将导入什么，不实际写入", "default": true,
				},
				"max_items": map[string]interface{}{"type": "integer", "description": "可选：本次最多导入多少条（默认 500）"},
			},
			"required": []string{"dir"},
		},
	}, func(args map[string]interface{}) (any, error) {
		dir, _ := args["dir"].(string)
		category, _ := args["category"].(string)
		// dry_run 默认 true：导入是批量写，第一次试某目录应先看清会写什么。
		dryRun := true
		if b, ok := boolArg(args, "dry_run"); ok {
			dryRun = b
		}
		includeMedia := false
		if b, ok := boolArg(args, "include_media"); ok {
			includeMedia = b
		}
		st, err := store.ImportDir(ImportOptions{
			Dir:          dir,
			Category:     category,
			DryRun:       dryRun,
			IncludeMedia: includeMedia,
			MaxItems:     intArg(args, "max_items"),
		})
		if err != nil {
			return nil, fmt.Errorf("knowledge import failed: %w", err)
		}
		return formatImportStats(st, dryRun, category), nil
	}); err != nil {
		return n, err
	}

	if err := add(ToolDef{
		Name:        "knowledge_delete",
		Description: "删除知识库中的指定知识条目。",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"name": map[string]interface{}{"type": "string", "description": "要删除的知识名称"},
			},
			"required": []string{"name"},
		},
	}, func(args map[string]interface{}) (any, error) {
		name, _ := args["name"].(string)
		if name == "" {
			return nil, fmt.Errorf("name is required")
		}
		if err := store.Remove(name); err != nil {
			return nil, fmt.Errorf("knowledge delete failed: %w", err)
		}
		return fmt.Sprintf("Knowledge %q deleted", name), nil
	}); err != nil {
		return n, err
	}

	return n, nil
}

// ── 参数与渲染的小工具（插件自己的，不借内核）──

// intArg 取整数参数（JSON 解出来是 float64）。
func intArg(args map[string]interface{}, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

// boolArg 取布尔参数（第二个返回值区分"没给"与"给了 false"）。
func boolArg(args map[string]interface{}, key string) (bool, bool) {
	v, ok := args[key].(bool)
	return v, ok
}

// stringSlice 取字符串数组参数。
func stringSlice(args map[string]interface{}, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// mediaRefs 把 digest 字符串转成媒体引用（Store.AttachMedia 认这个形状）。
func mediaRefs(digests []string) []KnowledgeMediaRef {
	if len(digests) == 0 {
		return nil
	}
	out := make([]KnowledgeMediaRef, 0, len(digests))
	for _, d := range digests {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, KnowledgeMediaRef{Digest: d})
		}
	}
	return out
}

// truncateRunes 按**字符**截断（不能按字节，否则中文会被切碎）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// formatTree 渲染分类树（插件自己的展示格式）。
func formatTree(node *TreeIndex, depth int) string {
	if node == nil {
		return "（知识库为空）"
	}
	var sb strings.Builder
	indent := strings.Repeat("  ", depth)
	for _, child := range node.Children {
		sb.WriteString(fmt.Sprintf("%s%s/\n", indent, child.Name))
		sb.WriteString(formatTree(child, depth+1))
	}
	for _, item := range node.Items {
		tags := ""
		if len(item.Tags) > 0 {
			tags = " [" + strings.Join(item.Tags, ", ") + "]"
		}
		sb.WriteString(fmt.Sprintf("%s· %s%s\n", indent, item.Name, tags))
	}
	return sb.String()
}

// formatImportStats 渲染导入结果（失败原因必须报给 agent，否则它无从下手）。
func formatImportStats(st ImportStats, dryRun bool, category string) string {
	var b strings.Builder
	verb := "已导入"
	if dryRun {
		verb = "将导入（dry_run，未实际写入）"
	}
	fmt.Fprintf(&b, "%s %d 条", verb, st.Imported)
	if category != "" {
		fmt.Fprintf(&b, "（分类前缀 %s）", category)
	}
	if st.Media > 0 {
		fmt.Fprintf(&b, "，含 %d 个媒体", st.Media)
	}
	if st.Skipped > 0 {
		fmt.Fprintf(&b, "；跳过 %d", st.Skipped)
	}
	if st.Failed > 0 {
		fmt.Fprintf(&b, "；失败 %d", st.Failed)
	}
	if st.Truncated {
		b.WriteString("；★ 超出 max_items 被截断，未导完（可调大 max_items 或分批）")
	}
	if len(st.Names) > 0 {
		show := st.Names
		if len(show) > 10 {
			show = show[:10]
		}
		fmt.Fprintf(&b, "。知识名：%s", strings.Join(show, "、"))
		if len(st.Names) > 10 {
			fmt.Fprintf(&b, " …共 %d 个", len(st.Names))
		}
	}
	for _, e := range st.Errors {
		fmt.Fprintf(&b, "\n- %s", e)
	}
	return b.String()
}
