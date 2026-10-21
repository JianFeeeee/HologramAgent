package webui

import (
	"bufio"
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ===== 登录入口的滥用防护 =====
//
// 真实风险：webui 门户经 frp 穿透到公网（https://homeagent.jianfgit.xyz/
// 实测直达），而 handleLogin 在加固前**零防护**：无速率限制、无失败计数、
// 口令用 == 明文比对、失败不审计。配合历史上出现过的弱口令习惯
// （日志里能看到「为截图登 WebUI 临时改密码」这类操作），等于把一个
// 可爆破的口子直接开到外网。
//
// 这组判据钉住四件事：有限流、限流不误伤、可退避、比对不泄漏信息。

const testAuthPassword = "correct-horse-battery"

func newAuthTestHandler(t *testing.T) *Handler {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	cfgReg.PluginConfig("webui").Set("password", testAuthPassword)
	h := NewHandler(testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)}))
	// 走生产真实入口 Handler() = proxyDispatch(logged(mux))。
	// 直接用 h.mux 会绕过 logged 中间件，测不到限流的实际生效位置。
	h.RegisterRoutes(http.NewServeMux())
	return h
}

func postLoginFrom(t *testing.T, h *Handler, ip, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.RemoteAddr = ip + ":54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

func tryLoginFrom(h *Handler, ip, user, pass string) int {
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.RemoteAddr = ip + ":54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func tryLogin(h *Handler, user, pass string) int {
	return tryLoginFrom(h, "203.0.113.1", user, pass)
}

func postLoginRaw(t *testing.T, h *Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", bytes.NewReader(body))
	req.RemoteAddr = "203.0.113.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec
}

// 连续失败必须被拦：放行到 30 次都不该一次 429 都没有。
func TestLoginRateLimitsAfterRepeatedFailures(t *testing.T) {
	h := newAuthTestHandler(t)

	ok, blocked := 0, 0
	for i := 0; i < 30; i++ {
		switch code := tryLogin(h, "admin", "wrong-password"); code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			blocked++
		}
	}
	if ok > 0 {
		t.Errorf("错误口令居然登录成功了 %d 次", ok)
	}
	if blocked == 0 {
		t.Error("连续 30 次错误口令从未触发限流（一次 429 都没有）—— 门户可被暴力破解")
	}
}

// 限流必须按来源区分：否则一个 IP 的狂刷就能把所有人（含管理员）一起锁死，
// 限流本身就变成了 DoS 手段。
func TestLoginRateLimitIsPerSource(t *testing.T) {
	h := newAuthTestHandler(t)

	for i := 0; i < 30; i++ {
		tryLoginFrom(h, "203.0.113.9", "admin", "bad")
	}
	if code := tryLoginFrom(h, "203.0.113.9", "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("攻击者来源应被限流，实际 %d", code)
	}
	if code := tryLoginFrom(h, "198.51.100.7", "admin", testAuthPassword); code != http.StatusOK {
		t.Errorf("其他来源的正常登录被误伤（跨来源污染），实际 %d", code)
	}
}

// 成功必须清零：不能因为早先手滑输错几次就再也登不进。
//
// 判据强度说明：不能只「跑 30 次看有没有限流」—— 阈值只有 5，
// 跑 30 次时**无论有没有 Reset 都会限流**，那样的判据是假的
// （已实测：去掉 Reset 后本条依然绿）。必须**测出实际阈值**：
// 清零后应当重新拿到完整的窗口额度。
func TestLoginSuccessResetsCounter(t *testing.T) {
	h := newAuthTestHandler(t)

	// 先用 3 次失败「污染」计数（低于阈值，此时仍能登录）
	for i := 0; i < 3; i++ {
		tryLogin(h, "admin", "bad")
	}
	if code := tryLogin(h, "admin", testAuthPassword); code != http.StatusOK {
		t.Fatalf("少量失败后应仍能正常登录，实际 %d", code)
	}

	// 成功之后，额度必须**重新算满**：连错 loginMaxFails 次才该被拦。
	for i := 1; i <= loginMaxFails; i++ {
		if code := tryLogin(h, "admin", "bad"); code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败期望 401，实际 %d —— 成功登录未清零计数（额度被提前扣掉了）",
				i, code)
		}
	}
	if code := tryLogin(h, "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("第 %d 次失败后应被限流，实际 %d", loginMaxFails+1, code)
	}
}

