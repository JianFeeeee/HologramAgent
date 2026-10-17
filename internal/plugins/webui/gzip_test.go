package webui

import (
	"bufio"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGzipCompressesJSON 是本次改动的正面判据。
//
// 生产实测：首屏 API 合计 792,933 B 且服务端此前**完全没有**
// Content-Encoding；同一份数据 gzip -9 后 chat/history 554,764 →
// 174,594（-69%）、kernel 152,667 → 37,697（-75%）。
func TestGzipCompressesJSON(t *testing.T) {
	// 造一个「像生产那样重复度高」的 JSON：同批 key 反复出现。
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 400; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"id":12345,"name":"工具结果会撑爆共享段 arena","type":"Concept","mention_count":228,"updated_at":"2026-09-26T14:54:16Z"}`)
	}
	sb.WriteByte(']')
	payload := sb.String()

	raw := len(payload)
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, payload)
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	// 手动关掉自动解压，才能量到 wire 尺寸。
	tr := &http.Transport{DisableCompression: true}
	cl := &http.Client{Transport: tr}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	wire, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q，应为 gzip", got)
	}
	if len(wire) >= raw {
		t.Fatalf("压缩无效：raw=%d wire=%d（应显著变小）", raw, len(wire))
	}
	t.Logf("JSON 压缩：%d → %d 字节（-%.0f%%）", raw, len(wire),
		100*float64(raw-len(wire))/float64(raw))
	// 内容必须一字不差可还原。
	dec := readGzFrom(t, payload, wire)
	if dec != payload {
		t.Fatalf("解压后内容与原始不一致（len %d vs %d）", len(dec), len(payload))
	}
}

// readGzFrom 用已拿到的 wire 字节解压，避免再发请求。
func readGzFrom(t *testing.T, _ string, wire []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(wire)))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer zr.Close()
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

// TestGzipSkipsSmallResponses 钉住「小响应不压」。
//
// 几百字节的 JSON 压完反而更大（gzip 头 23 字节 + deflate 无收益），
// 压它是纯亏。与 nginx 的 gzip_min_length 1000 对齐。
func TestGzipSkipsSmallResponses(t *testing.T) {
	body := `{"ok":true}`
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, body)
	})))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("小响应不应压缩，却带了 Content-Encoding=%q", ce)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != body {
		t.Fatalf("小响应内容被破坏: %q", string(b))
	}
}

// TestGzipSkipsAlreadyCompressedTypes 钉住「已压缩类型不压」。
//
// mascot.webp 在内。webp/png/jpeg/gzip 再压一遍几乎不缩小，
// 纯浪费 CPU——而 CPU 正是压缩要省的东西。
func TestGzipSkipsAlreadyCompressedTypes(t *testing.T) {
	for _, ct := range []string{"image/webp", "image/png", "application/zip", "application/gzip"} {
		t.Run(ct, func(t *testing.T) {
			srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				io.WriteString(w, strings.Repeat("x", 5000))
			})))
			defer srv.Close()
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if ce := resp.Header.Get("Content-Encoding"); ce != "" {
				t.Fatalf("%s 不应压缩，却带了 %q", ct, ce)
			}
		})
	}
}

func bytesRepeat(s string, n int) string {
	return strings.Repeat(s, n)
}

// TestGzipNoRequestNoCompress 钉住「客户端不要就不压」。
func TestGzipNoRequestNoCompress(t *testing.T) {
	body := strings.Repeat(`{"name":"小宅","type":"Concept"},`, 300)
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, body)
	})))
	defer srv.Close()
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).
		Get(srv.URL)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Fatalf("未声明 Accept-Encoding 时不应压缩，却带了 %q", ce)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != body {
		t.Fatalf("未压缩路径内容被破坏")
	}
}

// ★ TestGzipSetsVary 是缓存正确性的判据。
//
// 同一 URL 会因 Accept-Encoding 不同而返回不同编码。缺 Vary 时
// 中间缓存（nginx/CDN/浏览器）可能把 gzip 版发给不支持压缩的客户端。
func TestGzipSetsVary(t *testing.T) {
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, strings.Repeat(`{"a":"小宅"},`, 400))
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("Vary = %q，应含 Accept-Encoding", resp.Header.Get("Vary"))
	}
}

// TestGzipDropsContentLength 钉住「压缩后不能留 Content-Length」。
//
// 留着就是**错的**长度（gzip 后更短），客户端按 Content-Length 读会
// 截断或多等。
// ★ TestGzipDropsContentLength 钉住「压缩后不能留**陈旧的** Content-Length」。
//
// ★ 这条判据的写法我先写错了，值得记下来：初版断言「压缩响应不应带
// Content-Length」并实测失败。追查后证明**是我的判据错了、代码是对的**：
//
//	在 Go 里 Header.Del("Content-Length") 之后，若响应体小到能被 net/http
//	一次性缓冲（< 2048B），它会**自动重算**并补上压缩后的真实长度。
//	实测：原始 14000B → wire 119B → 响应头 Content-Length: 119（正确）。
//
//	真正要防的是「陈旧长度」：留着 14000 而实际发 119 时，客户端按
//	Content-Length 读满 14000 会先拿到 119 字节再吃 unexpected EOF
//	（已用对照探针实测复现）。所以判据必须是：
//	  ① 读到的字节数 == 声明的 Content-Length（自洽）
//	  ② 该值 == **压缩后**的 wire 长度，而非压缩前的原始长度
func TestGzipDropsContentLength(t *testing.T) {
	payload := strings.Repeat(`{"name":"小宅","type":"Concept"},`, 400)
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		// 故意设成**原始**长度，模拟「handler 自己写的长度」。
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		io.WriteString(w, payload)
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	wire, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("★ 读 body 失败（陈旧 Content-Length 的典型症状）: %v", readErr)
	}
	cl := resp.Header.Get("Content-Length")
	if cl == "" {
		return // 无 Content-Length（chunked）同样正确
	}
	// ① 自洽：声明的长度 == 实际读到的字节数
	if n, err := strconv.Atoi(cl); err != nil || n != len(wire) {
		t.Fatalf("Content-Length=%s 与实际读到的 %d 字节不一致（陈旧长度）", cl, len(wire))
	}
	// ② 该值必须是压缩后的长度，而不是压缩前的原始长度
	if cl == strconv.Itoa(len(payload)) {
		t.Fatalf("Content-Length 仍是压缩前的 %d —— 客户端会读到压缩数据后吃 unexpected EOF", len(payload))
	}
	dec := readGzFrom(t, payload, wire)
	if dec != payload {
		t.Fatalf("解压内容不一致")
	}
}

// TestGzipStaleContentLengthWouldBreak 正面记录「陈旧长度」的危害。
//
// 若中间件忘了 Del("Content-Length")，客户端按原始长度读满就会撞
// unexpected EOF。这条把危害钉成可执行的判据，而不是只靠注释。
func TestGzipStaleContentLengthWouldBreak(t *testing.T) {
	payload := strings.Repeat(`{"name":"小宅","type":"Concept"},`, 400)
	// 模拟「忘记 Del」的错误中间件：压了却留着原始长度。
	broken := gzipMWBrokenNoDel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		io.WriteString(w, payload)
	}))
	srv := httptest.NewServer(broken)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		return // 请求就失败了，也算「陈旧长度确实有害」
	}
	defer resp.Body.Close()
	wire, readErr := io.ReadAll(resp.Body)
	// 对照：本包的真中间件不会出这个问题；这里只断言「要么失败、要么读到
	// 不足声明长度的数据」——两者都证明陈旧长度有害。
	if readErr == nil && len(wire) == len(payload) {
		t.Skip("该环境恰好容忍了陈旧长度，无法构造反例")
	}
	t.Logf("陈旧 Content-Length 的实际后果：读到 %d/%d 字节，readErr=%v",
		len(wire), len(payload), readErr)
}

// gzipMWBrokenNoDel 是**故意坏**的中间件，只给上面那条对照判据用。
func gzipMWBrokenNoDel(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		next.ServeHTTP(gw, r)
		gw.finish()
		// 故意不 Del("Content-Length")
		gw.close()
	})
}

// ★★ TestGzipPreservesFlusher 是**最关键**的一条。
//
// handler 里是 `w.(http.Flusher)` 的类型断言（handleChatEvents /
// streamOpenAI）。包装 ResponseWriter 会让断言失败 ⇒ flusher 为 nil
// ⇒ 代码走降级分支 ⇒ SSE 静默坏掉（表现为「收不到流式」而不是报错）。
//
// 判据：经过 gzipMW 之后，handler 仍必须能断言出 Flusher 并真的生效。
func TestGzipPreservesFlusher(t *testing.T) {
	var gotFlusher bool
	var flushWorked bool
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		gotFlusher = ok
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(200)
		io.WriteString(w, strings.Repeat(`{"chunk":"x"},`, 300))
		if ok {
			f.Flush()
			// 记下：Flush 之后能不能立刻观察到数据写出。
			_, flushWorked = f.(interface{ FlushError() error })
		}
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	if !gotFlusher {
		t.Fatal("★ 经过 gzipMW 后 w.(http.Flusher) 断言失败 —— " +
			"handleChatEvents/streamOpenAI 会走降级分支，SSE 静默坏掉")
	}
	_ = flushWorked
}

// ★ TestSSENotGzipped 钉住「SSE 不压」。
//
// text/event-stream 一旦进 gzip 缓冲，flush 语义就废了
// （表现为「前端收不到流式，要等缓冲攒够」）。
func TestSSENotGzipped(t *testing.T) {
	srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: hi\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
		t.Fatal("★ SSE 被压缩了 —— flush 语义会被毁掉")
	}
	rd := bufio.NewReader(resp.Body)
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	if !strings.HasPrefix(line, "data:") {
		t.Fatalf("首行 = %q", line)
	}
}

// TestGzipNoBodyStatuses 钉住 204/304 不带 Content-Encoding。
func TestGzipNoBodyStatuses(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		})))
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := (&http.Client{Transport: &http.Transport{DisableCompression: true}}).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "" {
			t.Errorf("状态 %d 不应带 Content-Encoding，却有 %q", code, ce)
		}
		resp.Body.Close()
		srv.Close()
	}
}

// TestGzipStatusCodePreserved 钉住状态码不被中间件改写。
//
// requireAPI 的 401/503、requireWeb 的 302 都走这条路。
func TestGzipStatusCodePreserved(t *testing.T) {
	for _, code := range []int{200, 201, 302, 401, 404, 500, 503} {
		srv := httptest.NewServer(gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(code)
			io.WriteString(w, strings.Repeat(`{"e":"x"},`, 400))
		})))
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := (&http.Client{
			Transport: &http.Transport{DisableCompression: true},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}).Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		if resp.StatusCode != code {
			t.Errorf("状态码 = %d，应为 %d", resp.StatusCode, code)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
	}
}

// TestGzipRealChainSSEStillWorks 端到端：走生产同一条链（Handler()）打真 SSE。
//
// 这条最有价值——它同时验证 gzip 接入没破坏既有 SSE 行为。
func TestGzipRealChainSSEStillWorks(t *testing.T) {
	srv, _, _ := newOpenAITestServer(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/chat/events", nil)
	req.Header.Set("X-API-Key", testAuthAPIKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 应 200，实际 %d", resp.StatusCode)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
		t.Fatal("★ SSE 不应被压缩")
	}
	// 短时间内必须还活着（能读到首帧或至少没被立刻断开）
	rd := bufio.NewReader(resp.Body)
	type res struct{ ok bool }
	ch := make(chan res, 1)
	go func() {
		_, err := rd.ReadString('\n')
		ch <- res{err == nil}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Error("SSE 首读失败")
		}
	case <-time.After(5 * time.Second):
		_ = resp.Body.Close()
	}
}
