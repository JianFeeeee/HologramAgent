package knowledgeplugin

// http.go —— 知识库的**读写 HTTP 面**（原 kbtree 的 5 条只读路由 + 写面）。
//
// ## 为什么合进插件（用户裁定 §九）
//
// 「kbtree（531 行）**合入**本插件」＋「webui / proc 改走 HTTP 面
//  （配置读 `s.Config()` 的 listen_addr/token；大正文走临时文件传路径）」
//
// kbtree 曾经是**独立插件**，靠 `s.Knowledge()` 反向问内核。降为内部插件后
// 那个反向依赖必须消失：本文件直接用自己持有的 `*Store`。
//
// ## 读 vs 写：两种暴露面
//
//	读（默认开启，仅本机）     /tree /categories /counts /search
//	写（**默认关闭**）         POST /write（大正文走临时文件路径，见下）
//
// "写面默认仅本机"是刻意的：外部 agent 读知识库就够了；写入要决定分类归属
// 与媒体处理，让外部自行拼装容易造出越界/重名条目 —— 写留给内核工具。
// 需要时由运维显式打开 `allow_remote_write`。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HTTPServer 是知识库对外的只读（可选可写）HTTP 面。
type HTTPServer struct {
	store *Store
	scope *scope
	addr  string
	token string

	srv *http.Server
	mux *http.ServeMux
	// allowWrite 是写面开关（默认 false，见文件头）。
	allowWrite bool
	// scratchDir 是大正文落盘的临时目录（写面用）。
	scratchDir string
}

// NewHTTPServer 装配 HTTP 面（未 Start 前不占端口）。
func NewHTTPServer(store *Store, expose string, addr, token string, allowWrite bool) *HTTPServer {
	if addr == "" {
		addr = defaultAddr
	}
	if token == "" {
		token = genToken()
		log.Printf("[knowledge] http: no token configured, generated one for this run " +
			"(invalidated on restart)")
	}
	return &HTTPServer{
		store:      store,
		scope:      newScope(expose),
		addr:       addr,
		token:      token,
		mux:        http.NewServeMux(),
		allowWrite: allowWrite,
	}
}

// defaultAddr 默认只监听本机（与 kbtree 一致）。
const defaultAddr = "127.0.0.1:9892"

// Addr 返回实际监听地址（Start 之后用于日志与配置回显）。
func (h *HTTPServer) Addr() string { return h.addr }

// Start 起服务（gRPC 无关，纯 HTTP；失败返回错误而不是静默）。
func (h *HTTPServer) Start() error {
	if h == nil || h.store == nil {
		return fmt.Errorf("knowledge http: store 不可用")
	}
	h.registerRoutes()
	h.srv = &http.Server{
		Addr:              h.addr,
		Handler:           h.mux,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris 防护
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ln, err := net.Listen("tcp", h.addr)
	if err != nil {
		return fmt.Errorf("knowledge http: listen %s: %w", h.addr, err)
	}
	h.addr = ln.Addr().String()
	go func() {
		if err := h.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[knowledge] http serve error: %v", err)
		}
	}()
	log.Printf("[knowledge] read-only HTTP face on http://%s (GET /tree, /categories, /counts, /search)", h.addr)
	if h.allowWrite {
		log.Printf("[knowledge] write face ENABLED on the same listener (POST /write)")
	}
	return nil
}

// Stop 停服务（幂等）。
func (h *HTTPServer) Stop() error {
	if h == nil || h.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return h.srv.Shutdown(ctx)
}

// registerRoutes 挂载全部路由（全部需 token）。
func (h *HTTPServer) registerRoutes() {
	h.mux.HandleFunc("/tree", h.authed(h.handleTree))
	h.mux.HandleFunc("/categories", h.authed(h.handleCategories))
	h.mux.HandleFunc("/counts", h.authed(h.handleCounts))
	h.mux.HandleFunc("/search", h.authed(h.handleSearch))
	if h.allowWrite {
		h.mux.HandleFunc("/write", h.authedWrite(h.handleWrite))
	}
	h.mux.HandleFunc("/", h.authed(h.handleRoot))
}

// authed 是**只读**路由的鉴权壳（GET only）。
func (h *HTTPServer) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.checkToken(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET（本面只读）"})
			return
		}
		next(w, r)
	}
}

// authedWrite 是**写面**的鉴权壳（POST only + 显式开关）。
func (h *HTTPServer) authedWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.allowWrite {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "写面未启用"})
			return
		}
		if !h.checkToken(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 POST"})
			return
		}
		next(w, r)
	}
}

