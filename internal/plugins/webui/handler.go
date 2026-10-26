package webui

import (
	"bufio"
	"embed"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/JianFeeeee/HologramAgent/internal/pluginview"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"net/http"
	"path"
)

// 本文件是 WebUI 的骨架：嵌入式前端资源、Handler 结构、构造、路由表、
// 鉴权/会话/日志中间件与静态页。各资源的具体 handler 见同包 handler_*.go。

//go:embed dashboard.html dashboard.css dashboard.js mascot.webp logo.svg
var dashboardFS embed.FS

// static/ 放前端第三方库（three.js / OrbitControls / marked / DOMPurify）。
//
// ★ 为什么必须 vendor 而不是走 CDN（这是星图「看不了」的直接原因）：
//
//	dashboard.html 原先从 cdnjs / jsdelivr 拉这四个库。用户点「星图」
//	看到「3D 星图不可用（CDN 加载失败）」—— 服务端 curl 同一 URL 是 200，
//	说明是**浏览器**访问不到公网 CDN（内网 / 出口受限 / 断网）。
//
//	换 CDN 只是把同一个赌注重下一遍：HologramAgent 明确支持离线与内网部署，
//	前端却有 4 个硬依赖在公网上，断网即坏且用户无从修复。
//
//	连带的两个理由：
//	- XSS：DOMPurify 是净化 Markdown 的关键一环，它挂了前端会退化到
//	  「不净化」分支（见 dashboard.js 的 typeof 检查）—— 那是安全降级。
//	- 体积：四个库共 ~700KB，embed 进二进制后由本服务同源提供，
//	  省掉 4 个跨域握手，也不再受第三方可用性影响。
//
//go:embed static
var staticFS embed.FS

var dashboardHTML string

