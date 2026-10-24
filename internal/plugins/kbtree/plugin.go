// Package kbtree 把知识库的**分类树**作为独立 HTTP 服务暴露给外部 agent。
//
// 定位（与 HologramAgent 内部知识工具的分工）：
//   - HologramAgent 自己的 agent **直接调内部方法**（knowledge_search /
//     knowledge_create 等内核工具），走的是进程内直调，最快、也最少攻击面。
//   - 其它 agent（别的进程、别的机器、别的语言写的）走本插件的 HTTP 接口。
//
// 为何独立成服务而不是复用 WebUI 的 /api/v1/knowledge*：
//  1. **不共享 WebUI 的鉴权与端口**。WebUI 的 api_key 是给人操作界面用的，
//     把它分发给外部 agent 等于把管理面凭据扩散出去。本服务用**独立 token**
//     且**独立端口**，可单独关闭。
//  2. **只读**。外部 agent 读知识库就够了；写入要决定分类归属与媒体处理，
//     让外部自行拼装反而容易造出越界/重名的条目 —— 写入留给内核工具。
//  3. 形状按树组织（分类导航、分类内检索、懒加载），而不是平铺的搜索接口。
//
// 配套的 assets/skills/knowledge-base/SKILL.md 是给 agent 读的指令文档，
// 两者配合即可"别的 agent 也能查这套知识库"。
package kbtree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// defaultAddr 默认只监听本机。外部 agent 若在别的机器，改配置显式对外，
// 并自行确认 token 的分发方式（见 RegisterDef 里的说明）。
const defaultAddr = "127.0.0.1:9892"

func init() {
	plugin.RegisterPluginMeta("kbtree", "知识库树服务", "Knowledge Tree Service")
	plugin.RegisterFactory("kbtree", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name   string
	sdkRef *sdk.PluginSDK
	addr   string
	token  string
	// expose 是暴露范围（nil/空 = 全部可见）。见 scope.go。
	expose *scope
	// kn 是知识库句柄的测试注入口（生产从 sdkRef 取）。
	kn     sdk.KnowledgeAPI
	server *http.Server
	mux    *http.ServeMux
	// 启动时未显式配置 token 则自动生成（与 remotedevice 同策略）
	generatedToken bool
}

func New(name string) *Plugin {
	return &Plugin{
		name: name,
		addr: defaultAddr,
		mux:  http.NewServeMux(),
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	if s == nil {
		return fmt.Errorf("kbtree: SDK 不可用")
	}
	p.sdkRef = s

	// Settings 缺失时不能直接 RegisterDef —— 那是空接口上的调用，会 panic。
	// 缺 Settings 意味着宿主装配不完整（测试环境或裁剪版内核），
	// 此时用默认值把服务跑起来，而不是让整个插件加载失败。
	if set := s.Settings(); set != nil {
		set.RegisterDef(sdk.ConfigDef{
			Key: "listen_addr", Default: defaultAddr, Type: "string",
			DisplayName: "监听地址", Category: "kbtree",
			Description: "知识库树服务 HTTP 监听地址（默认 127.0.0.1:9892，仅本机）。改为对外地址前请确认 token 分发方式",
		})
		set.RegisterDef(sdk.ConfigDef{
			Key: "token", Default: "", Type: "password",
			DisplayName: "访问令牌", Category: "kbtree",
			Description: "外部 agent 访问本服务所需的令牌；留空则启动时随机生成（仅本次运行有效）",
		})
		set.RegisterDef(sdk.ConfigDef{
			Key: "expose_categories", Default: "", Type: "string",
			DisplayName: "暴露范围", Category: "kbtree",
			Description: "逗号分隔的分类路径，只暴露这些分类及其子树（如 \"public,tech/go\"）。留空=全部可见。前缀按路径分段匹配：public 不会匹配 publication。根下无分类的条目在范围非空时不可见。",
		})
		if v, _ := set.Get("expose_categories"); v != nil {
			if str, ok := v.(string); ok {
				p.expose = newScope(str)
			}
		}
		if v, _ := set.Get("listen_addr"); v != nil {
			if a, ok := v.(string); ok && strings.TrimSpace(a) != "" {
				p.addr = strings.TrimSpace(a)
			}
		}
		if v, _ := set.Get("token"); v != nil {
			if tk, ok := v.(string); ok && strings.TrimSpace(tk) != "" {
				p.token = strings.TrimSpace(tk)
			}
		}
	}
	if p.token == "" {
		p.token = genToken()
		p.generatedToken = true
	}

	// 启动前确认知识库可用：不可用就别占着端口
	kn := s.Knowledge()
	if kn == nil {
		return fmt.Errorf("kbtree: 知识库不可用，未启动服务（避免占端口后只回 503）")
	}

	p.registerRoutes()
	p.server = &http.Server{
		Addr:              p.addr,
		Handler:           p.mux,
		ReadHeaderTimeout: 5 * time.Second, // Slowloris 防护
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		if err := p.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[kbtree] 服务错误: %v", err)
		}
	}()

	if p.generatedToken {
		log.Printf("[kbtree] 已启动 http://%s（token 未配置，本次随机生成；重启后失效）", p.addr)
	} else {
		log.Printf("[kbtree] 已启动 http://%s", p.addr)
	}
	log.Printf("[kbtree] 外部 agent 可用：GET /tree、/categories、/counts、/search?q=&category=")
	return nil
}

func (p *Plugin) Stop() error {
	if p.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return p.server.Shutdown(ctx)
	}
	return nil
}

