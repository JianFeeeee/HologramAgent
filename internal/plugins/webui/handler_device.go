package webui

import (
	"bufio"
	"io"
	"log"
	"net"
	"strings"
	"time"

	"net/http"
)

// 设备网关反代：/api/v1/device/* → remotedevice（HTTP + WS 升级透传）。

// ======== Remote Device Gateway (proxied to remotedevice, opt-in) ========

// deviceGatewayEnabled / deviceGatewayAddr 由 webui 插件启动时从设置读取并注入。
// 默认禁用：用户显式配置 device_gateway_enabled=true 后，/api/v1/device/* 才会反代到
// remotedevice 插件（self-contained），避免与 remotedevice 耦合。
var (
	deviceGatewayEnabled bool
	deviceGatewayAddr    string
	deviceGatewayToken   string
)

// handleDeviceGatewayProxy 将 /api/v1/device/* 反代到 remotedevice 内部 HTTP 服务。
// 鉴权：本端走 requireAPI（webui API key），转发时带 remotedevice 的 token（X-API-Key）。
// WS 升级请求（Upgrade: websocket）走 hijack 双向字节透传（标准库 http.Client 不支持 101 升级）。
func (h *Handler) handleDeviceGatewayProxy(w http.ResponseWriter, r *http.Request) {
	if !deviceGatewayEnabled {
		http.NotFound(w, r)
		return
	}
	// 客户端鉴权模式：服务端不再提供授权接口（授权由设备端本地控制）。
	// 拒绝旧的 /device/auth 调用，避免误导。
	if strings.HasSuffix(r.URL.Path, "/device/auth") {
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "device authorization moved to client-side; the server no longer stores authorization state",
		})
		return
	}
	addr := deviceGatewayAddr
	if addr == "" {
		addr = "127.0.0.1:9890"
	}
	path := r.URL.Path // 保留 /api/v1/device/... 全路径
	url := "http://" + addr + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}

	// WebSocket 升级：hijack 双向透传（支持 WS over 远程 homed）
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		h.proxyWebSocket(w, r, addr, path)
		return
	}

	h.reverseToUpstream(w, r, url, deviceGatewayToken)
}

// upstreamClient 是旧反代路径共用的 HTTP 客户端。
//
// ★ 为什么必须**自定义**而不是用 http.DefaultClient（两者都有实际故障）：
//
//  1. **禁止跟随重定向**。默认客户端最多跟 10 跳，于是上游回 302 时：
//     反代跟过去 → 目标可能是内网另一个服务或不可达 → 最终把
//     「上游的 302」变成「本层的 502」，并且错误里带着内网 URL。
//     外部用户看到 Bad Gateway + 他访问不了的内网地址：既无用又泄露拓扑。
//     反代**不应有重定向策略** —— 那是客户端的事，原样透传才对。
//
//  2. **超时必须存在**。默认客户端不设超时，上游卡住会拖住本层 goroutine
//     直到客户端放弃；并发下会把连接与内存占满。
//
//  3. **不用 Transport 的自动解压**（默认行为）：反代应当字节级透传，
//     由客户端自己决定是否解压。默认 Transport 会解开 gzip 并丢掉
//     Content-Encoding，导致「上游说 gzip、客户端收到明文」的不一致。
var upstreamClient = &http.Client{
	Timeout: 60 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse // 原样返回 3xx，不跟随
	},
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // 字节级透传，见上
	},
}

// hopByHopHeaders 是逐跳头，按 RFC 7230 §6.1 **不得**由代理转发。
//
// 照抄上游这些头会出真问题：Content-Length 与 Transfer-Encoding 描述的是
// **上游那条连接**的分帧方式，本层到客户端是另一条连接，直接抄会导致
// 分帧错乱（客户端按错误的长度读）；Keep-Alive/Connection 同理。
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// reverseToUpstream 是两条旧反代路径（设备网关 / pluginmgr）共用的转发实现。
//
// 抽出来是因为原来两处各抄了一份，于是同一个 bug 修了两遍还漏了两处
// （跟随 3xx、不 flush、不透传 X-Forwarded）。共用一份就不会再分叉。
//
// authToken 非空时以 X-API-Key 注入（上游是受令牌保护的内网服务）。
func (h *Handler) reverseToUpstream(w http.ResponseWriter, r *http.Request, url, authToken string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "反代请求构造失败"})
		return
	}
	req.Header = r.Header.Clone()
	if authToken != "" {
		req.Header.Set("X-API-Key", authToken)
	}
	h.setForwardedHeaders(req, r)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		// ★ 错误细节**不得**回给客户端：原始 err 里含上游地址与栈
		// （实测泄露 "http://127.0.0.1:9890/...: connect: connection refused"）。
		// 那对用户无用（内网地址他访问不了），却把内部拓扑说给了外人。
		// 完整原因写日志，对外只给一句话。
		log.Printf("[webui] reverse-proxy upstream unreachable url=%s err=%v", url, err)
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error": "上游服务不可达",
		})
		return
	}
	defer resp.Body.Close()

	// 复制响应头（剔除逐跳头）
	for k, vs := range resp.Header {
		if isHopByHop(k) {
			continue
		}
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)

	// ★ 逐帧 flush：这是「流式透传」的关键。
	//
	// 原实现用 io.Copy(w, resp.Body)：ResponseWriter 自带缓冲，
	// 上游按帧下发的 SSE/长轮询内容会全部堆到上游响应结束才吐出。
	// 实测（裸 TCP 观测）上游每 80ms 一帧共 3 帧，缓冲版本只产生
	// **1 次**读（集中在 161ms），客户端表现为「卡住不动然后一次性全出来」。
	//
	// 为什么不用 httputil.ReverseProxy：本函数要保留「注入 X-API-Key」
	// 这类旧行为，改写量比重写还大；且新反代 proxy.go 已用 ReverseProxy，
	// 这里保持手写但补上 flush 即可。
	flushCopy(w, resp.Body)
}

