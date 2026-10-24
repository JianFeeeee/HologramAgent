package sdk

import (
	"net"
	"strconv"
	"strings"
)

// 反向代理声明：插件告诉 HologramAgent「我起了个 HTTP 服务，请把它反代出去」。
//
// 为什么需要：插件自带 Web UI / HTTP API 时，监听地址在插件自己的配置里
// （如 127.0.0.1:12100），外部无从得知；而 webui 的对外端口通常只有一个
// （默认 :8080，且常经 frp 单端口隧道穿透）。没有声明机制时，用户只能
// 「知道端口 + 自己配转发」，插件换端口就失效。
//
// 设计取舍——**声明式而非注册式**：声明写在 plugin.json 里，由 HologramAgent
// 在加载插件时读取聚合，而不是让插件在运行期调 API 注册。理由：
//  1. 静态可发现：未启动/已崩溃的插件，其服务声明依然可见（可给出准确报错
//     「插件 X 声明了 ui 但目标 127.0.0.1:12100 不可达」，而不是静默 404）；
//  2. 可版本化：声明随插件包一起分发、可 diff、可审计；
//  3. 旧内核无害：manifest 解析忽略未知字段，未支持该能力的 HologramAgent 读旧
//     插件、或旧 HologramAgent 读新插件都不会报错。
//
// 与 ToolDef / ChannelDef / ConfigDef 同族：SDK 定义声明契约，内核实现行为。
// 声明方式与其它能力一致 —— 在 Start() 里调 RegisterProxy(name, def)，
// 或写进 plugin.json 的 proxies 字段（外部插件两种都支持）。
//
// 安全性：**不声明 = 不被反代**。声明本身就是能力声明，因此不需要在
// capabilities 里另外开一个开关——最小权限默认生效。
//
// # 单一入口原则（强制要求）
//
// **一个声明 = 一个入口**。被反代的插件必须让它的全部资源与接口都能从
// 该入口的一个基准路径出发访问到，不得依赖「入口之外的根路径」。
//
// 为什么强制：反代有两种挂载形态，而它们对「根路径」的处理截然不同——
//
//	Host 形态（host）：插件独占 <标签>.<基域名>，根路径就是插件的根。
//	                  根绝对路径（fetch('/api/x')）**天然正确**。
//	Path 形态（path）：插件挂在门户自身 host 的某个前缀下，根路径属于**门户**。
//	                  此时插件里的 fetch('/api/x') 会打到门户自己的 /api/x
//	                  —— 静默错路由，页面能开但功能全坏。
//
// 于是「同一个插件必须同时支持两种形态」这条要求，等价于：
//
//	**插件内部一律使用相对路径**（或基于 <base>/location 推导的路径），
//	绝不硬编码以 / 开头的绝对路径。
//
// 这样同一份前端在两种形态下都正确，插件作者也不必知道自己被挂在哪。
// 反代层据此可以：外部子域可用时给 Host 形态，子域不可用（证书/放行限制）
// 时给 Path 形态，**无需插件配合改动**。
//
// 自检（插件作者在本地就该做）：把页面挂到 <门户>/<任意前缀>/ 下访问，
// 所有请求都必须仍然打到插件自己。
//
// 本项目实测案例：某插件前端写死 fetch('/api/status')，配在
// /p/huawei/ 下会打到门户的 /api/status（404 或返回门户数据）；
// 改成相对路径后两种形态同时可用。
// ProxyDef 是一个服务的**反代声明体**。
//
// 与 ToolDef 同构：Name 同时出现在字段与 RegisterProxy 的第一个参数里
// （ToolDef 也是这么做的 —— 字段供 plugin.json 序列化，参数供运行期调用）。
// Name 只用于展示、日志与冲突提示，**不参与路由**（路由键是 Host 与 Path）。
type ProxyDef struct {
	// Name 是同一插件内多条声明的唯一标识（如 "ui"、"api"）。
	// 运行期由 RegisterProxy 的第一个参数填入；声明式由 plugin.json 的
	// name 键填入。省略时由 HologramAgent 兜底为 "service"。
	Name string `json:"name,omitempty"`
	// Host 是**子域名标签**（不含基域名），如 "huawei" 对应 huawei.<基域名>。
	//
	// 约束：仅小写字母、数字与连字符，不以连字符开头/结尾，长度 ≤ 63
	// （DNS label 规则）。省略时默认取插件名（下划线转连字符，因为下划线
	// 不是合法 DNS label 字符）。
	//
	// 冲突处理：两个插件声明同一 Host 时，HologramAgent 不做「后者覆盖前者」——
	// 那样会让先声明者静默消失。冲突条目被拒绝并在反代表里记录原因。
	Host string `json:"host,omitempty"`

	// Target 是上游地址，形如 "127.0.0.1:12100" 或 "http://127.0.0.1:12100"。
	// 可带路径前缀（如 "127.0.0.1:3000/base"），HologramAgent 转发时保留该前缀。
	//
	// 端口由插件自己填它**实际监听**的地址，避免「声明与实际漂移」。
	Target string `json:"target"`

	// WebSocket 表示该服务需要 WebSocket 升级透传（默认 false）。
	//
	// 为什么必须显式声明而不是「有 Upgrade 头就转」：WS 是长连接，会占用
	// 反代侧连接与 goroutine，且绕过普通请求的响应缓冲/超时逻辑。默认关闭
	// 让普通 HTTP 服务的失败模式保持简单；未声明时的升级请求会被明确拒绝，
	// 而不是静默降级成普通请求（后者表现为前端一直重连、排查困难）。
	WebSocket bool `json:"websocket,omitempty"`

	// Path 是可选的**路径挂载前缀**（如 "/api/v1/device"）。
	//
	// 为什么 Host 子域之外还需要它：子域形态依赖 DNS 解析，而 *.localhost
	// 只有浏览器内置该特例（RFC 6761）—— 普通进程（设备客户端、固件、
	// CLI）走系统解析器，实测解析不到，会以「no such host」失败。
	// 路径形态挂在门户自身 host 下，**无任何 DNS 依赖**，是给非浏览器
	// 客户端用的。
	//
	// 语义：请求路径**原样保留**（不做前缀剥除）——声明者按上游真实路径填写，
	// 例如上游注册 /api/v1/device/ws，就声明 Path="/api/v1/device"。
	// 这样设备客户端可以直接使用它已硬编码的路径，不需要知道反代的存在。
	//
	// 与 Host 形态的关系（见包注释的「单一入口原则」）：声明的服务应当
	// **同时**能被两种形态访问。因此 Path 形态下插件内部必须用相对路径，
	// 否则它的前端会把请求打到门户自己身上。
	//
	// 留空 = 只提供子域形态（插件自带 UI 的常见情形：UI 与它自己的 API
	// 同源，走子域天然正确）。
	Path string `json:"path,omitempty"`

	// StripPath 决定转发前是否**剥掉** Path 前缀。默认 false（原样保留）。
	//
	// 两种挂载语义真实不同，必须由声明者选，不能靠猜：
	//
	//	false（别名模式）：Path 就是上游真实路径的一部分。
	//	  请求 /api/v1/device/ws + Path="/api/v1/device"
	//	  → 上游收到 /api/v1/device/ws（一模一样）。
	//	  适用：客户端**已硬编码**路径的机器接口（设备网关就是如此，
	//	  它按 /api/v1/device/ws 连接，不可能知道反代的存在）。
	//
	//	true（前缀模式）：Path 只是门户上的挂载点，上游不知道它。
	//	  请求 /p/myapp/api/status + Path="/p/myapp"
	//	  → 上游收到 /api/status。
	//	  适用：自带 UI 的服务（前端用相对路径，被挂到哪里都对）。
	//
	// 为什么不能自动判定：同一个声明「Path=/api/v1/device」在两种语义下
	// 都说得通，代理无从分辨 —— 猜错的结果是全部请求 404，且看起来像
	// 上游故障。所以由声明者显式写清楚。
	StripPath bool `json:"strip_path,omitempty"`

	// Auth 决定这条反代由谁保护，取值见 ProxyAuthNone / ProxyAuthHomeAgent。
	// 空串等价于 ProxyAuthHomeAgent（默认安全）。
	//
	// 为什么做成可声明项：设备网关（remotedevice）这类服务的调用方是**设备**，
	// 它们不可能持有浏览器会话 cookie，而服务自身已有接入令牌（如 ws_token）。
	// 强制走 HologramAgent 门户鉴权会把这类链路挡死；反过来，插件自带的 UI 若
	// 声明 none，就等于把管理界面裸露给任何能访问该端口的人。
	// 因此必须由插件**逐条**声明，而不是全局一刀切。
	Auth string `json:"auth,omitempty"`
}