// 429 必须带 Retry-After，否则客户端/脚本无从判断何时该重试。
func TestLoginRateLimitedCarriesRetryAfter(t *testing.T) {
	h := newAuthTestHandler(t)

	for i := 0; i < 30; i++ {
		tryLogin(h, "admin", "bad")
	}
	rec := postLoginFrom(t, h, "203.0.113.1", "admin", "bad")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("期望 429，实际 %d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 响应缺少 Retry-After 头")
	}
}

// 请求体必须限量：不限流的话一个请求就能把内存吃光。
//
// 判据要点：**不能只看状态码**。8MB 垃圾 JSON 会让解码器直接失败并返回
// 400，与「被限流拒绝」撞码 —— 那样这条判据是假的（改与不改都绿）。
// 所以断言大请求体在解码前就被挡下，即 413。
func TestLoginBodySizeLimited(t *testing.T) {
	h := newAuthTestHandler(t)

	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = 'a'
	}
	rec := postLoginRaw(t, h, body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应返回 413，实际 %d（body=%.120s）—— "+
			"若为 400 说明只是解码失败而非体积限制，判据无效", rec.Code, rec.Body.String())
	}
	if code := tryLogin(h, "admin", testAuthPassword); code != http.StatusOK {
		t.Errorf("正常登录被体积限制误伤，实际 %d", code)
	}
}

// 失败原因不得可区分：不同失败给不同状态码或报文 = 可枚举用户名。
func TestLoginNoUsernameEnumeration(t *testing.T) {
	h := newAuthTestHandler(t)

	badUser := postLoginFrom(t, h, "203.0.113.1", "no-such-user-xyz", "whatever")
	badPass := postLoginFrom(t, h, "198.51.100.7", "admin", "wrong")

	if badUser.Code != badPass.Code {
		t.Errorf("不同失败原因返回不同状态码（%d vs %d），可用于枚举用户名",
			badUser.Code, badPass.Code)
	}
	if badUser.Body.String() != badPass.Body.String() {
		t.Errorf("不同失败原因返回不同响应体，可用于枚举用户名:\n  用户不存在: %s\n  口令错误  : %s",
			badUser.Body.String(), badPass.Body.String())
	}
}

// 限流必须是**有界**的：过期记录要被清掉，否则攻击者轮换来源 IP
// 就能把 map 喂成内存泄漏。
func TestLoginLimiterPrunesExpiredKeys(t *testing.T) {
	l := newLoginLimiter(loginMaxFails, loginWindow)

	for i := 0; i < 500; i++ {
		l.Fail("src-" + strconv.Itoa(i))
	}
	if n := l.liveKeys(); n != 500 {
		t.Errorf("记录数 = %d，期望 500", n)
	}
	// 推进到窗口之后并触发清理
	l.clockAdvance(loginWindow + time.Minute)
	l.prune()
	if n := l.liveKeys(); n != 0 {
		t.Errorf("过期后仍残留 %d 条记录 —— 轮换 IP 即可无限增长（内存泄漏）", n)
	}
}

// 计时器替代方案：验证 Allow 返回的重试时长不为 0。
// 返回 0 会让客户端立即重试 —— 那等于没有限流。
func TestLoginLimiterRetryNeverZero(t *testing.T) {
	l := newLoginLimiter(2, time.Hour)
	l.Fail("k")
	l.Fail("k")
	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("达到阈值后应被限流")
	}
	if retry <= 0 {
		t.Errorf("Retry-After 时长 = %v，必须为正（返回 0 会让客户端立即重试）", retry)
	}
}

// ===== 限流的来源识别：穿透部署下不能把所有人算成一个 =====
//
// ★ 这是我在生产上亲手踩出来的：加了按 IP 限流后，跑 8 次错误登录做验证，
// 结果**把管理员自己锁在外面 10 分钟**。
//
// 原因：webui 经 frp/nginx 穿透到公网，所有外部请求的 RemoteAddr 都是
// 127.0.0.1（日志实证：from=127.0.0.1 status=429）。于是所有人共用一个桶，
// 任何人爆破 5 次，**所有人**（含管理员）一起被锁 —— 限流反而成了 DoS。
//
// 正确做法不是「不信 XFF」（那正是我第一版的做法，会退化成全局限流），
// 而是：**只信任来自受信反代的 X-Forwarded-For**。受信判定不能靠 IP 名单
// 猜（穿透场景下反代就在本机 127.0.0.1），得由配置显式声明。

