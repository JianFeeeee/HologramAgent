package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzip 中间件：给可压缩的响应加 Content-Encoding: gzip。
//
// ★ 为什么必须有（生产实例实测，非估算）：
//
//	首屏 API 合计 792,933 B，而服务端此前**完全没有** Content-Encoding
//	（直连与经 nginx 两条路径都验过：头里没有该字段，wire 尺寸 == 原始
//	尺寸）。实测同一份数据 gzip -9 后：
//
//	  /api/v1/chat/history?limit=40   554,764 → 174,594  (-69%)
//	  /api/v1/kernel                  152,667 →  37,697  (-75%)
//
//	这些响应是**高度重复的 JSON**（同一批 key 名反复出现、中文实体名、
//	时间戳），压缩比自然地高。经公网入口（frp + 移动网络）时，793KB 的
//	首屏与 53MB/h 的空闲轮询都是实打实的流量钱。
//
// 放在哪一层：
//
//	包在 logged **外面**（链：proxyDispatch → gzip → logged → mux）。
//	理由：proxyDispatch 命中时直接 return，响应来自上游（上游自己的
//	Content-Encoding 由 httputil 处理），我们不该插手；而门户自身的
//	全部响应（含 requireAPI 的 401/503、requireWeb 的 302 跳转、
//	HTML/CSS/JS、全部 JSON API）都该压。
//
// ★ 三个必须显式处理的坑：
//
//  1. **SSE / 流式不能压。** text/event-stream 一旦进了 gzip 缓冲，
//     flush 语义就废了（表现为「前端收不到流式，要等缓冲攒够」）。
//  2. **必须透传 http.Flusher。** handler 里有 `w.(http.Flusher)`
//     的类型断言（handleChatEvents / streamOpenAI）。包装 ResponseWriter
//     会让断言失败 ⇒ flusher 为 nil ⇒ 代码走降级分支，SSE 直接坏掉。
//     这不是「顺手加一下」能过的改动。
//  3. **HEAD / 204 / 304 没有 body**，压缩它们只会浪费 CPU 和加坏头。
const gzipMinLength = 1024 // 与 nginx 的 gzip_min_length 对齐

// gzipCompressibleContentType 判定是否值得压。
//
// 压「已经压缩过」的类型是纯浪费：webp/png/jpeg/gzip/zip 再压一遍
// 几乎不缩小，却要付 CPU + 掉帧。webui 自带 mascot.webp（约 25KB）就是这类
// ——注意体积大小与本判定无关：值得压的是「文本」，不是「大文件」。
//
// ★ text/event-stream 明确**不**列（虽然它在通用规则里可压）：
// 压它会毁掉 flush 语义。宁可漏压也不要压坏流。
func gzipCompressibleContentType(ct string) bool {
	if ct == "" {
		return false
	}
	// 取分号前的主类型（content-type 可能带 charset）
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.TrimSpace(strings.ToLower(ct))
	switch ct {
	case "application/json", "application/javascript", "text/javascript",
		"text/html", "text/css", "text/plain",
		"application/xml", "text/xml", "image/svg+xml":
		return true
	}
	return false
}

// acceptsGzip 判断客户端是否要 gzip。
func acceptsGzip(r *http.Request) bool {
	for _, v := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if i := strings.IndexByte(v, ';'); i >= 0 {
			v = v[:i]
		}
		if strings.EqualFold(strings.TrimSpace(v), "gzip") {
			return true
		}
	}
	return false
}

// gzipWriter 池：gzip.NewWriter 每次都要分配窗口/哈希状态，
// 而 webui 的 API 响应极频繁，不复用会让 GC 压力反噬我们要省的目的。
var gzipPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzip 响应的三个状态。写成枚举而不是几个 bool —— 上一版用
// passthrough/decided/buffering/allowBuf 四个 bool 交叉表示，
// 出现了「小响应内容被写成空」和「已压缩类型仍被压」两个 bug，
// 根因就是「到底该不该压」在 Write / WriteHeader / 收尾三处各判一次、
// 判据还不一致。**单一判据在单一处求值**是这里的硬要求。
type gzipMode int

const (
	// gzipUndecided：还没看过 Content-Type，不知道该不该压。
	gzipUndecided gzipMode = iota
	// gzipPassThrough：不该压（或不能压），原样透传。
	gzipPassThrough
	// gzipBuffering：可压且已决定压，但还没写够阈值，先攒着。
	gzipBuffering
	// gzipStreaming：正在边收边压（已越过阈值）。
	gzipStreaming
)

// gzipResponseWriter 包装 ResponseWriter，边写边压。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	mode        gzipMode
	wroteHeader bool
	status      int
	buf         []byte
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.status = code
	// 无 body 的状态码不压，也不加 Content-Encoding。
	if code == http.StatusNoContent || code == http.StatusNotModified {
		g.commit(gzipPassThrough)
		return
	}
	// 内容类型不可压（如 image/webp）：透传，头照常发。
	if !gzipCompressibleContentType(g.Header().Get("Content-Type")) {
		g.commit(gzipPassThrough)
		return
	}
	// 可压，但**先不发头**：Content-Length 一旦发出就不能改，
	// 得先知道最终写多少字节才能决定压不压（小响应压了反而变大）。
	// 真正的 commit 发生在首次 Write 越过阈值、或 handler 返回时。
}

