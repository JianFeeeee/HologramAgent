package webui

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ===== 旧反代路径：两处仍用 http.DefaultClient =====
//
// 新反代（proxy.go）早已修掉「跟随上游 3xx」与「不逐帧 flush」，
// 但**这两条旧路径**没跟上，各自复制了一份 http.DefaultClient 的实现：
//
//   proxyToPluginmgr        （handler_settings.go）
//   handleDeviceGatewayProxy（handler_device.go）
//
// 后果是同一个 bug 修了两遍、还漏了两处。判据钉住它们的行为。
//
// ── Bug 1：跟随上游 3xx，把 302 变成 502 并泄露内网 URL ──
//
// http.DefaultClient 默认跟随最多 10 跳重定向。于是：
//
//	上游 302 → Location: http://127.0.0.1:12000/...
//	DefaultClient 跟过去 → 本机另一个服务（或是探不到）
//	最终返回 500/502，且错误信息里带着内网地址
//
// 外部用户看到的是「Bad Gateway」加上一个他完全用不上的内网 URL。
// 既帮不上忙（那是内网地址，他访问不了），又泄露了内部拓扑。

// 上游 302 必须原样透传给客户端，**不得**跟随。
func TestLegacyProxyDoesNotFollowRedirects(t *testing.T) {
	var followed string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirected" {
			followed = "被跟到 /redirected 了"
			w.WriteHeader(http.StatusOK)
			return
		}
		// 指向上游自己的另一个路径：若被跟随，测试就能发现
		w.Header().Set("Location", "/redirected")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()

	h := newLegacyProxyTestHandler(t)
	setDeviceGatewayTarget(t, upstream.Listener.Addr().String())

	rec, _, _ := callDeviceProxy(t, h, "/api/v1/device/x")
	if rec.Code != http.StatusFound {
		t.Errorf("上游 302 应原样透传（302），实际 %d —— 跟随重定向后状态码就变了", rec.Code)
	}
	if followed != "" {
		t.Errorf("反代跟随了上游重定向：%s", followed)
	}
}

// 3xx 不得泄露内网 URL。
//
// 缺陷形态：DefaultClient 跟到内网地址后失败，把 err 写进响应体
// （"device gateway unreachable: Get \"http://127.0.0.1:9890/...\": ..."）。
func TestLegacyProxyDoesNotLeakInternalURL(t *testing.T) {
	// 上游指向一个**不可达**的地址，模拟「跟到内网后连不上」
	h := newLegacyProxyTestHandler(t)
	setDeviceGatewayTarget(t, "127.0.0.1:1") // 必然拒绝连接

	rec, body, _ := callDeviceProxy(t, h, "/api/v1/device/x")
	// 不可达时应 502（这是对的事实），但**不得**在响应里出现内网细节
	if rec.Code != http.StatusBadGateway {
		t.Errorf("上游不可达应 502，实际 %d", rec.Code)
	}
	for _, leak := range []string{"127.0.0.1:1", "connect: connection refused", "dial tcp"} {
		if strings.Contains(body, leak) {
			t.Errorf("错误响应泄露了内网细节 %q: %s", leak, body)
		}
	}
}