// ProxyAuth 取值。空串按 ProxyAuthAgent 处理（安全的默认）。
const (
	// ProxyAuthAgent 表示由内核统一保护：浏览器走门户会话
	// （HologramAgent session cookie），非浏览器客户端走 X-API-Key。
	// 两者都没有时返回 401，而不是把请求透传给上游。
	ProxyAuthAgent = "agent"

	// ProxyAuthNone 表示不经内核鉴权，直接把请求转发给上游。
	//
	// 适用场景：上游自己有鉴权且调用方不是浏览器（设备/嵌入式客户端），
	// 或上游是刻意公开的服务。选用它意味着**信任上游自身的鉴权**，
	// 且该服务在网络层可达范围内对所有人开放。
	ProxyAuthNone = "none"

	// ProxyAuthAgentLegacy 是旧值的字面量，**只用于兼容读取**，不要在新声明里写它。
	//
	// 为什么必须继续接受（2026-10-22）：`Auth` 是写进 plugin.json 的**持久化
	// 声明值**，存量插件的配置文件里已经写着旧值（本机实测 huawei_smarthome 的
	// plugin.json:19 就是 "auth": "homeagent"）。删掉这个分支 ⇒ 那些插件在加载
	// 时被判为非法声明，反代直接不生效，且症状是"插件好好的但页面打不开"。
	//
	// 这正是 sdkModuleNames 处理 module 路径改名时的同一条教训：**协议值的
	// 改名必须保留旧值的读取路径**，否则存量数据无声失效。
	ProxyAuthAgentLegacy = "homeagent"
)