const loginHTML = `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="UTF-8"><meta name="viewport" content="width=device-width,initial-scale=1.0"><title>HologramAgent 登录</title><style>
:root{--sakura-300:#ffb3c8;--sakura-400:#ff7fac;--sakura-500:#f33b7c;--frost-300:#88c0d0;--text-primary:#e8e6ee;--text-secondary:#a0a3b5;--text-muted:#6e7284;--bg-primary:#0d0d16;--bg-card:rgba(24,24,38,0.72);--bg-input:rgba(13,13,22,0.6);--border-color:rgba(255,255,255,0.09);--glass-border:rgba(255,255,255,0.12);--glass-blur:20px;--radius-lg:18px;--radius-md:12px;--radius-pill:999px;--shadow-glow:0 0 18px rgba(243,59,124,0.35);--ease-out:cubic-bezier(.22,.61,.36,1)}
[data-theme="light"]{--text-primary:#23252e;--text-secondary:#5b5f73;--text-muted:#9aa0b5;--bg-primary:#f6f3f8;--bg-card:rgba(255,255,255,0.72);--bg-input:rgba(255,255,255,0.8);--border-color:rgba(35,37,46,0.1);--glass-border:rgba(255,255,255,0.75);--shadow-glow:0 0 18px rgba(243,59,124,0.28)}
*{box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI','PingFang SC','Microsoft YaHei',sans-serif;background:radial-gradient(1200px 800px at 15% 0%,rgba(243,59,124,.22),transparent 55%),radial-gradient(1000px 700px at 90% 10%,rgba(136,192,208,.18),transparent 55%),radial-gradient(900px 600px at 50% 110%,rgba(163,184,255,.14),transparent 60%),var(--bg-primary);background-attachment:fixed;color:var(--text-primary);display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;padding:20px;transition:background .3s,color .2s;overflow:hidden}
body::before{content:'';position:fixed;inset:0;pointer-events:none;background-image:radial-gradient(rgba(255,255,255,.05) 1px,transparent 1px);background-size:28px 28px}
.login-wrap{width:100%;max-width:400px;position:relative;z-index:1}
.login-card{background:var(--bg-card);backdrop-filter:blur(var(--glass-blur)) saturate(1.4);-webkit-backdrop-filter:blur(var(--glass-blur)) saturate(1.4);border:1px solid var(--glass-border);border-radius:var(--radius-lg);padding:36px 32px 28px;box-shadow:0 20px 60px rgba(0,0,0,.45)}
.logo{width:84px;height:84px;margin:0 auto 16px;border-radius:50%;overflow:hidden;border:2px solid rgba(255,255,255,.25);box-shadow:0 8px 24px rgba(243,59,124,.35);background:#F8FAFC;display:flex;align-items:center;justify-content:center}
.logo img{width:100%;height:100%;object-fit:cover;display:block}
h1{margin:0 0 6px;font-size:22px;font-weight:700;text-align:center;letter-spacing:-.01em;background:linear-gradient(120deg,var(--sakura-400),var(--frost-300));-webkit-background-clip:text;background-clip:text;-webkit-text-fill-color:transparent}
.sub{margin:0 0 24px;font-size:12px;color:var(--text-muted);text-align:center}
label{display:block;font-size:11px;color:var(--text-secondary);margin:14px 0 6px;font-weight:500;letter-spacing:.03em}
input{width:100%;padding:11px 14px;border-radius:var(--radius-md);border:1px solid var(--border-color);background:var(--bg-input);color:var(--text-primary);font-size:14px;outline:none;transition:border .15s,box-shadow .15s}
input:focus{border-color:var(--sakura-400);box-shadow:var(--shadow-glow)}
input::placeholder{color:var(--text-muted)}
button{width:100%;margin-top:22px;padding:12px 14px;border:none;border-radius:var(--radius-md);background:linear-gradient(120deg,var(--sakura-500),var(--sakura-400));color:#fff;font-size:14px;font-weight:600;cursor:pointer;letter-spacing:.08em;transition:transform .15s var(--ease-out),box-shadow .2s,filter .2s}
button:hover{transform:translateY(-1px);filter:brightness(1.08);box-shadow:0 8px 24px rgba(243,59,124,.4)}
button:active{transform:translateY(0) scale(.98)}
button:disabled{opacity:.6;cursor:not-allowed;transform:none}
.err{margin-top:14px;color:#ff6b6b;font-size:13px;text-align:center;min-height:18px;transition:opacity .2s}
.foot{margin-top:20px;font-size:11px;color:var(--text-muted);text-align:center}
.foot svg{width:12px;height:12px;vertical-align:-2px}
@media(max-width:480px){.login-card{padding:28px 22px 22px}}
</style></head><body><div class="login-wrap"><div class="login-card"><div class="logo"><img src="/logo.svg" alt="HologramAgent"></div><h1>HologramAgent</h1><p class="sub">智能家居助手控制台</p><form id="login-form"><label>用户名</label><input id="username" autocomplete="username" placeholder="请输入用户名" required><label>密码</label><input id="password" type="password" autocomplete="current-password" placeholder="请输入密码" required><button type="submit" id="submit-btn">登 录</button><div id="err" class="err"></div></form></div><div class="foot">HologramAgent &middot; NapCat Theme</div></div><script>
(function(){var m=window.matchMedia('(prefers-color-scheme: light)');function apply(){document.documentElement.setAttribute('data-theme',m.matches?'light':'dark')}apply();m.addEventListener('change',apply)})();
document.getElementById('login-form').addEventListener('submit',async(e)=>{e.preventDefault();const username=document.getElementById('username').value.trim();const password=document.getElementById('password').value;const err=document.getElementById('err');const btn=document.getElementById('submit-btn');err.textContent='';if(!username||!password){err.textContent='请输入用户名和密码';return}btn.disabled=true;btn.textContent='登录中...';try{const r=await fetch('/api/v1/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username,password})});if(r.ok){location.href='/';return}let data={};try{data=await r.json()}catch(_){}err.textContent=data.error||'登录失败'}catch(_){err.textContent='网络错误,请重试'}finally{btn.disabled=false;btn.textContent='登 录'}});
document.getElementById('password').addEventListener('keydown',function(e){if(e.key==='Enter')document.getElementById('login-form').dispatchEvent(new Event('submit'))});
</script></body></html>`

