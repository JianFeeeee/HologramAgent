package webui

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta("webui", "Web 控制台", "WebUI")
	plugin.RegisterFactory("webui", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return New(name), nil
	})
}

type Plugin struct {
	name    string
	handler *Handler
	server  *http.Server
	mux     *http.ServeMux
}

func New(name string) *Plugin {
	return &Plugin{
		name: name,
		mux:  http.NewServeMux(),
	}
}

func randomSecret(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// webFilesDir 是 agent 向 webui 发送文件时的中转目录（<data>/webui_files）。
// 由插件 Start 时从 daemon.data_dir 推导注入。
var webFilesDir string

// webDataDir 是 <data> 根目录（同上推导），供聊天记录等路径解析使用。
var webDataDir string

// uploadsDir 是用户经 webui 上传文件的存储目录（<data>/uploads）。
// handleChatFile 落盘、handleUploads 下载共用；参考 qq 插件 files_dir 收文件设计。
var uploadsDir string

// listenOverride 是内核在插件加载前给出的监听地址覆盖（CLI --webui，
// 或核心配置 webui.listen_addr 被显式改成非默认值）。
//
// 为什么需要这个旁路：内核曾在插件加载前写 settings["addr"]，但那时
// config_<name> 表还没建，PluginSettings.Set 的 INSERT 会失败且错误被忽略；
// 随后 plugin Start 里 RegisterDef 才建表并写入默认值 :8080。结果是
// CLI --webui 与 webui.listen_addr **一直是死配置**。这里改为插件自己
// 接受一个显式覆盖值，优先级高于 settings["addr"]（后者是 Web 设置页的持久值）。
var listenOverride string

// SetListenOverride 设置监听地址覆盖（空值表示不覆盖）。
// 由 cmd/homed 在插件加载前调用，见 resolveWebUIOverride。
//
// 测试也用它在加载内置插件前把地址指到 127.0.0.1:0：
// 否则测试会绑生产端口 :8080，与线上实例互相抢（见 integration_test.go）。
func SetListenOverride(addr string) {
	listenOverride = strings.TrimSpace(addr)
}

// ListenOverride 返回当前的覆盖值（空串表示未覆盖）。
// 供调用方「保存-还原」用，避免测试互相污染。
func ListenOverride() string { return listenOverride }

// resolveListenAddr 决定最终监听地址：覆盖值 > 插件设置 > 内置默认。
// 抽成纯函数是为了能被单测直接钉住优先级。
func resolveListenAddr(setting string) string {
	addr := ":8080"
	if setting != "" {
		addr = setting
	}
	if listenOverride != "" {
		addr = listenOverride
	}
	return addr
}

// stageWebFile 把 agent 要发送的本地文件拷贝到 webui_files 中转目录，
// 返回可下载 URL 路径与字节数。image/file 的 payload 支持本地路径或 http(s) URL
// （URL 直接透传给前端，不落盘）。文件名用随机 UUID 防路径猜测，扩展名保留自源文件。
func stageWebFile(payload string, isImage bool) (url string, size int64, err error) {
	if strings.HasPrefix(payload, "http://") || strings.HasPrefix(payload, "https://") {
		return payload, 0, nil // 远程 URL 直接透传
	}
	if webFilesDir == "" {
		return "", 0, fmt.Errorf("webui files dir not initialized")
	}
	src := payload
	if _, err := os.Stat(src); err != nil {
		return "", 0, fmt.Errorf("文件不存在: %s", src)
	}
	if err := os.MkdirAll(webFilesDir, 0755); err != nil {
		return "", 0, fmt.Errorf("create webui_files: %w", err)
	}
	buf := make([]byte, 8)
	rand.Read(buf)
	ext := strings.ToLower(filepath.Ext(src))
	if extBad(ext) {
		ext = ".bin"
	}
	name := hex.EncodeToString(buf) + ext
	dst := filepath.Join(webFilesDir, name)
	in, err := os.Open(src)
	if err != nil {
		return "", 0, fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return "", 0, fmt.Errorf("create dest: %w", err)
	}
	defer out.Close()
	n, err := io.Copy(out, in)
	if err != nil {
		os.Remove(dst)
		return "", 0, fmt.Errorf("copy: %w", err)
	}
	return "/files/" + name, n, nil
}

// extBad 过滤危险/无意义扩展名（防双扩展名绕过 Content-Type）。
func extBad(ext string) bool {
	switch ext {
	case "", ".html", ".htm", ".svg", ".js", ".exe", ".sh", ".bat", ".cmd", ".ps1":
		return true
	}
	return false
}

func (p *Plugin) ensureAuthBootstrap(s *sdk.PluginSDK) {
	sett := s.Settings()
	if sett == nil {
		return
	}
	if v, _ := sett.Get("username"); v == nil || fmt.Sprint(v) == "" {
		_ = sett.Set("username", "admin")
	}
	if v, _ := sett.Get("password"); v == nil || fmt.Sprint(v) == "" {
		pw := randomSecret(12)
		_ = sett.Set("password", pw)
		log.Printf("[webui] bootstrap password generated for user admin: %s", pw)
	}
	if v, _ := sett.Get("api_key"); v == nil || fmt.Sprint(v) == "" {
		key := randomSecret(16)
		_ = sett.Set("api_key", key)
		log.Printf("[webui] bootstrap api_key generated: %s", key)
	}
	if v, _ := sett.Get("session_ttl_hours"); v == nil || fmt.Sprint(v) == "" {
		_ = sett.Set("session_ttl_hours", "24")
	}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
	s.SetAutoRestart(true)

	// 中转目录：<data>/webui_files，agent 发送 image/file 时拷贝至此
	if dd, err := s.Settings().GetCore("daemon.data_dir"); err == nil {
		if s2, ok := dd.(string); ok && s2 != "" {
			webDataDir = s2
			webFilesDir = filepath.Join(s2, "webui_files")
			uploadsDir = filepath.Join(s2, "uploads")
		}
	}

	addr := ":8080"
	if v, _ := s.Settings().Get("addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			addr = s2
		}
	}
	addr = resolveListenAddr(addr)

	// 能力位 7 = CapText|CapFile|CapImage；旧值 1 仅文本，agent 无法向 webui 发文件/图片
	// 入站通道：webui（控制台对话）与 http（外部 HTTP 注入），都由本插件注入输入。
	// http 通道还声明 NoMemory：外部抓来的内容不进记忆计算（见 handler 里的 NoMemory 注入）。
	_ = s.RegisterInputChannel("webui", sdk.ChannelDef{})
	_ = s.RegisterInputChannel("http", sdk.ChannelDef{NoMemory: true})
	s.RegisterOutputChannel("webui", 7, "Web 控制台（支持文字/图片/文件，图片内联展示、文件可下载）", sdk.ChannelDef{}, func(args map[string]interface{}) (interface{}, error) {
		payload, _ := args["payload"].(string)
		rawType, _ := args["type"].(string)
		if payload == "" {
			return nil, fmt.Errorf("payload 不能为空")
		}
		// 能力位：CapText|CapFile|CapImage = 1|2|4 = 7（旧值 1 仅文本）。
		// image/file 时 payload 为本地路径（或 http URL），拷贝到 webui_files
		// 并经 /files/ 带鉴权下发；前端按 kind 渲染图片预览/文件下载卡片。
		if rawType == "image" || rawType == "file" {
			url, size, err := stageWebFile(payload, rawType == "image")
			if err != nil {
				return nil, err
			}
			s.Publish(&sdk.Event{
				Type: sdk.EventAgentOutput,
				Payload: map[string]interface{}{
					"content":     payload,
					"channel":     "webui",
					"kind":        "channel_output",
					"output_type": rawType,
					"url":         url,
					"size":        size,
				},
			})
			return map[string]interface{}{"status": "ok", "url": url, "size": size}, nil
		}
		s.Publish(&sdk.Event{
			Type: sdk.EventAgentOutput,
			Payload: map[string]interface{}{
				"content": payload,
				"channel": "webui",
				"kind":    "channel_output",
			},
		})
		return map[string]interface{}{"status": "ok"}, nil
	})

	s.Settings().RegisterDef(sdk.ConfigDef{Key: "addr", Default: ":8080", Type: "string", DisplayName: "监听地址", Description: "Web 控制台监听地址", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "history_file", Default: "", Type: "string", DisplayName: "聊天记录文件", Description: "聊天记录存放路径。留空 = <data>/webui_chat_history.json；相对路径按 data 目录解析（可指向独立挂载盘）", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "trusted_proxies", Default: "", Type: "string", DisplayName: "受信反代网段", Description: "逗号分隔的 CIDR 或裸 IP（如 127.0.0.1,10.0.0.0/8）。只有来自这些网段的请求，其 X-Forwarded-For 才被采信用于登录限流计数。**经 frp/nginx 穿透到公网时必须配置**（反代通常就在本机 127.0.0.1），否则所有外部访问被视为同一来源，限流会误伤所有人。留空 = 不采信任何 XFF（保守默认）", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "api_key", Default: "", Type: "password", DisplayName: "API 密钥", Description: "访问 API 时需要的密钥", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "username", Default: "admin", Type: "string", DisplayName: "登录用户名", Description: "Web 控制台登录用户名", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "password", Default: "", Type: "password", DisplayName: "Web 控制台登录密码", Description: "Web 控制台登录密码", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "session_ttl_hours", Default: "24", Type: "int", DisplayName: "会话时长(小时)", Description: "登录 cookie 有效时长", Category: "webui"})
	// ---- 通用反代（HologramAgent 自带能力）----
	// 基域名：默认 localhost ⇒ <标签>.localhost:<端口> 开箱即用（RFC 6761 强制
	// 解析到 loopback，无需 DNS/证书/hosts）。远程访问时改成本机可达的域名，
	// 如 webui.example.com ⇒ <标签>.webui.example.com。
	// 外部入口 base URL：经 frp/nginx 穿透时，请求 Host 往往是内网地址或
	// 缺少协议信息，而生成给用户的链接必须是**外部可点的**。填这里即覆盖。
	// 例：https://homeagent.example.com —— 生成的链接一律用它做协议+主机。
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "base_url", Default: "", Type: "string", DisplayName: "外部入口 Base URL", Description: "经反代/穿透暴露给外部的完整入口地址（含协议），如 https://homeagent.example.com。留空则按请求推导（直连时正确；经多层网关时可能拼错）。填了它，所有生成的外部链接都用它", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "base_domain", Default: "localhost", Type: "string", DisplayName: "反代基域名", Description: "插件服务按子域反代时用的基域名（如 webui.example.com 则 <插件标签>.webui.example.com）。默认 localhost 只对本机浏览器有效。若外层未放行子域，请改用手填反代条目或路径挂载", Category: "webui"})
	// 手填反代条目（自动发现之外的补充）：每行 `<标签> <上游地址> [ws] [auth=none]`
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "proxy_routes", Default: "", Type: "text", DisplayName: "手填反代条目", Description: "每行一条：<子域标签> <上游地址> [ws] [auth=none|homeagent]。插件的 proxies 声明会自动发现，这里只用于补充未声明/第三方服务。例：grafana 127.0.0.1:3000", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_enabled", Default: "false", Type: "bool", DisplayName: "设备网关反代", Description: "启用后 /api/v1/device/* 反代到 remotedevice 插件（默认关闭，避免硬耦合）", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_addr", Default: "127.0.0.1:9890", Type: "string", DisplayName: "设备网关地址", Description: "remotedevice 插件的内部监听地址", Category: "webui"})
	s.Settings().RegisterDef(sdk.ConfigDef{Key: "device_gateway_token", Default: "", Type: "password", DisplayName: "设备网关令牌", Description: "访问 remotedevice 的 token（与 remotedevice 的 ws_token 一致）", Category: "webui"})
	p.ensureAuthBootstrap(s)
	// 注入设备网关反代配置（默认禁用；仅当用户开启时才挂载路由）
	if v, _ := s.Settings().Get("device_gateway_enabled"); v != nil {
		if s2, ok := v.(string); ok && s2 == "true" {
			deviceGatewayEnabled = true
		}
	}
	if v, _ := s.Settings().Get("device_gateway_addr"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			deviceGatewayAddr = s2
		}
	}
	if v, _ := s.Settings().Get("device_gateway_token"); v != nil {
		if s2, ok := v.(string); ok && s2 != "" {
			deviceGatewayToken = s2
		}
	}

	s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "pre_action", "message": "thinking"}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeToolcall, func(ctx *sdk.StageContext) error {
		tool := ""
		if len(ctx.ToolCalls) > 0 {
			tool = ctx.ToolCalls[0].Name
		}
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "before_toolcall", "tool": tool, "message": "tool:" + tool}})
		return nil
	})
	s.RegisterStage(sdk.StageBeforeOutput, func(ctx *sdk.StageContext) error {
		s.Publish(&sdk.Event{Type: sdk.EventStage, Payload: map[string]interface{}{"phase": "before_output", "message": "output"}})
		return nil
	})

	// 反代接线：手填条目 + 自动发现回调。
	// 自动发现读插件目录里的 plugin.json（webui 已能拿到该目录），因此
	// **无需给内核接口加方法**即可发现声明，插件进程没起来也照样可见。
	if v, _ := s.Settings().Get("proxy_routes"); v != nil {
		if raw, ok := v.(string); ok {
			SetManualProxyRoutes(raw)
		}
	}
	if pm := s.PluginMgr(); pm != nil {
		dir := pm.PluginDir()
		SetProxyDeclProvider(func() []proxyDecl { return readPluginProxyDecls(dir) })
	}
	InvalidateProxyRoutes()

	p.handler = NewHandler(s)
	// 受信反代网段：限流要按真实客户端隔离，就得以可信方式拿到客户端 IP。
	// 穿透部署下反代通常就在本机 127.0.0.1（外部请求的 RemoteAddr 全是它），
	// 不配置的话所有人共用一个桶，限流会误伤所有人 —— 生产上踩过。
	p.handler.trustedProxies = parseTrustedProxies(settingString(s.Settings(), "trusted_proxies"))
	p.handler.RegisterRoutes(p.mux)

	// 最外层套 logged 中间件：记录每个请求的来源 IP / 方法 / 路径 / 认证方式 / 状态码。
	// 用于排查“谁调用了什么接口”（如插件禁用等变更操作）。
	//
	// 同步 Listen：端口被占时必须**在这里**失败并把错误交回加载器，
	// 而不是“后台 goroutine 里报一行日志、插件仍被当成加载成功”。
	// 修复前 Start 总是返回 nil，于是 :8080 被占时 WebUI 静默死亡，
	// 调用方看不到任何失败信号。
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("webui: 监听 %s 失败: %w", addr, err)
	}
	// 记录实际监听端口：「服务入口」链接必须带同一端口（单端口穿透的前提）。
	if _, port, err := net.SplitHostPort(ln.Addr().String()); err == nil {
		p.handler.hostPort = ":" + port
	}
	// 超时配置。为什么不"全设上"（读侧全设、写侧全不设）：
	//
	// 读侧必须有超时，否则 Slowloris —— 攻击者只占连接不发完整请求头，
	// 每个连接挂几 KB。MaxHeaderBytes 限的是头部**大小**，"慢慢发"不占大小，
	// 因此不受它约束；几百个连接就能耗尽 fd。这里三个读侧超时分别覆盖：
	//   ReadHeaderTimeout —— 请求头必须在此时间内发完（Slowloris 的正解）
	//   ReadTimeout       —— 读完整请求（含 body）的预算，防慢速上传
	//   IdleTimeout       —— keep-alive 空闲连接（另外两个都管不到）
	//
	// 写侧**刻意不设**：webui 有长连接 SSE（/api/v1/chat/events）与可跑
	// 300s 的流式 /v1/chat/completions。WriteTimeout 是"从请求开始到响应
	// 写完"的**总预算**，会把它们腰斩（表现为 SSE 每隔一段时间断一次、
	// 前端疯狂重连）—— 这类回归很难在功能测试里立刻发现，所以有专门
	// 的反向判据钉住它必须保持为 0。
	p.server = &http.Server{
		Handler:           p.handler.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout 保持 0（见上方说明）
	}
	go func() {
		if err := p.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[webui] server error: %v", err)
		}
	}()
	log.Printf("[webui] HTTP server listening on %s", ln.Addr())
	return nil
}

func (p *Plugin) Stop() error {
	// 先停聊天记录写盘协程并落最后一次，再关服务器：
	// 写盘是节流的（chatSaveThrottle），不显式关会丢掉最后一轮对话。
	if p.handler != nil {
		p.handler.Close()
	}
	if p.server != nil {
		return p.server.Close()
	}
	return nil
}
