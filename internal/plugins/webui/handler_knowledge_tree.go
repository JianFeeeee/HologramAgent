package webui

import (
	"net/http"
	"strconv"
	"strings"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 知识库的**树形只读 API**。供外部 agent、skill、脚本按分类导航知识库。
//
// 为何只读：写入面已有 POST /api/v1/knowledge（可带媒体）。而写操作
// 需要决定"挂到哪个分类、是否带媒体、是否重算稠密向量"，那是内核的事；
// 让外部 agent 自己拼分类路径反而容易写出越界/重名的条目。读多写少，
// 且读才是"让别的 agent 用起来"的关键。
//
// 鉴权沿用 requireAPI（api_key 或 cookie session），不新增鉴权面。
//
// 路由：
//
//	GET /api/v1/knowledge/tree                      整棵树
//	GET /api/v1/knowledge/tree/{category}           某棵子树
//	    ?depth=N         限制层数（0/省略 = 不限）——分类多时做懒加载
//	    ?items=0|1       是否返回条目详情，默认 1
//	    ?preview=N       预览字数上限，默认 120
//	    ?q=关键词        在**该子树内**检索（分类 + 关键词组合）
//	    ?limit=N         q 时的返回条数，默认 10
//	GET /api/v1/knowledge/tree/categories           平铺分类列表
//	GET /api/v1/knowledge/tree/counts               各分类条目数（倒序）
//
// 为什么把 categories/counts 也挂在 /tree/ 下：它们是**导航辅助**，
// 回答"有哪些分类、哪里的内容最多"，与树形视图同源，挂在别处会割裂。

// handleKnowledgeTree 处理树形只读请求。
func (h *Handler) handleKnowledgeTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET"})
		return
	}
	if h.knowledge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
		return
	}

	// path 形如 /api/v1/knowledge/tree[/sub/category]
	sub := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/knowledge/tree"), "/")

	// 平铺分类列表
	if sub == "categories" {
		names, err := h.knowledge.Categories()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if names == nil {
			names = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"categories": names})
		return
	}
	// 各分类条目数
	if sub == "counts" {
		counts, err := h.knowledge.CategoryCounts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if counts == nil {
			counts = []sdk.KnowledgeCategoryCount{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"counts": counts})
		return
	}

	q := r.URL.Query()
	opt := sdk.KnowledgeTreeOptions{
		MaxDepth:     intQuery(q.Get("depth"), 0),
		IncludeItems: boolQuery(q.Get("items"), true),
		PreviewLimit: intQuery(q.Get("preview"), 0),
	}

	// 分类 + 关键词：在该子树内检索
	if keyword := strings.TrimSpace(q.Get("q")); keyword != "" {
		limit := intQuery(q.Get("limit"), 10)
		if limit <= 0 || limit > 100 {
			limit = 10
		}
		results, err := h.knowledge.SearchIn(keyword, sub, limit)
		if err != nil {
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		views := make([]knowledgeView, 0, len(results))
		for _, k := range results {
			views = append(views, toKnowledgeView(k, k.Name))
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"results":  views,
			"category": sub,
			"query":    keyword,
		})
		return
	}

	// 树视图（category 为空 = 整棵树）
	view, err := h.knowledge.Subtree(sub, opt)
	if err != nil {
		code, msg := knowledgeStatusFor(err)
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	if view == nil {
		// 分类不存在：这是调用方能自己纠正的错误，给 404 + 现有分类便于自查
		known, _ := h.knowledge.Categories()
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":      "分类不存在: " + sub,
			"categories": known,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"tree": view})
}

// intQuery 解析整数查询参数；空/非法时用 def。
func intQuery(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// boolQuery 解析布尔查询参数；空时用 def。接受 1/0/true/false。
func boolQuery(s string, def bool) bool {
	if s == "" {
		return def
	}
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