// dashboardHTML 是组装好的控制台页面：dashboard.html 外壳 + dashboard.css + dashboard.js。
//
// 前端刻意没有构建链，所以拆分的办法是：外壳里留 {{DASHBOARD_CSS}} / {{DASHBOARD_JS}}
// 两个占位符，启动时把两个资产原样填回去——**发出的 HTML 与拆分前逐字节一致**，
// 但 6637 行的单文件变成「外壳 + 样式 + 脚本」三份，便于编辑与评审。
func init() {
	html, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		return
	}
	out := string(html)
	for _, a := range []struct{ placeholder, file string }{
		{"{{DASHBOARD_CSS}}", "dashboard.css"},
		{"{{DASHBOARD_JS}}", "dashboard.js"},
	} {
		body, err := dashboardFS.ReadFile(a.file)
		if err != nil {
			log.Printf("[webui] reading frontend asset %s failed: %v", a.file, err)
			continue
		}
		// 资产文件末尾的换行由占位符所在行自己的换行承担，避免多出空行。
		out = strings.Replace(out, a.placeholder, strings.TrimRight(string(body), "\n"), 1)
	}
	dashboardHTML = out
}

type Handler struct {
	chatStopOnce2 sync.Once // close 纪律：chatStop 的幂等守卫
	sdk           *sdk.PluginSDK
	supervisor    sdk.SupervisorAPI
	memory        sdk.MemoryAPI
	indexer       sdk.IndexerAPI
	adapter       sdk.AdapterAPI
	config        sdk.ConfigAPI
	startTime     time.Time
	textMem       sdk.TextMemoryAPI
	knowledge     sdk.KnowledgeAPI
	tracker       sdk.TrackerAPI
	settings      sdk.SettingsAPI
	// kernelCtl 是内核重启/关机通道。nil = 该部署没接（端点会明确报不可用）。
	kernelCtl sdk.KernelControlAPI
	pluginMgr sdk.PluginManager
	status    sdk.StatusAPI
	llm       sdk.LLMAPI

	// termView 是插件侧终端视图：订阅事件总线自己攒（内核不再维护终端语义）。
	// 见 internal/pluginview/terminal.go 的职责边界说明。
	termView   *pluginview.TerminalView
	termUnsubs []func()

	// hostPort 是 webui 实际监听的 ":port"（用于推导服务入口链接）。
	hostPort string

	// mux 由 RegisterRoutes 记下，供 Handler() 组装生产链。
	mux *http.ServeMux

	sessionMu sync.Mutex
	sessions  map[string]time.Time

	// loginLimiter 是登录入口的按来源失败计数（见 login_limiter.go）。
	// 门户可被穿透到公网，登录是唯一的口令入口，必须有滥用防护。
	loginLimiter *loginLimiter

	// trustedProxies 是受信反代网段（CIDR 或裸 IP）。
	// 只有来自这些网段的请求，其 X-Forwarded-For 才会被采信用于限流计数。
	// 为空 = 不采信任何 XFF（保守默认，见 login_limiter.go 的 sourceKey）。
	// 由设置项 webui.trusted_proxies 配置（逗号分隔）。
	trustedProxies []string

	sseEvents *sseEventRing // SSE 事件环状缓冲区，Last-Event-ID 重放用

	chatMu      sync.Mutex
	chatHistory []ChatMsg
	pendingIdx  int   // chatHistory 中正在进行的 assistant 消息索引，-1 表示无
	chatSeq     int64 // 已分配的最大序号；单调递增，作增量查询游标

	// history 是聊天记录的独立存储（默认 <data>/webui_chat_history.json，
	// 插件设置 history_file 可改）。
	history *historyStore

	// 聊天记录写盘节流：chatDirty 由 persistChatLocked 置位，
	// chatPersistLoop 合并连续变更后落盘（chatSaveThrottle 去抖 +
	// chatSaveMaxDelay 兜底），Close 前强制落最后一次。
	chatDirty      bool
	chatDirtySince time.Time
	chatSaveWake   chan struct{}
	chatStop       chan struct{}
	chatLoopDone   chan struct{}
	chatCloseOnce  sync.Once

	chatMsgMu    sync.Mutex
	chatMsgCache map[string]*chatMsgEntry // client_msg_id -> 首次处理结果
	chatMsgOrder []string                 // FIFO 淘汰序
}