func (g *gzipResponseWriter) Write(p []byte) (int, error) {
	switch g.mode {
	case gzipPassThrough:
		g.commit(gzipPassThrough)
		return g.ResponseWriter.Write(p)

	case gzipStreaming:
		// 已开压：直接喂进 gzip 流。注意此时若下游还没 WriteHeader 过，
		// startCompress 已经替我们发过了（见 commit）。
		if g.gz == nil {
			return g.ResponseWriter.Write(p)
		}
		return g.gz.Write(p)

	case gzipBuffering:
		g.buf = append(g.buf, p...)
		if len(g.buf) >= gzipMinLength {
			g.startCompress()
			g.writeBufToStream()
		}
		return len(p), nil

	default: // gzipUndecided
		// WriteHeader 没被显式调用（handler 直接 Write）也走这里。
		if !gzipCompressibleContentType(g.Header().Get("Content-Type")) {
			g.commit(gzipPassThrough)
			return g.ResponseWriter.Write(p)
		}
		g.buf = append(g.buf, p...)
		if len(g.buf) >= gzipMinLength {
			g.startCompress()
			g.writeBufToStream()
		} else {
			g.mode = gzipBuffering
		}
		return len(p), nil
	}
}

// writeBufToStream 把缓冲内容送进 gzip 流。写失败（客户端已断开）在
// 响应收尾阶段无法处置，与 close/Flush 中的处理一致地忽略。
func (g *gzipResponseWriter) writeBufToStream() {
	if g.gz != nil && len(g.buf) > 0 {
		_, _ = g.gz.Write(g.buf)
	}
	g.buf = nil
}

// startCompress 真正开始压缩：剥掉 Content-Length、补 Content-Encoding
// 与 Vary，然后才发头。
func (g *gzipResponseWriter) startCompress() {
	h := g.Header()
	h.Del("Content-Length") // 压缩后长度未知，留着就是错的
	h.Set("Content-Encoding", "gzip")
	// Vary：同一 URL 会因 Accept-Encoding 不同而返回不同编码。中间缓存
	// （nginx/CDN/浏览器）必须据此区分，否则会把 gzip 版发给不支持
	// 压缩的客户端。
	h.Add("Vary", "Accept-Encoding")
	if g.gz == nil {
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.commit(gzipStreaming)
}

// commit 定模式并发头（幂等）。
func (g *gzipResponseWriter) commit(mode gzipMode) {
	if g.wroteHeader {
		g.mode = mode
		return
	}
	g.mode = mode
	g.wroteHeader = true
	if g.status == 0 {
		g.status = http.StatusOK
	}
	g.ResponseWriter.WriteHeader(g.status)
}

// Flush 透传：SSE 依赖它逐帧下发。
//
// ★ 存在即必须正确：handler 里是 `w.(http.Flusher)` 断言，
// 拿不到就等于没有 Flush，SSE 会卡到缓冲满。
//
// 若此刻仍在缓冲（可压但没写够阈值），必须先把已攒的内容发出去，
// 否则 Flush 形同虚设、且数据永远滞留缓冲。
func (g *gzipResponseWriter) Flush() {
	switch g.mode {
	case gzipUndecided:
		// 没写过任何东西就 Flush（少见）：先把头发出去。
		g.commit(gzipPassThrough)
	case gzipBuffering:
		// 有内容但没到阈值：SSE 场景不该走到这；真走到了就直接定夺——
		// 有内容就压（已经攒了半天，收益远大于 23 字节的头开销）。
		if len(g.buf) > 0 {
			g.startCompress()
			g.writeBufToStream()
		} else {
			g.commit(gzipPassThrough)
		}
	case gzipStreaming:
		if g.gz != nil {
			_ = g.gz.Flush()
		}
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish 在 handler 返回后收尾：把「攒着没发」的内容定夺掉。
func (g *gzipResponseWriter) finish() {
	switch g.mode {
	case gzipBuffering:
		if len(g.buf) >= gzipMinLength {
			// 攒够阈值：压。
			g.startCompress()
			g.writeBufToStream()
		} else {
			// 小于阈值（典型：/api/v1/status 197B）：**原样发出**。
			// 这一支是「小响应不压」判据的落地点 —— 压它反而更大。
			g.commit(gzipPassThrough)
			if len(g.buf) > 0 {
				_, _ = g.ResponseWriter.Write(g.buf)
			}
			g.buf = nil
		}
	case gzipUndecided:
		// handler 没写过 body（如只 WriteHeader）但我们压住了头：
		// 按「无 body」处理，原样发头。
		g.commit(gzipPassThrough)
	}
}

// close 关闭 gzip 流并归还池。
func (g *gzipResponseWriter) close() {
	if g.gz != nil {
		_ = g.gz.Close()
		g.gz.Reset(io.Discard)
		gzipPool.Put(g.gz)
		g.gz = nil
	}
}

// gzipMW 是压缩中间件。
func gzipMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HEAD 没有 body；不协商编码。
		if r.Method == http.MethodHead || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		// SSE 直接透传：压缩会毁掉 flush 语义（见文件头注释）。
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			gw.finish()
			gw.close()
		}()
		next.ServeHTTP(gw, r)
	})
}
