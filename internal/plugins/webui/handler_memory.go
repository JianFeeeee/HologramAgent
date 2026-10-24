package webui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 记忆面：图记忆 / 文档记忆 / 文本记忆 / 知识库 / LLM 源 / 变更追踪。

func (h *Handler) handleMemory(w http.ResponseWriter, r *http.Request) {
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		userInput := r.URL.Query().Get("q")
		keywords := strings.Split(userInput, ",")
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		if depth <= 0 {
			depth = 2
		}
		entities, relations, err := h.memory.Recall(keywords, depth)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"entities": entities, "relations": relations})
	case http.MethodPost:
		var req struct {
			Triples []sdk.Triple `json:"triples"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.memory.Commit(req.Triples); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{"status": "committed", "committed": len(req.Triples)})
	case http.MethodDelete:
		var req struct {
			Criteria map[string]string `json:"criteria"`
			Mode     string            `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		deleted, err := h.memory.Purge(req.Criteria, req.Mode)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": deleted})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleMemoryContext(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	userInput := r.URL.Query().Get("q")
	injected, err := h.indexer.BuildContext(userInput)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"context":        h.indexer.FormatContext(injected),
		"summary":        injected.Summary,
		"entities":       injected.Entities,
		"token_estimate": injected.TokenEstimate,
		"tool_prompt":    h.indexer.BuildToolPrompt(),
	})
}

func (h *Handler) handleMemoryTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.indexer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "indexer not available"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tools":       h.indexer.GetToolDefinitions(),
		"tool_prompt": h.indexer.BuildToolPrompt(),
	})
}

// graphBlockView 是 memory_blocks 的**瘦身**下发视图。
//
// 为什么要瘦身（实测生产实例 1151 节点 / 866 边）：
//
//	原始 /memory/graph 响应 408,146 B，其中 memory_blocks[].vector 占 79,314 B
//	（19%）。那是稠密向量 —— 检索侧（SearchIn / 稠密召回）才需要它，
//	而星图是本接口**唯一**消费者，它只画节点/连线，压根不读 vector。
//
//	更大的问题是量级：每多一块记忆就多一份向量。8 块已经 79KB，
//	200 块就是约 2MB 白白从库里查出来、序列化、走 socket、丢进浏览器堆，
//	全程没有一行代码看过它。文本向量的维度还随模型走（数百到数千），
//	换一次 embedder 就能让这个开销翻几倍。
//
// 所以这里显式裁掉 vector，而不是让 GraphData 返回值带个开关：
// 本接口的语义就是「图谱的可视化数据」，让唯一调用方拿到它要的东西。
type graphBlockView struct {
	ID          string    `json:"id"`
	Modality    string    `json:"modality"`
	Text        string    `json:"text,omitempty"`
	HasData     bool      `json:"has_data"`
	MIME        string    `json:"mime,omitempty"`
	Size        int64     `json:"size"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Source      string    `json:"source,omitempty"`
	Tool        string    `json:"tool,omitempty"`
	Scene       string    `json:"scene,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// graphDataForVisual 把 GraphData 的原始 map 裁成可视化视图（去掉稠密向量）。
//
// 用 map 断言而不是泛型/反射：GraphData 返回 map[string]interface{}，
// 里面的具体类型是包内私有的 graphEntity/[]*memory.MemoryBlock，
// 断言不中就原样透传（宁可多发也不让接口挂掉）。
func graphDataForVisual(data map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		out[k] = v
	}
	blocks, _ := out["memory_blocks"].([]memory.MemoryBlock)
	if blocks == nil {
		// 可能是 []*memory.MemoryBlock 或空；两种都不是就直接跳过裁剪。
		return out
	}
	views := make([]graphBlockView, 0, len(blocks))
	for i := range blocks {
		b := blocks[i]
		views = append(views, graphBlockView{
			ID:          b.ID,
			Modality:    string(b.Modality),
			Text:        b.Text,
			HasData:     len(b.Data) > 0,
			MIME:        b.MIME,
			Size:        b.Size,
			Width:       b.Width,
			Height:      b.Height,
			Fingerprint: b.Fingerprint,
			Source:      b.Source,
			Tool:        b.Tool,
			Scene:       b.Scene,
			CreatedAt:   b.CreatedAt,
			UpdatedAt:   b.UpdatedAt,
		})
	}
	out["memory_blocks"] = views
	return out
}

func (h *Handler) handleMemoryGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	data, err := h.memory.GraphData()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": graphDataForVisual(data)})
}