// flushCopy 逐块拷贝并在块间显式 Flush。
//
// 用 io.CopyBuffer + 小块（4KB）：块越大，单次 flush 的延迟越高。
// 4KB 是 SSE 与插件 UI 增量响应的合理粒度 —— 更大没有吞吐收益
// （这些响应本来就不是吞吐型的），却会让首帧更晚可见。
func flushCopy(w http.ResponseWriter, src io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// isHopByHop 判定逐跳头（大小写不敏感）。
func isHopByHop(k string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// setForwardedHeaders 注入 X-Forwarded-*，让上游能知道真实来源。
//
// 只在**尚未设置**时补：若请求本身已带这些头（例如外层 nginx 已注入），
// 覆盖会丢掉真正的客户端 IP —— 那正是限流与审计最需要的。
func (h *Handler) setForwardedHeaders(dst *http.Request, src *http.Request) {
	if dst.Header.Get("X-Forwarded-For") == "" {
		if ip, _, err := net.SplitHostPort(src.RemoteAddr); err == nil {
			dst.Header.Set("X-Forwarded-For", ip)
		} else if src.RemoteAddr != "" {
			dst.Header.Set("X-Forwarded-For", src.RemoteAddr)
		}
	}
	if dst.Header.Get("X-Real-IP") == "" {
		dst.Header.Set("X-Real-IP", clientIP(src))
	}
	if dst.Header.Get("X-Forwarded-Host") == "" && src.Host != "" {
		dst.Header.Set("X-Forwarded-Host", src.Host)
	}
	if dst.Header.Get("X-Forwarded-Proto") == "" {
		proto := "http"
		if src.TLS != nil {
			proto = "https"
		}
		dst.Header.Set("X-Forwarded-Proto", proto)
	}
}

// proxyWebSocket 用 TCP 直连 + hijack 将客户端 WS 连接双向透传到设备网关。
func (h *Handler) proxyWebSocket(w http.ResponseWriter, r *http.Request, addr, path string) {
	// 设备网关默认仅监听 127.0.0.1（remotedevice），反代目标即内网 homed 本机或指定 addr。
	// 用 net.Dial 直连网关并手动发起 WS 升级握手（net/http 客户端不支持 ws:// 升级）。
	host, port := addr, "9890"
	if h2, p2, ok := splitHostPort(addr); ok {
		host, port = h2, p2
	}
	target := net.JoinHostPort(host, port)
	upConn, err := net.DialTimeout("tcp", target, 15*time.Second)
	if err != nil {
		http.Error(w, "ws upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// 手动构造 WS 升级请求（保留客户端头 + 注入网关 token）
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		key = "homeagent-proxy-random-key"
	}
	reqPath := path
	if r.URL.RawQuery != "" {
		reqPath += "?" + r.URL.RawQuery
	}
	var b strings.Builder
	b.WriteString("GET " + reqPath + " HTTP/1.1\r\n")
	b.WriteString("Host: " + addr + "\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	if deviceGatewayToken != "" {
		b.WriteString("X-API-Key: " + deviceGatewayToken + "\r\n")
	}
	for k, vv := range r.Header {
		kl := strings.ToLower(k)
		if kl == "upgrade" || kl == "connection" || kl == "sec-websocket-key" || kl == "sec-websocket-version" || kl == "host" || kl == "x-api-key" || kl == "authorization" {
			continue
		}
		for _, v := range vv {
			b.WriteString(k + ": " + v + "\r\n")
		}
	}
	b.WriteString("\r\n")
	if _, err := upConn.Write([]byte(b.String())); err != nil {
		http.Error(w, "ws upstream write: "+err.Error(), http.StatusBadGateway)
		return
	}

	// 读上游 101 响应
	br := bufio.NewReader(upConn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		http.Error(w, "ws upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		http.Error(w, "ws upstream status: "+resp.Status, http.StatusBadGateway)
		return
	}

	// 客户端 hijack：把 101 响应头写给客户端并接管双向连接
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	clientConn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	// 向上游 101 响应头转发给客户端
	if err := resp.Write(brw); err != nil {
		return
	}
	if err := brw.Flush(); err != nil {
		return
	}

	// 双向透传（WS 帧字节不动）：
	// 客户端 -> 上游
	errCh := make(chan struct{}, 2)
	go func() {
		io.Copy(upConn, brw)
		if tc, ok := upConn.(interface{ CloseWrite() error }); ok {
			tc.CloseWrite()
		}
		errCh <- struct{}{}
	}()
	// 上游 -> 客户端
	go func() {
		wb := bufio.NewWriter(clientConn)
		io.Copy(wb, br)
		wb.Flush()
		errCh <- struct{}{}
	}()
	<-errCh
}

// splitHostPort 拆分 addr 为 host/port；无端口时返回 ok=false。
func splitHostPort(addr string) (string, string, bool) {
	if strings.Contains(addr, ":") {
		h, p, err := net.SplitHostPort(addr)
		if err == nil {
			return h, p, true
		}
	}
	return addr, "", false
}