// registerRoutes 挂载只读路由。全部需 token。
func (p *Plugin) registerRoutes() {
	p.mux.HandleFunc("/tree", p.requireToken(p.handleTree))
	p.mux.HandleFunc("/categories", p.requireToken(p.handleCategories))
	p.mux.HandleFunc("/counts", p.requireToken(p.handleCounts))
	p.mux.HandleFunc("/search", p.requireToken(p.handleSearch))
	// 根路径给个自述，便于外部 agent 摸索
	p.mux.HandleFunc("/", p.requireToken(p.handleRoot))
}

func (p *Plugin) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-API-Key")
		if k == "" {
			k = r.Header.Get("Authorization")
			k = strings.TrimPrefix(k, "Bearer ")
			k = strings.TrimSpace(k)
		}
		if k == "" {
			k = r.URL.Query().Get("token")
		}
		if k == "" || k != p.token {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET（本服务只读）"})
			return
		}
		next(w, r)
	}
}

// handleRoot 自述端点：告诉调用方有哪些接口可用。
func (p *Plugin) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"service":   "kbtree",
		"purpose":   "HologramAgent 知识库分类树的只读访问（供外部 agent 使用）",
		"read_only": true,
		"endpoints": []string{
			"GET /tree?category=&depth=&items=          分类树（可指定子树/层数）",
			"GET /categories                            全部分类路径（含中间层）",
			"GET /counts                                各分类条目数（按数量倒序）",
			"GET /search?q=&category=&limit=            检索（category 前缀匹配子树）",
		},
		"auth":  "X-API-Key 头 或 Authorization: Bearer <token> 或 ?token=",
		"notes": "结果按相关度排序，只采用第一条；节点 name 是本级段名，path 是完整路径",
	})
}

// handleTree 返回分类树。
func (p *Plugin) handleTree(w http.ResponseWriter, r *http.Request) {
	kn := p.knowledge(w)
	if kn == nil {
		return
	}
	q := r.URL.Query()
	opt := sdk.KnowledgeTreeOptions{
		MaxDepth:     intQ(q.Get("depth"), 0),
		IncludeItems: boolQ(q.Get("items"), true),
		PreviewLimit: intQ(q.Get("preview"), 0),
	}
	cat := strings.Trim(strings.TrimSpace(q.Get("category")), "/")
	view, err := kn.Subtree(cat, opt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if view == nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error":      "分类不存在: " + cat,
			"categories": p.safeCategories(kn),
		})
		return
	}
	// 范围裁剪在服务端做：裁剪后响应里根本不含范围外条目，
	// 客户端无从察觉它们存在。
	p.expose.filterTree(view)
	writeJSON(w, http.StatusOK, map[string]interface{}{"tree": view})
}

// handleCategories 返回平铺分类列表。
func (p *Plugin) handleCategories(w http.ResponseWriter, r *http.Request) {
	kn := p.knowledge(w)
	if kn == nil {
		return
	}
	names := p.expose.filterNames(p.safeCategories(kn))
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"categories": names})
}

// handleCounts 返回各分类条目数（倒序）。
func (p *Plugin) handleCounts(w http.ResponseWriter, r *http.Request) {
	kn := p.knowledge(w)
	if kn == nil {
		return
	}
	counts, err := kn.CategoryCounts()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if counts == nil {
		counts = []sdk.KnowledgeCategoryCount{}
	}
	// 附总量：只数叶子分类，避免中间层重复计数（范围过滤后按过滤结果重算，
	// 否则 total 会把范围外的条目数也报出去 —— 数量本身也是信息泄露）
	counts, total := p.expose.filterCounts(counts)
	writeJSON(w, http.StatusOK, map[string]interface{}{"counts": counts, "total": total})
}

// handleSearch 按分类 + 关键词检索。
func (p *Plugin) handleSearch(w http.ResponseWriter, r *http.Request) {
	kn := p.knowledge(w)
	if kn == nil {
		return
	}
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
	results, err := kn.SearchIn(query, cat, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// ★ 范围过滤必须在这里（服务端）。只在前端/客户端过滤等于没过滤：
	//   范围外条目全文已经随响应发出去了。
	//   同时它也修正了 limit 语义 —— 范围外条目不占名额，范围内的
	//   条目不会因为 limit 被范围外条目挤掉而漏掉。
	items := make([]map[string]interface{}, 0, len(results))
	for _, k := range results {
		if !p.expose.allows(k.Category) {
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

// knowledge 取知识库；不可用时写 503 并返回 nil。
func (p *Plugin) knowledge(w http.ResponseWriter) sdk.KnowledgeAPI {
	// kn 是测试注入口，优先于 sdkRef（它在 sdkRef 之前就已经有值了）
	if p.kn != nil {
		return p.kn
	}
	if p.sdkRef == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "plugin not started"})
		return nil
	}
	kn := p.sdkRef.Knowledge()
	if kn == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge not available"})
		return nil
	}
	return kn
}

func (p *Plugin) safeCategories(kn sdk.KnowledgeAPI) []string {
	names, err := kn.Categories()
	if err != nil {
		return nil
	}
	return names
}

// isParentCategory 判断 c 是否是别的分类的前缀（即中间层）。
func isParentCategory(counts []sdk.KnowledgeCategoryCount, c string) bool {
	for _, o := range counts {
		if o.Category != c && strings.HasPrefix(o.Category, c+"/") {
			return true
		}
	}
	return false
}

func genToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

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

func boolQ(s string, def bool) bool {
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