// handleMemoryGraphPulse 是给星图「跟随 agent 动」用的**轻量**活动端点。
//
// 为什么不让星图反复拉完整 /memory/graph 做对比：
//
//	完整图谱生产实例 408KB（瘦身前 408KB→瘦身后约 329KB，仍含 1151 个节点
//	和 866 条边的全量 JSON）。为了「知道哪些节点是新的」而每 N 秒拉一次全量，
//	是把带宽和 JSON.parse 全花在重复数据上。
//
// 这里只回「最近 since 秒内变动过的实体」，字段压到最小（id + name +
// mention_count + updated_at），实测是几百字节到几 KB 的量级 ——
// 与完整图谱差两个数量级。新节点「生长」出来、老节点被再次提及而计数变化，
// 都能从这份清单里看出来。
//
// since 缺省给 900s（15 分钟）：略大于星图轮询周期（10s），
// 即使客户端漏掉几个周期也能自愈，不必担心漏掉节点。
func (h *Handler) handleMemoryGraphPulse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.memory == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "memory system not available"})
		return
	}
	since := time.Now().Add(-defaultGraphPulseWindow)
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		if sec, err := strconv.Atoi(raw); err == nil && sec > 0 {
			since = time.Now().Add(-time.Duration(sec) * time.Second)
		}
	}
	// ★ 直接查「窗口内变动的块」，不再走全量 GraphData()（2026-10-08 修性能）。
	//
	// 原实现先 GraphData()（全量 3190 块 + 2692 边）→ Marshal 约 3.3MB →
	// 反序列化 → 才按时间过滤。实测与全量端点**同价**（145ms vs 135ms），
	// 而本端点每 10s 被星图轮询一次 —— 「轻量端点」名不副实。
	// 现在在库侧过滤，响应体只含窗口内的块（差两个数量级）。
	blocks, err := h.memory.BlocksChangedSince(since, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	pulse := make([]map[string]interface{}, 0, len(blocks))
	for _, b := range blocks {
		pulse = append(pulse, map[string]interface{}{
			"id":         b.ID,
			"name":       b.Text,
			"modality":   b.Modality,
			"has_data":   b.HasData,
			"mime":       b.MIME,
			"tool":       b.Tool,
			"scene":      b.Scene,
			"source":     b.Source,
			"updated_at": b.UpdatedAt,
			"created_at": b.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    map[string]interface{}{"nodes": pulse},
	})
	return

}

// defaultGraphPulseWindow 是 /memory/graph/pulse 不带 since 时的回看窗口。
const defaultGraphPulseWindow = 900 * time.Second

// knowledgeWriteReq 是知识写入请求体（JSON 分支）。
type knowledgeWriteReq struct {
	Name    string                  `json:"name"`
	Content string                  `json:"content"`
	Media   []sdk.KnowledgeMediaRef `json:"media,omitempty"`
}

// knowledgeView 是返回给前端的知识条目视图。
//
// 为何不让前端直接吃 *knowledge.Knowledge：那个结构里有 Path（服务端绝对
// 路径，不该外泄）、Dense（几百 KB 浮点数组）。前端只需要 name/category/
// tags/size/updated_at/media 摘要。
type knowledgeView struct {
	Name      string               `json:"name"`
	Category  string               `json:"category,omitempty"`
	Preview   string               `json:"preview"`
	Size      int                  `json:"size"`
	Tags      []string             `json:"tags,omitempty"`
	UpdatedAt string               `json:"updated_at,omitempty"`
	Media     []knowledgeMediaView `json:"media,omitempty"`
}

type knowledgeMediaView struct {
	Digest string `json:"digest"`
	MIME   string `json:"mime"`
	Kind   string `json:"kind,omitempty"`
}

// knowledgeStatusFor 把内核的 ErrInvalidName / ErrNotFound 映射到正确状态码。
//
// 此前一律 500：把「名称非法」「不存在」这种**调用方能自己纠正**的错报成
// 服务器故障，前端无从区分该改请求还是该报服务器挂了。
func knowledgeStatusFor(err error) (int, string) {
	switch {
	case errors.Is(err, knowledge.ErrInvalidName):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, knowledge.ErrNotFound):
		return http.StatusNotFound, err.Error()
	default:
		return http.StatusInternalServerError, err.Error()
	}
}

func toKnowledgeView(k *pubsdk.Knowledge, name string) knowledgeView {
	v := knowledgeView{Name: name}
	if k.Content == "" {
		return v
	}
	// 预览按 rune 截断，避免把多字节字符切成乱码
	r := []rune(k.Content)
	if len(r) > 200 {
		v.Preview = string(r[:200]) + "..."
	} else {
		v.Preview = k.Content
	}
	v.Size = len(k.Content)
	return v
}