func NewHandler(s *sdk.PluginSDK) *Handler {
	var (
		sup sdk.SupervisorAPI
		mem sdk.MemoryAPI
		idx sdk.IndexerAPI
		ad  sdk.AdapterAPI
		cfg sdk.ConfigAPI
		tm  sdk.TextMemoryAPI
		ks  sdk.KnowledgeAPI
		tr  sdk.TrackerAPI
		se  sdk.SettingsAPI
		pm  sdk.PluginManager
		st  sdk.StatusAPI
		llm sdk.LLMAPI
		// kc 是内核重启/关机通道；nil 表示该部署没接（端点会明确报不可用）。
		kc sdk.KernelControlAPI
	)
	if s != nil {
		sup, mem, idx = s.Supervisor(), s.Memory(), s.Indexer()
		ad, cfg = s.Adapter(), s.Config()
		tm, ks, tr = s.TextMemory(), s.Knowledge(), s.Tracker()
		se, pm = s.Settings(), s.PluginMgr()
		st, llm = s.Status(), s.LLM()
		kc = s.KernelControl()
	}
	h := &Handler{
		sdk:          s,
		supervisor:   sup,
		memory:       mem,
		indexer:      idx,
		adapter:      ad,
		config:       cfg,
		startTime:    time.Now(),
		textMem:      tm,
		knowledge:    ks,
		tracker:      tr,
		settings:     se,
		kernelCtl:    kc,
		pluginMgr:    pm,
		status:       st,
		llm:          llm,
		termView:     pluginview.NewTerminalView(),
		sessions:     make(map[string]time.Time),
		loginLimiter: newLoginLimiter(loginMaxFails, loginWindow),
		pendingIdx:   -1,
		chatMsgCache: make(map[string]*chatMsgEntry),
		sseEvents:    newSSEEventRing(200),
		// 聊天记录独立存储：默认 <data>/webui_chat_history.json，
		// 插件设置 history_file 可改（相对路径按 data 目录解析）。
		history: newHistoryStore(resolveHistoryFile(settingString(se, "history_file"), webDataDir)),

		chatSaveWake: make(chan struct{}, 1),
		chatStop:     make(chan struct{}),
		chatLoopDone: make(chan struct{}),
	}
	// 订阅事件总线攒终端视图（内核不再维护终端语义）：
	//   EventTerminalOutput —— agentcli 的输出与生命周期
	//   EventToolCall       —— cmd_run 历史、terminal_create/close 归并
	if s != nil {
		h.termUnsubs = append(h.termUnsubs,
			s.Subscribe(sdk.EventTerminalOutput, func(ev *sdk.Event) {
				h.termView.OnTerminalOutput(ev.Payload)
			}),
			s.Subscribe(sdk.EventToolCall, func(ev *sdk.Event) {
				h.termView.OnToolCall(ev.Payload)
			}),
		)
	}
	go h.chatPersistLoop()
	h.loadChatHistory()
	if s != nil {
		h.subscribeChatEvents()
	}
	return h
}

func (h *Handler) getWebUIConfig() (apiKey, username, password string, ttl time.Duration) {
	ttl = 24 * time.Hour
	if h.settings == nil {
		return
	}
	if v, _ := h.settings.Get("api_key"); v != nil {
		apiKey, _ = v.(string)
	}
	if v, _ := h.settings.Get("username"); v != nil {
		username, _ = v.(string)
	}
	if v, _ := h.settings.Get("password"); v != nil {
		password, _ = v.(string)
	}
	if v, _ := h.settings.Get("session_ttl_hours"); v != nil {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				ttl = time.Duration(n) * time.Hour
			}
		case string:
			if i, err := strconv.Atoi(n); err == nil && i > 0 {
				ttl = time.Duration(i) * time.Hour
			}
		}
	}
	if username == "" {
		username = "admin"
	}
	return
}

func (h *Handler) createSession() (string, time.Time, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	_, _, _, ttl := h.getWebUIConfig()
	expires := time.Now().Add(ttl)
	token := hex.EncodeToString(buf)
	h.sessionMu.Lock()
	h.sessions[token] = expires
	h.sessionMu.Unlock()
	return token, expires, nil
}