// ProxyAuthHomeAgent 是 ProxyAuthAgent 的旧名，仅为**存量源码**保留。
//
// Deprecated: 用 ProxyAuthAgent。存量插件的 plugin.json 里写的旧值仍被
// ValidProxyAuth/EffectiveProxyAuth 接受（见 ProxyAuthAgentLegacy）。
const ProxyAuthHomeAgent = ProxyAuthAgent

// ValidProxyAuth 校验 Auth 取值；空串合法（等价 ProxyAuthAgent）。
//
// 同时接受旧值 ProxyAuthAgentLegacy —— 存量 plugin.json 里写的仍是它。
func ValidProxyAuth(auth string) bool {
	switch auth {
	case "", ProxyAuthAgent, ProxyAuthNone, ProxyAuthAgentLegacy:
		return true
	}
	return false
}

// EffectiveProxyAuth 返回生效的鉴权模式（空串与旧值都归一化为 ProxyAuthAgent）。
//
// 归一化旧值是关键：下游一律只可能拿到 ProxyAuthAgent，不必各自兼容旧值
// （否则每个判断点都要写 `== ProxyAuthAgent || == ProxyAuthAgentLegacy`，
// 漏一处就是鉴权绕过）。
func EffectiveProxyAuth(auth string) string {
	if auth == "" || auth == ProxyAuthAgentLegacy {
		return ProxyAuthAgent
	}
	return auth
}