// 经受信反代时，必须按 XFF 里的真实客户端 IP 计数。
func TestLoginRateLimitUsesForwardedForFromTrustedProxy(t *testing.T) {
	h := newAuthTestHandler(t)
	h.trustedProxies = []string{"127.0.0.1/32", "::1/128"}

	// 攻击者（XFF 声明的来源）狂刷
	for i := 0; i < 30; i++ {
		tryLoginXFF(h, "203.0.113.66", "admin", "bad")
	}
	// 受害者：不同的 XFF 声明 + 正确口令 → 不该被牵连
	if code := tryLoginXFF(h, "198.51.100.23", "admin", testAuthPassword); code != http.StatusOK {
		t.Errorf("受信反代下，不同真实客户端被牵连（限流退化成全局），实际 %d", code)
	}
}

// 不受信来源的 XFF 必须被忽略：否则任何人都能换一个头就绕过限流
// （甚至把限流当成打别人来源的武器）。
func TestLoginRateLimitIgnoresUntrustedForwardedFor(t *testing.T) {
	h := newAuthTestHandler(t)
	// 显式配置为「无受信反代」
	h.trustedProxies = nil

	for i := 0; i < 30; i++ {
		tryLoginXFF(h, "203.0.113.66", "admin", "bad")
	}
	// 换一个 XFF 继续试：来源未被认可，应仍然被限流
	if code := tryLoginXFF(h, "198.51.100.23", "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("换 XFF 头就绕过了限流，实际 %d —— 说明采信了不可信的 XFF", code)
	}
}

// 反代在**同一台机器**上（穿透部署的常态）时，若未配置受信反代，
// 必须仍能识别不同客户端 —— 否则默认配置就把限流变成了全局锁。
// 判据：未配置时退化到「有 XFF 就用第一个非内网地址」？不行 —— 那等于
// 无条件采信。所以这里钉的是另一个行为：**必须显式配置才能生效**，
// 且未配置时的行为要与「无反代」场景一致（全部算同一个来源）。
func TestLoginRateLimitNeedsExplicitTrustedProxyConfig(t *testing.T) {
	h := newAuthTestHandler(t)
	h.trustedProxies = nil // 未配置

	// 未配置 = 不采信 XFF ⇒ 两个不同 XFF 视为同一来源（127.0.0.1）
	for i := 0; i < 6; i++ {
		tryLoginXFF(h, "203.0.113.66", "admin", "bad")
	}
	if code := tryLoginXFF(h, "198.51.100.23", "admin", "bad"); code != http.StatusTooManyRequests {
		t.Errorf("未配置受信反代时，XFF 不应被采信（应视为同一来源），实际 %d", code)
	}
}