func (h *Handler) validSession(r *http.Request) bool {
	cookie, err := r.Cookie("homeagent_session")
	if err != nil || cookie.Value == "" {
		return false
	}
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	expires, ok := h.sessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(h.sessions, cookie.Value)
		return false
	}
	return true
}

func (h *Handler) validAPIKey(r *http.Request) bool {
	apiKey, _, _, _ := h.getWebUIConfig()
	if apiKey == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if got == "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(auth, "Bearer ") {
			got = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	return got != "" && got == apiKey
}

func (h *Handler) requireAPI(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey, _, _, _ := h.getWebUIConfig()
		if apiKey == "" && !h.validSession(r) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webui api_key not configured"})
			return
		}
		if h.validAPIKey(r) || h.validSession(r) {
			fn(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	}
}

func (h *Handler) requireWeb(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// API key 客户端（ArkTS/GUI 远程连接）与 cookie session 同等放行：
		// agent 输出的 /files/、/uploads/ 附件 URL 会被非浏览器客户端直接加载，
		// 它们没有也不应有 web 登录态。
		if h.validAPIKey(r) {
			fn(w, r)
			return
		}
		_, username, password, _ := h.getWebUIConfig()
		if username == "" || password == "" {
			http.Error(w, "webui username/password not configured", http.StatusServiceUnavailable)
			return
		}
		if h.validSession(r) {
			fn(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// clientIP 提取请求的真实客户端 IP。
// 优先取 X-Forwarded-For / X-Real-IP（反向代理/frp 场景），回退 RemoteAddr。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// 取第一个（最接近客户端的地址）
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// requestLog 每次请求的日志行（含 IP、认证方式、状态码）。
// 由 logged 中间件在请求完成后调用，authType 为 caller 预先判定。
func requestLog(r *http.Request, authType, ip string, status int, dur time.Duration) {
	// 认证方式判定（供排查谁调用了变更接口）
	if authType == "" {
		switch {
		case r.Header.Get("X-API-Key") != "" || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "):
			authType = "api-key"
		case func() bool { c, err := r.Cookie("homeagent_session"); return err == nil && c.Value != "" }():
			authType = "session"
		default:
			authType = "none"
		}
	}
	log.Printf("[webui] %s %s from=%s auth=%s status=%d (%s)", r.Method, r.URL.Path, ip, authType, status, dur.Round(time.Millisecond))
}

// logged 中间件：包装任意 handler，记录请求 IP / 方法 / 路径 / 认证方式 / 状态码。
// 置于最外层（mux 之上），覆盖所有路由（含登录页、静态资源）。
func (h *Handler) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 预判认证方式（在 requireAPI/requireWeb 之前的原始请求判定）
		authType := "none"
		switch {
		case h.validAPIKey(r):
			authType = "api-key"
		case h.validSession(r):
			authType = "session"
		}
		// SSE 长连接：不阻塞在完成时记录（连接可能持续很久），启动即记一条
		if strings.HasSuffix(r.URL.Path, "/chat/events") {
			requestLog(r, authType, clientIP(r), http.StatusOK, 0)
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		requestLog(r, authType, clientIP(r), sw.status, time.Since(start))
	})
}

// statusWriter 包装 ResponseWriter 以捕获响应状态码。
// SSE 等流式写入直接透传不做缓冲。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	return sw.ResponseWriter.Write(b)
}

func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sw *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// proxyDispatch 是**最外层**的 Host 分发中间件。
//
// 为什么不放在 mux 的 "/" 兜底里（这是实测踩出来的）：stdlib ServeMux 是
// 最长前缀优先，任何更具体的模式都会先命中。插件子域上的路径若与门户某条
// 路由同名（/api/v1/device/online 就是——门户为「旧路径反代」注册了
// /api/v1/device/），请求会被那条**面向门户**的路由截走：实测表现为 401，
// 且响应体是门户的 {"error":"unauthorized"} 而不是上游的响应，极难排查。
//
// 包在 mux 外层后，Host 判定先于任何路径匹配发生：插件子域整体交给反代，
// 主门户 Host 则原样下沉给 mux 走各自路由，两边互不干扰。
// Handler 返回**生产用的完整处理链**（外 → 内：Host 分发 → gzip → 日志 → mux）。
//
// 抽成一个方法而非在 plugin.go 里手写组合：测试必须能拿到与线上**逐字节
// 相同**的链，否则很容易测出错位的东西——本次就踩过：测 mux 而中间件挂在
// plugin.go，判据全绿却在真实实例上 401。共享同一条链可以结构性地避免
// 这类漂移。
func (h *Handler) Handler() http.Handler {
	// gzip 夹在 proxyDispatch 与 logged 之间：proxyDispatch 命中时直接
	// return，响应来自上游（其 Content-Encoding 由 httputil 处理），
	// 我们不插手；门户自身的全部响应则都被压缩。
	return h.proxyDispatch(gzipMW(h.logged(h.mux)))
}