// ValidProxyHostLabel 校验子域名标签是否合法（DNS label 规则）。
//
// 独立成导出函数：插件作者在写声明时、HologramAgent 在加载时、工具链在打包时
// 都要用同一套规则判定，避免三处各写一份而互相不一致。
func ValidProxyHostLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// NormalizeProxyHost 由插件名派生默认 Host 标签。
//
// 下划线转连字符：插件名允许下划线（huawei_smarthome），但 DNS label 不允许，
// 直接用会导致该子域名无法解析——这里统一转换，避免每个插件各自碰运气。
func NormalizeProxyHost(pluginName string) string {
	s := strings.ToLower(strings.TrimSpace(pluginName))
	s = strings.ReplaceAll(s, "_", "-")
	// 去掉其它非法字符，保证结果是合法 label（宁可退化成保守值也不产出非法域名）
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b.WriteByte(c)
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "plugin"
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// ValidateProxyDef 校验一条反代声明，返回人类可读的错误说明（合法时为空）。
//
// 为什么要在 SDK 里做校验：HologramAgent 加载插件时必须能明确拒绝坏声明并说明
// 原因（而不是静默忽略导致用户以为配好了）；插件作者也需要在本地就能查出
// 拼错的 Target/Host。同一套规则两端共用。
func ValidateProxyDef(d ProxyDef) string {
	if strings.TrimSpace(d.Target) == "" {
		return "target 为空：必须给出上游地址（如 127.0.0.1:12100 或 http://127.0.0.1:12100）"
	}
	if !ValidProxyAuth(d.Auth) {
		return "auth 取值非法：" + d.Auth + "（只允许 \"\" / \"homeagent\" / \"none\"）"
	}
	if d.Host != "" && !ValidProxyHostLabel(d.Host) {
		return "host 不是合法的子域名标签（只允许小写字母/数字/连字符，且不以连字符开头结尾）: " + d.Host
	}
	if p := strings.TrimSpace(d.Path); p != "" {
		if !strings.HasPrefix(p, "/") {
			return "path 必须以 / 开头: " + d.Path
		}
		if strings.HasSuffix(p, "/") {
			return "path 不应以 / 结尾（它是前缀，不是目录）: " + d.Path
		}
		if strings.Contains(p, "..") || strings.ContainsAny(p, " \t\r\n\x00?#") {
			return "path 含非法字符: " + d.Path
		}
	}
	// 前缀模式必须给出可剥的前缀。
	// 注意 "/" 不需要单独判：它是前缀又同时以 "/" 结尾，已被上面的
	// 「不应以 / 结尾」规则挡掉（挂到门户根会覆盖整站的意图因此无法达成）。
	if d.StripPath && strings.TrimSpace(d.Path) == "" {
		return "strip_path=true 时必须给出 path（否则没有可剥的前缀）"
	}
	// Target 的 host:port 部分必须可解析；路径前缀允许保留。
	//
	// 规则（刻意从严，因为地址写错是最常见的声明错误，而错误的反代会把
	// 用户带到别处去）：
	//   - 带 scheme 时（http://…）允许省略端口，由反代层按 scheme 补默认值；
	//   - 不带 scheme 时必须给出 host:port；
	//   - 端口必须是数字（SplitHostPort 本身不校验数字，"host:abc" 会通过）。
	scheme := ""
	raw := d.Target
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme = strings.ToLower(raw[:i])
		if scheme != "http" && scheme != "https" {
			return "target scheme 只支持 http/https（WS 由 websocket 字段声明，不写 ws://）: " + d.Target
		}
		raw = raw[i+3:]
	}
	if i := strings.IndexByte(raw, '/'); i >= 0 {
		raw = raw[:i]
	}
	if raw == "" {
		return "target 缺少主机部分: " + d.Target
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		if scheme == "" {
			return "target 必须给出 host:port（或带 http:// 前缀以便省略端口）: " + d.Target
		}
		// 带 scheme 且解析失败：只剩主机名一种合法情形。
		host, port = raw, ""
	}
	if host == "" {
		return "target 缺少主机部分: " + d.Target
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "target 端口非法（应为 1-65535 的数字）: " + d.Target
		}
	}
	return ""
}

// ProxyRegistrar 由内核注入（与 ToolRegistrar / InputChannelRegistrar 同族）。
// 插件不直接调它，用 RegisterProxy。
//
// 为什么需要运行期注册（明明有 plugin.json 自动发现）：**内置插件**
// （编译进内核、没有独立插件目录与 plugin.json，如 remotedevice）扫不到；
// 而它们恰恰最需要被反代出去（设备网关就是内置的）。两种来源互补：
//   - 外部插件 → plugin.json 的 proxies（静态，未启动也可见）
//   - 内置插件 → RegisterProxy（运行期，随 Start 注册）
type ProxyRegistrar func(name string, def ProxyDef)

// SetProxyRegistrar 由内核注入。插件不直接调它（与 SetInputChannelRegistrar 同族）。
func (s *PluginSDK) SetProxyRegistrar(r ProxyRegistrar) {
	if s == nil {
		return
	}
	s.apiMu.Lock()
	s.proxyReg = r
	s.apiMu.Unlock()
}

// RegisterProxy 声明一个需要 HologramAgent 反代出去的服务。
//
// 与 RegisterTool / RegisterInputChannel / RegisterOutputChannel 同一风格：
// 显式给名字 + 声明体。名字用于展示、日志与冲突提示（不参与路由 —— 路由键是
// def.Host / def.Path）。
//
// 用法（通常在 Start 里调用）：
//
//	s.RegisterProxy("ui", sdk.ProxyDef{
//	    Host: "myapp", Target: "127.0.0.1:12100",
//	})
//
// 声明立即生效（反代表在下一次请求时重建）。**不做去重**：同一 Host/Path
// 被两条声明占用时由反代层判定冲突并明确报错，而不是在这里静默吞掉 ——
// 插件作者需要看见冲突。
//
// 与 plugin.json 的 proxies 字段等价：写哪个都行，两者会合并（同名以本调用为准）。
func (s *PluginSDK) RegisterProxy(name string, def ProxyDef) {
	if s == nil {
		return
	}
	s.apiMu.RLock()
	r := s.proxyReg
	s.apiMu.RUnlock()
	if r != nil {
		r(name, def)
	}
}