// tryLoginXFF 带 X-Forwarded-For 的登录。
func tryLoginXFF(h *Handler, xff, user, pass string) int {
	body := `{"username":"` + user + `","password":"` + pass + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/login", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321" // 穿透场景：反代在本机
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", xff)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// parseTrustedProxies 解析要稳：合法项接受，非法项被丢弃（而不是让整份
// 配置静默失效）。
func TestParseTrustedProxies(t *testing.T) {
	got := parseTrustedProxies(" 127.0.0.1 , 10.0.0.0/8 ,, ::1 ")
	if len(got) != 3 {
		t.Errorf("应解析出 3 项，实际 %d: %v", len(got), got)
	}
	// 非法项被丢弃
	got = parseTrustedProxies("127.0.0.1,999.999.999.999,10.0.0.0/33")
	if len(got) != 1 || got[0] != "127.0.0.1" {
		t.Errorf("非法项未被丢弃，实际 %v", got)
	}
	// 空串 → nil（不采信任何 XFF）
	if parseTrustedProxies("   ") != nil {
		t.Error("空白配置应返回 nil（保守默认：不采信 XFF）")
	}
}

// ===== Server 超时：Slowloris 防护，但不能误杀流式 =====
//
// http.Server 原本**一个超时都没设**（只有 Handler）。后果是 Slowloris：
// 攻击者只占连接不发完整请求头，每个连接挂几 KB，Go 默认不主动断
// （MaxHeaderBytes 限了头部大小，但「慢慢发」不占头部大小），
// 几百个连接就能耗尽 fd。
//
// ★ 但不能图省事直接加 WriteTimeout：webui 有一条**长连接** SSE
// （/api/v1/chat/events）与流式 /v1/chat/completions（可跑 300s）。
// WriteTimeout 是**从请求开始到响应写完**的总预算，会把它们全部腰斩
// （表现为 SSE 每 30s 断一次、前端疯狂重连）。
//
// 判据钉住该设的与不该设的。
func TestServerHasReadSideTimeouts(t *testing.T) {
	p := newServerForTest(t)
	srv := p.server
	if srv == nil {
		t.Fatal("server 未初始化")
	}
	// ReadHeaderTimeout 是 Slowloris 的正解：头在规定时间内没发完就断。
	if srv.ReadHeaderTimeout <= 0 {
		t.Errorf("ReadHeaderTimeout = %v，必须 > 0（无此值时 Slowloris 可挂住连接）",
			srv.ReadHeaderTimeout)
	}
	// IdleTimeout 覆盖 keep-alive 空闲连接（ReadHeaderTimeout 管不到）。
	if srv.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %v，必须 > 0（keep-alive 空闲连接会无限累积）", srv.IdleTimeout)
	}
	// ReadTimeout 限制「读完整请求」的��间（含 body），防慢速上传。
	if srv.ReadTimeout <= 0 {
		t.Errorf("ReadTimeout = %v，必须 > 0（慢速上传会长期占用连接）", srv.ReadTimeout)
	}
}

// ★ 反向判据：WriteTimeout 必须为 0（保持流式不被腰斩）。
// 这是「不该设的超时」，同样要钉住 —— 否则将来有人「顺手补全」就把
// SSE 与流式端点弄坏了，而这类回归在功能测试里很难立刻发现。
func TestServerHasNoWriteTimeout(t *testing.T) {
	p := newServerForTest(t)
	if got := p.server.WriteTimeout; got != 0 {
		t.Errorf("WriteTimeout = %v，应为 0 —— 它会腰斩 SSE（/api/v1/chat/events）"+
			"与流式 /v1/chat/completions（可跑 300s），表现为 SSE 每隔一段时间断一次",
			got)
	}
}

// SSE 端点必须真的能长时间保持连接（判据的正面一侧）。
// 短于 WriteTimeout 的观察窗口即可（不需要真等 30s）。
func TestSSEConnectionSurvivesBeyondReadTimeout(t *testing.T) {
	srv, _, _ := newOpenAITestServer(t)

	// 连上 SSE，观察它至少活过 ReadHeaderTimeout（证明没有被读侧超时误杀）
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/chat/events", nil)
	req.Header.Set("X-API-Key", testAuthAPIKey)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE 连接失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 应 200，实际 %d", resp.StatusCode)
	}
	// 试着读一点：能读到（哪怕是心跳/注释行）说明连接是活的
	rd := bufio.NewReader(resp.Body)
	done := make(chan bool, 1)
	go func() {
		_, err := rd.ReadString('\n')
		done <- err == nil
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Error("SSE 首读即失败（连接被立即关闭）")
		}
	case <-time.After(5 * time.Second):
		// 没数据也算活：SSE 空闲时不发帧是正常的，关键是连接没断。
		_ = resp.Body.Close()
	}
}

// newServerForTest 起一个 webui 插件实例（走真实 Start），用于检查 server 配置。
func newServerForTest(t *testing.T) *Plugin {
	t.Helper()
	cfgReg := internalConfig.NewConfigRegistry("")
	seedWebUIConfig(cfgReg)
	// 绑到空闲端口：绝不能用 :8080，那是生产端口（见 a752ae1 的教训）
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()
	cfgReg.PluginConfig("webui").Set("addr", addr)

	p := &Plugin{name: "webui", mux: http.NewServeMux()}
	s := testSDK(sdk.SDKConfig{Settings: sdk.NewSettings("webui", cfgReg)})
	if err := p.Start(s); err != nil {
		t.Fatalf("启动 webui 失败: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })
	return p
}