// ── Bug 2：不逐帧 flush，流式响应被缓冲到结束 ──
//
// ★ 这条判据的设计过程值得留在代码里（我为此试错了三轮）：
//
//	第一版「首帧早于末帧」→ 假绿。Go 的 net/http 在响应结束后把缓冲一次性
//	吐出，首末帧之间仍有**微秒级**差，任何 `> 0` 都绿。
//
//	第二版改用 Go http.Client 量「首帧延迟 < 上游总时长 70%」→ 仍是假绿。
//	实测直连上游首帧 761ns、总时长 160ms：Go 的 HTTP **客户端**会合并读，
//	第一次 Read 往往把缓冲里已有的字节一次取走，量到的「首帧」其实是
//	客户端第一次拿到数据的时间，与服务端何时 flush 无关。
//
//	第三版（当前）：**用裸 TCP 直连被测服务**。socket 读到几次、分别在
//	什么时刻，是唯一不受客户端读合并干扰的观测。对照组实测（裸 TCP 直连
//	一个逐帧 flush 的上游）：3 次读，时刻为 0.24ms / 80ms / 161ms ——
//	正是上游的分帧节奏。
//
//	于是判据变成：**代理是否把上游的分帧节奏透传出来**（读到多次且
//	跨越上游的帧间隔），而不是「多久收到第一块」。
func TestLegacyProxyFlushesIncrementally(t *testing.T) {
	const (
		frames     = 3
		frameDelay = 80 * time.Millisecond
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := "AAAABBBBCCCC"
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < frames; i++ {
			_, _ = w.Write([]byte(body[i*4 : (i+1)*4]))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(frameDelay)
		}
	}))
	defer upstream.Close()

	h := newLegacyProxyTestHandler(t)
	setDeviceGatewayTarget(t, upstream.Listener.Addr().String())
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// 先取代理的监听地址，用裸 TCP 观测（绕开客户端读合并）
	proxyAddr := srv.Listener.Addr().String()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn,
		"GET /api/v1/device/stream HTTP/1.1\r\nHost: x\r\nX-API-Key: %s\r\nConnection: close\r\n\r\n",
		testAuthAPIKey)

	start := time.Now()
	var reads []time.Duration
	buf := make([]byte, 512)
	// 读到 EOF 为止：测试请求带了 Connection: close，上游写完 3 帧后
	// 服务端会关闭连接，循环自然结束。
	//
	// ★ 第二个坑（记下来）：**不能按「累计 12 字节正文」判结束**。
	// 第一次 read 通常把「响应头 + 首帧正文」一起带来（实测 chunk0 =
	// 头 + "AAAA"），按字节数判会在首读就认为读完，后面两帧的时序全丢 ——
	// 于是判据又变成假的。
	for {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, rerr := conn.Read(buf)
		if n > 0 {
			reads = append(reads, time.Since(start))
		}
		if rerr != nil {
			break
		}
	}

	// 判据：读次数必须 ≥ 2 且跨越一个帧间隔 —— 说明代理没有把响应攒到最后。
	// 阈值取半个帧间隔：Go 的写合并偶尔会把相邻两帧并成一次读，
	// 但绝不可能把三帧跨越 160ms 全并成一次。
	if len(reads) < 2 {
		t.Errorf("只读到 %d 次（%v）—— 反代把流式响应缓冲到结束才一次性吐出。"+
			"上游是每 %v 写一帧共 %d 帧的。", len(reads), reads, frameDelay, frames)
		return
	}
	span := reads[len(reads)-1] - reads[0]
	if span < frameDelay/2 {
		t.Errorf("各次读取集中在 %v 内（%v）—— 没有透传上游的分帧节奏"+
			"（帧间隔 %v），反代仍在缓冲。", span, reads, frameDelay)
	}
}

// 旧反代必须设置 X-Forwarded-*（否则上游无法知道真实来源，
// 且与新反代 proxy.go 的行为不一致 —— 同一个系统两套规则）。
func TestLegacyProxySetsForwardedHeaders(t *testing.T) {
	var gotFwdProto, gotFwdHost, gotXRealIP string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotFwdProto = r.Header.Get("X-Forwarded-Proto")
		gotFwdHost = r.Header.Get("X-Forwarded-Host")
		gotXRealIP = r.Header.Get("X-Real-IP")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	h := newLegacyProxyTestHandler(t)
	setDeviceGatewayTarget(t, upstream.Listener.Addr().String())

	req, _ := http.NewRequest(http.MethodGet, "/api/v1/device/x", nil)
	req.Header.Set("X-API-Key", testAuthAPIKey)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "homeagent.example.com")
	req.Header.Set("X-Real-IP", "198.51.100.9")
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)

	if gotFwdHost == "" {
		t.Error("旧反代未设置 X-Forwarded-Host（上游无法知道真实来源）")
	}
	if gotXRealIP == "" {
		t.Error("旧反代未设置 X-Real-IP")
	}
	_ = gotFwdProto
}

// 上游回的头不得原样把 hop-by-hop 头透给客户端。
// Content-Length/Transfer-Encoding 等由本层连接决定，照抄上游会错乱。
func TestLegacyProxyStripsHopByHopHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Custom", "keep-me")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	h := newLegacyProxyTestHandler(t)
	setDeviceGatewayTarget(t, upstream.Listener.Addr().String())

	rec, _, _ := callDeviceProxy(t, h, "/api/v1/device/x")
	if rec.Header().Get("Keep-Alive") != "" {
		t.Errorf("Keep-Alive 不应透传给客户端（hop-by-hop 头）")
	}
	if rec.Header().Get("X-Custom") != "keep-me" {
		t.Errorf("普通响应头应透传，实际 %q", rec.Header().Get("X-Custom"))
	}
}

// ===== 测试脚手架（走真实 seam，不另造抽象层）=====

// newLegacyProxyTestHandler 造一个 handler，并把设备网关反代打开。
func newLegacyProxyTestHandler(t *testing.T) *Handler {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))
	h.RegisterRoutes(http.NewServeMux())

	prevEnabled, prevAddr, prevToken := deviceGatewayEnabled, deviceGatewayAddr, deviceGatewayToken
	deviceGatewayEnabled = true
	deviceGatewayToken = "gw-token"
	t.Cleanup(func() {
		deviceGatewayEnabled, deviceGatewayAddr, deviceGatewayToken = prevEnabled, prevAddr, prevToken
	})
	return h
}

func setDeviceGatewayTarget(t *testing.T, addr string) {
	t.Helper()
	deviceGatewayAddr = addr
}

// callDeviceProxy 经真实入口发一次请求，返回响应与文本体。
func callDeviceProxy(t *testing.T, h *Handler, path string) (*httptest.ResponseRecorder, string, *http.Response) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-API-Key", testAuthAPIKey)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec, rec.Body.String(), rec.Result()
}