func (h *Handler) proxyDispatch(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.serveProxyHost(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	h.mux = mux
	mux.HandleFunc("/login", h.handleLoginPage)
	mux.HandleFunc("/api/v1/login", h.handleLogin)
	mux.HandleFunc("/api/v1/logout", h.handleLogout)
	mux.HandleFunc("/api/v1/status", h.requireAPI(h.handleStatus))
	// 内核控制：重启 / 关机。
	//
	// 都要鉴权：能停服务的人必须是已认证的管理者。这两个端点比"读状态"
	// 危险一个量级 —— 关机之后只能 ssh 上去才能恢复。
	mux.HandleFunc("/api/v1/kernel/restart", h.requireAPI(h.handleKernelRestart))
	mux.HandleFunc("/api/v1/kernel/shutdown", h.requireAPI(h.handleKernelShutdown))
	// 内核更新：GET 检查 / POST 升级 —— 与 agent 工具、CLI 共享
	// internal/update 同一底层（见 kernel_update.go 头注释）。
	mux.HandleFunc("/api/v1/kernel/update", h.requireAPI(h.handleKernelUpdate))
	// 数据目录一致性（两处来源不等价，不一致会数据分裂；见 datadir_status.go）
	mux.HandleFunc("/api/v1/system/data-dir", h.requireAPI(h.handleDataDirStatus))
	mux.HandleFunc("/api/v1/agents", h.requireAPI(h.handleAgents))
	mux.HandleFunc("/api/v1/agents/", h.requireAPI(h.handleAgentByID))
	mux.HandleFunc("/api/v1/memory", h.requireAPI(h.handleMemory))
	mux.HandleFunc("/api/v1/memory/", h.requireAPI(h.handleMemory))
	mux.HandleFunc("/api/v1/memory/graph", h.requireAPI(h.handleMemoryGraph))
	// 星图活动端点：只回最近变动的实体（几百字节 ~ 几 KB），
	// 而不是每 10s 重拉 400KB 全量图谱。见 handleMemoryGraphPulse。
	mux.HandleFunc("/api/v1/memory/graph/pulse", h.requireAPI(h.handleMemoryGraphPulse))
	mux.HandleFunc("/api/v1/memory/context", h.requireAPI(h.handleMemoryContext))
	mux.HandleFunc("/api/v1/memory/tools", h.requireAPI(h.handleMemoryTools))
	mux.HandleFunc("/api/v1/memory/text", h.requireAPI(h.handleTextMemory))
	mux.HandleFunc("/api/v1/network", h.requireAPI(h.handleNetwork))
	mux.HandleFunc("/api/v1/config", h.requireAPI(h.handleConfig))
	mux.HandleFunc("/api/v1/settings", h.requireAPI(h.handleSettings))
	mux.HandleFunc("/api/v1/settings/reveal", h.requireAPI(h.handleSettingsReveal))
	mux.HandleFunc("/api/v1/settings/", h.requireAPI(h.handleSettings))
	mux.HandleFunc("/api/v1/knowledge", h.requireAPI(h.handleKnowledge))
	mux.HandleFunc("/api/v1/knowledge/", h.requireAPI(h.handleKnowledge))
	// 知识库的树形只读面（供外部 agent / skill 按分类导航）。
	// 路由顺序无关：Go 1.22+ 的 ServeMux 取**最长前缀匹配**
	// （/knowledge/ 只是子树通配，不会吃掉 /knowledge/tree）。go.mod 要求 1.25。
	mux.HandleFunc("/api/v1/knowledge/tree", h.requireAPI(h.handleKnowledgeTree))
	mux.HandleFunc("/api/v1/knowledge/tree/", h.requireAPI(h.handleKnowledgeTree))
	mux.HandleFunc("/api/v1/adapters", h.requireAPI(h.handleAdapters))
	mux.HandleFunc("/api/v1/adapters/", h.requireAPI(h.handleAdapterByID))
	mux.HandleFunc("/api/v1/tracker", h.requireAPI(h.handleTracker))
	mux.HandleFunc("/api/v1/tracker/", h.requireAPI(h.handleTracker))
	mux.HandleFunc("/api/v1/chat", h.requireAPI(h.handleChat))
	// 用户上传文件并附带消息注入 agent（multipart：file + message）
	mux.HandleFunc("/api/v1/chat/file", h.requireAPI(h.handleChatFile))
	mux.HandleFunc("/api/v1/chat/history", h.requireAPI(h.handleChatHistory))
	mux.HandleFunc("/api/v1/chat/interrupt", h.requireAPI(h.handleChatInterrupt))
	mux.HandleFunc("/api/v1/chat/events", h.requireAPI(h.handleChatEvents))
	mux.HandleFunc("/api/v1/terminals", h.requireAPI(h.handleTerminals))
	mux.HandleFunc("/api/v1/cmd/history", h.requireAPI(h.handleCmdHistory))
	mux.HandleFunc("/api/v1/kernel", h.requireAPI(h.handleKernel))
	// 运行态小快照：调度器（排队/四级中断队列/中断栈）+ 驻留子 + 通道拓扑。
	// 单独一条是为了让前端能秒级刷新而不必反复拉 30KB 的 /kernel。
	mux.HandleFunc("/api/v1/runtime", h.requireAPI(h.handleRuntime))
	mux.HandleFunc("/api/v1/persona", h.requireAPI(h.handlePersona))
	mux.HandleFunc("/api/v1/plugins", h.requireAPI(h.handlePlugins))
	mux.HandleFunc("/api/v1/plugins/", h.requireAPI(h.handlePluginByID))
	// 设备网关的**遗留路径反代**（装置开关 device_gateway_enabled 控制）。
	//
	// 鉴权**不再套 requireAPI**：该路由的调用方是设备与嵌入式客户端，
	// 它们带的是设备接入令牌（?token= / X-API-Key），不是门户凭证；套上
	// requireAPI 会把它们全部挡在 401（实测：waiter 经此路径升级握手 401）。
	// 上游 remotedevice 自己用 requireToken 强制校验令牌，安全性不降级。
	//
	// 声明了 path 的服务（新机制）会在最外层 Host 分发里更早命中，
	// 走不到这里；这条保留是为了「只开开关、不写声明」的老部署仍可用。
	mux.HandleFunc("/api/v1/device/", h.handleDeviceGatewayProxy)
	// agent 发送的文件下载（webui_files 中转目录；requireWeb 与 dashboard 同源同鉴权）
	mux.HandleFunc("/files/", h.requireWeb(h.handleFiles))
	// 用户上传文件的下载（uploads 目录，同一安全模型）
	mux.HandleFunc("/uploads/", h.requireWeb(h.handleUploads))
	mux.HandleFunc("/v1/chat/completions", h.requireAPI(h.handleOpenAICompletions))
	// /v1/models：几乎每个 OpenAI 客户端（curl 脚本、LangChain、OpenAI SDK、
	// IDE 插件）启动时都会先列模型来探测服务是否可用。没有它，客户端直接
	// 判定「服务不可用」而连试都不试 —— 这是最容易让集成方踩空的缺口。
	mux.HandleFunc("/v1/models", h.requireAPI(h.handleOpenAIModels))
	// 反代服务入口清单：给前端渲染「插件 UI」选项卡。
	// 走 requireAPI：清单本身含上游地址，属于管理面信息，不该匿名可读。
	mux.HandleFunc("/api/v1/proxy/services", h.requireAPI(h.handleProxyServices))
	mux.HandleFunc("/api/v1/proxy", h.requireAPI(h.handleProxyInfo))
	// 设备网关发现：客户端（GUI/鸿蒙/waiter）据此自动链接，不再自己拼地址。
	mux.HandleFunc("/api/v1/device/gateway", h.requireAPI(h.handleDeviceGatewayDiscovery))
	// 静态资源（页面本体 + 内嵌第三方库）**不要求认证**：
	// 登录页自身就要加载 CSS 与库才能渲染出来，若加认证则未登录时
	// 页面连样式都加载不到（503/401），用户看到的是「白屏 + 控制台报错」。
	// 这类资源不含任何用户数据，公开无害。
	mux.HandleFunc("/static/", h.handleStaticVendor)
	mux.HandleFunc("/", h.requireWeb(h.handleStatic))
}

func (h *Handler) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.validSession(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginHTML))
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 限流必须在**校验之前**：口令错误也要计数。否则爆破请求每次都走
	// 完整套校验（含配置读取），限流也就失去了保护意义。
	if !h.enforceLoginRateLimit(w, r) {
		return
	}
	_, username, password, _ := h.getWebUIConfig()
	if username == "" || password == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "webui username/password not configured"})
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	// 体积限制必须在解码**之前**生效，且不能只依赖解码器报错：
	//
	//   json.Decoder 是**按需读流**的。若请求体是「超大且非法 JSON」，
	//   解码器会在第 0 个字节就报语法错误，**永远不会读到上限**，
	//   于是 MaxBytesError 根本不会出现 —— 而 8MB 数据仍已被读入缓冲。
	//   那样「限体积」只对「合法到能继续解析的大 JSON」生效。
	//
	// 所以先用 ContentLength 快速拒绝（覆盖绝大多数真实攻击：直接发
	// 声明很大的 Content-Length），再用 MaxBytesReader 兜住分块传输
	// 与谎报 Content-Length 的情况。
	if r.ContentLength > loginBodyLimit {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "请求体过大"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "请求体过大"})
			return
		}
		// 解析到一半也可能撞上上限（合法 JSON 但超长），再兜一次。
		if r.ContentLength < 0 && bodyOverLimit(r) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "请求体过大"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	// 两条失败路径**必须**给完全相同的状态码与报文，否则可用于枚举用户名。
	// 用 constant-time 比较：== 会在第一个不同字节处短路，泄漏
	// 「猜对了几位」的时序信息（远程噪声大，但攻击者可多次采样取均值）。
	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) == 1
	if !userOK || !passOK {
		h.loginLimiter.Fail(h.sourceKey(r))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "用户名或密码错误"})
		return
	}
	// 成功即清零：惩罚只针对持续失败，手滑输错几次不该被记账。
	h.loginLimiter.Reset(h.sourceKey(r))
	token, expires, err := h.createSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "homeagent_session", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie("homeagent_session"); err == nil {
		h.sessionMu.Lock()
		delete(h.sessions, cookie.Value)
		h.sessionMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "homeagent_session", Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStaticVendor 提供内嵌的第三方前端库（/static/<file>）。
//
// 独立于 handleStatic 且**不走 requireWeb**：未登录时页面也要能加载这些库
// 才能渲染出登录框，否则用户看到白屏（比 401 更难自查）。
//
// 用 path.Base 挡目录穿越；文件不存在一律 404（而不是 200 + 空体 ——
// 空体在浏览器里的症状与 404 完全一样，但日志里更难看出是文件缺失）。
func (h *Handler) handleStaticVendor(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	if name == "" || name == "." || name == "/" {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// 内容不会变（vendored 固定版本），可长缓存；不用 immutable ——
	// 将来换库版本时同 URL 的强缓存会让客户端长期用旧文件。
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(data)
}

func (h *Handler) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Write([]byte(dashboardHTML))
		return
	}
	if r.URL.Path == "/mascot.webp" {
		data, err := dashboardFS.ReadFile("mascot.webp")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
		return
	}
	if r.URL.Path == "/logo.svg" {
		data, err := dashboardFS.ReadFile("logo.svg")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
		return
	}
	http.NotFound(w, r)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