func (h *Handler) handleKnowledge(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		// category 为空 = 全库；非空则限定在该分类子树内（前缀匹配）
		category := q.Get("category")
		if query := strings.TrimSpace(q.Get("q")); query != "" {
			limit := 10
			if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 && v <= 100 {
				limit = v
			}
			var results []*pubsdk.Knowledge
			var err error
			if scoped, ok := h.knowledge.(interface {
				SearchIn(query, category string, topK int) ([]*pubsdk.Knowledge, error)
			}); ok {
				results, err = scoped.SearchIn(query, category, limit)
			} else {
				results, err = h.knowledge.Search(query, limit)
			}
			if err != nil {
				code, msg := knowledgeStatusFor(err)
				writeJSON(w, code, map[string]string{"error": msg})
				return
			}
			views := make([]knowledgeView, 0, len(results))
			for _, k := range results {
				views = append(views, toKnowledgeView(k, k.Name))
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{"results": views, "category": category})
			return
		}
		names, err := h.knowledge.List()
		if err != nil {
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		if names == nil {
			names = []string{}
		}
		stats := h.knowledge.Stats()
		payload := map[string]interface{}{"names": names, "stats": stats}
		if ds, ok := h.knowledge.(interface {
			DenseStats() map[string]interface{}
		}); ok {
			payload["dense"] = ds.DenseStats()
		}
		writeJSON(w, http.StatusOK, payload)

	case http.MethodPost:
		// multipart 分支：**先看它到底是什么**。
		//
		// 旧实现无论传什么都把字节 utf-8 强转后当 Markdown 存进 content.md：
		// 上传一张 PNG 得到的是一份乱码文本知识，还会在 .index.json 里占一份
		// preview，且没有任何迹象表明出了问题。现在改为：
		//   - 文本类（text/* 或 JSON 字节）→ 走原路径存正文
		//   - 媒体类（image/audio/video）→ 入 media CAS，按 digest 挂到条目上
		ct := r.Header.Get("Content-Type")
		if strings.HasPrefix(ct, "multipart/form-data") {
			h.handleKnowledgeUpload(w, r)
			return
		}

		var req knowledgeWriteReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		if strings.TrimSpace(req.Content) == "" && len(req.Media) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "content or media is required"})
			return
		}
		if err := h.knowledge.AddWithMedia(req.Name, req.Content, req.Media); err != nil {
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]interface{}{
			"status": "created", "name": req.Name, "media": len(req.Media),
		})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name query param required"})
			return
		}
		if err := h.knowledge.Remove(name); err != nil {
			// 之前所有失败一律 404，包括名称非法（400 的事）与真实 IO 错误
			// （500 的事）。"啥都没删"和"服务器坏了"被混为一谈。
			code, msg := knowledgeStatusFor(err)
			writeJSON(w, code, map[string]string{"error": msg})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "name": name})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// maxKnowledgeUploadBytes 是知识库单次上传的体积上限。
// 与 media CAS 的定位一致：知识条目的媒体是引用，不该拖着一堆原始字节。
const maxKnowledgeUploadBytes = 32 << 20