// checkToken 校验令牌（三种携带方式都认，与 kbtree 一致）。
func (h *HTTPServer) checkToken(w http.ResponseWriter, r *http.Request) bool {
	k := r.Header.Get("X-API-Key")
	if k == "" {
		k = strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}
	if k == "" {
		k = r.URL.Query().Get("token")
	}
	if k == "" || k != h.token {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return false
	}
	return true
}

// handleRoot 自述端点（便于外部 agent 摸索）。
func (h *HTTPServer) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	endpoints := []string{
		"GET /tree?category=&depth=&items=    分类树（可指定子树/层数）",
		"GET /categories                      全部分类路径",
		"GET /counts                          各分类条目数（按数量倒序）",
		"GET /search?q=&category=&limit=      检索（category 前缀匹配子树）",
	}
	if h.allowWrite {
		endpoints = append(endpoints, "POST /write                         写入一条知识（大正文用 body_file 传路径）")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"service":   "knowledge",
		"purpose":   "HologramAgent 知识库的 HTTP 面（供外部 agent / webui / proc 使用）",
		"read_only": !h.allowWrite,
		"endpoints": endpoints,
		"auth":      "X-API-Key 头 或 Authorization: Bearer <token> 或 ?token=",
	})
}

// handleTree 返回分类树。
func (h *HTTPServer) handleTree(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opt := TreeOptions{
		MaxDepth:     intQ(q.Get("depth"), 0),
		IncludeItems: boolQ(q.Get("items"), true),
		PreviewLimit: intQ(q.Get("preview"), 0),
	}
	cat := strings.Trim(strings.TrimSpace(q.Get("category")), "/")
	view := h.store.Subtree(cat, opt)
	if view == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":      "分类不存在: " + cat,
			"categories": h.scope.filterNames(h.store.Categories()),
		})
		return
	}
	// 范围裁剪在服务端做：裁剪后响应里根本不含范围外条目。
	h.scope.filterTree(view)
	writeJSON(w, http.StatusOK, map[string]interface{}{"tree": view})
}

// handleCategories 返回平铺分类列表。
func (h *HTTPServer) handleCategories(w http.ResponseWriter, r *http.Request) {
	names := h.scope.filterNames(h.store.Categories())
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"categories": names})
}

// handleCounts 返回各分类条目数（倒序）。
func (h *HTTPServer) handleCounts(w http.ResponseWriter, r *http.Request) {
	counts, total := h.scope.filterCounts(h.store.CategoryCounts())
	if counts == nil {
		counts = []CategoryCount{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"counts": counts, "total": total})
}

// handleSearch 按分类 + 关键词检索。
func (h *HTTPServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q 必填"})
		return
	}
	limit := intQ(q.Get("limit"), 10)
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	cat := strings.Trim(strings.TrimSpace(q.Get("category")), "/")
	results := h.store.SearchIn(query, cat, limit)
	items := make([]map[string]interface{}, 0, len(results))
	for _, k := range results {
		// ★ 范围过滤必须在服务端：客户端过滤等于没过滤（全文已发出）。
		if !h.scope.allows(k.Category) {
			continue
		}
		items = append(items, map[string]interface{}{
			"name":     k.Name,
			"category": k.Category,
			"content":  k.Content,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"query":    query,
		"category": cat,
		"results":  items,
		"hint":     "结果按相关度排序；只采用第一条，第一条不相关请换分类或关键词",
	})
}

// writeRequest 是写面的请求体。
type writeRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
	// BodyFile 是大正文的**替代载体**：正文超过阈值时由调用方先落临时文件，
	// 这里传路径（见 §九「大正文走临时文件传路径」）。
	BodyFile string `json:"body_file,omitempty"`
}

// handleWrite 写一条知识（写面默认关闭）。
func (h *HTTPServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	content := req.Content
	if req.BodyFile != "" {
		data, err := readScratch(req.BodyFile)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		content = data
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name 与 content 必填"})
		return
	}
	if err := h.store.Add(req.Name, content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"name": req.Name, "chars": len(content)})
}

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// intQ 解析整数查询参数（缺省或非法 ⇒ def）。
func intQ(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// boolQ 解析布尔查询参数（缺省或非法 ⇒ def）。
func boolQ(s string, def bool) bool {
	if s == "" {
		return def
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return def
	}
	return b
}

// genToken 生成一次性令牌（未配置时使用）。
func genToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