// handleKnowledgeUpload 处理 multipart 上传，按实际类型分流：媒体入 CAS，
// 文本存正文。绝不把二进制当文本存。
func (h *Handler) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(maxKnowledgeUploadBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid multipart: " + err.Error()})
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}

	files := r.MultipartForm.File["file"]
	// 也接受通用字段名，避免前端只有 file 字段名不匹配时静默走成"无媒体"
	if len(files) == 0 {
		files = r.MultipartForm.File["media"]
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "file field required"})
		return
	}
	if len(files) > 16 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "最多 16 个文件"})
		return
	}

	var (
		media    []sdk.KnowledgeMediaRef
		texts    []string
		rejected []string
	)
	for _, fh := range files {
		f, err := fh.Open()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "open " + fh.Filename + ": " + err.Error()})
			return
		}
		data, err := io.ReadAll(io.LimitReader(f, maxKnowledgeUploadBytes+1))
		f.Close()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "read " + fh.Filename + ": " + err.Error()})
			return
		}
		if len(data) == 0 {
			rejected = append(rejected, fh.Filename+": 空文件")
			continue
		}
		if len(data) > maxKnowledgeUploadBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge,
				map[string]string{"error": fmt.Sprintf("%s 超过 %d 上限", fh.Filename, maxKnowledgeUploadBytes)})
			return
		}

		mime := fh.Header.Get("Content-Type")
		if mime == "" {
			mime = contentTypeByExt(strings.ToLower(filepath.Ext(fh.Filename)))
		}
		// 按**探测到的真实类型**判定，而不是信客户端给的 Content-Type：
		// 声明 text/plain 的 PNG 曾是真实场景，光看头会把二进制当文本存。
		detected := http.DetectContentType(data)
		if isMediaMIME(mime) || isMediaMIME(detected) {
			useMIME := detected
			if detected == "application/octet-stream" {
				useMIME = mime
			}
			// ★ CAS 淘汰（2026-10-11）：字节不再寄存在全局内容寻址库，
			//   而是作为**条目自带数据**随引用携带，由 knowledge.Write 落进
			//   条目目录（与 content.md 同生共灭）。Digest 仍按内容派生
			//   （MediaBlockID 同构），但不再需要先 Put 进独立存储。
			media = append(media, sdk.KnowledgeMediaRef{
				Digest: memory.MediaBlockID(useMIME, data),
				MIME:   useMIME,
				Kind:   mediaKindOf(useMIME),
				File:   filepath.Base(fh.Filename),
				Data:   data,
			})
			continue
		}
		// 文本类：确认是合法 UTF-8 才当正文，否则拒绝并说明原因
		if !utf8.Valid(data) {
			rejected = append(rejected, fmt.Sprintf("%s: 非文本内容且无法识别为媒体（type=%q）", fh.Filename, useMIMEOr(detected, mime)))
			continue
		}
		texts = append(texts, string(data))
	}

	content := strings.TrimSpace(r.FormValue("content"))
	if content == "" {
		content = strings.Join(texts, "\n\n")
	}
	if strings.TrimSpace(content) == "" && len(media) == 0 {
		msg := "没有可写入的内容"
		if len(rejected) > 0 {
			msg += "：" + strings.Join(rejected, "; ")
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	if err := h.knowledge.AddWithMedia(name, content, media); err != nil {
		code, msg := knowledgeStatusFor(err)
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}
	resp := map[string]interface{}{"status": "created", "name": name, "media": media}
	if len(rejected) > 0 {
		resp["rejected"] = rejected
	}
	writeJSON(w, http.StatusCreated, resp)
}

func useMIMEOr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isMediaMIME(m string) bool {
	return strings.HasPrefix(m, "image/") || strings.HasPrefix(m, "audio/") || strings.HasPrefix(m, "video/")
}

func mediaKindOf(m string) string {
	switch {
	case strings.HasPrefix(m, "image/"):
		return "image"
	case strings.HasPrefix(m, "audio/"):
		return "audio"
	case strings.HasPrefix(m, "video/"):
		return "video"
	}
	return "file"
}

func (h *Handler) handleTextMemory(w http.ResponseWriter, r *http.Request) {
	if h.textMem == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "text memory not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		recent, _ := h.textMem.RecentEvents(50)
		stats := h.textMem.Stats()
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":  stats,
			"recent": recent,
		})
	case http.MethodDelete:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "not_implemented"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapters(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{"adapters": h.adapter.List()})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		if err := h.adapter.Load(req.Name, req.Code); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "loaded", "name": req.Name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleAdapterByID(w http.ResponseWriter, r *http.Request) {
	if h.adapter == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "lua vm not available"})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/v1/adapters/")
	if name == "" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		for _, a := range h.adapter.List() {
			if a.Name == name {
				writeJSON(w, http.StatusOK, a)
				return
			}
		}
		http.NotFound(w, r)
	case http.MethodDelete:
		if err := h.adapter.Remove(name); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "adapter not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "name": name})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"network_status": "monitoring",
		"endpoints":      h.config.Get().Defaults.LLMEndpoints,
	})
}

func (h *Handler) handleTracker(w http.ResponseWriter, r *http.Request) {
	if h.tracker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "tracker not available"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/tracker")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "changesets" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"changesets": h.tracker.ChangeSets(),
			"count":      len(h.tracker.ChangeSets()),
		})
	case path == "rollback" && r.Method == http.MethodPost:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "rollback_complete"})
	case path == "" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"stats":       h.tracker.Stats(),
			"has_changes": h.tracker.HasChanges(),
			"changesets":  len(h.tracker.ChangeSets()),
		})
	case path == "" && r.Method == http.MethodDelete:
		if err := h.tracker.Rollback(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "cleared"})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
