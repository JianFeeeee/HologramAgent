package webui

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ===== 登录入口的滥用防护 =====
//
// 为什么需要：webui 门户可以经 frp 之类的穿透暴露到公网
// （本项目实测 https://homeagent.jianfgit.xyz/ 直达），而 handleLogin
// 在此之前是**零防护**：无速率限制、无失败计数、口令用 == 明文比对、
// 失败无审计。这等于把一个可爆破的口子直接开到外网。
//
// 设计取舍（都不是「越多越好」）：
//
//   - **按来源 IP 计数**，不全局：全局计数会让攻击者用一个 IP 的狂刷
//     把所有人（含管理员自己）一起锁死 —— 那既是 DoS，又让「锁着」
//     变成一种攻击手段。按来源隔离后，攻击者只能锁自己。
//   - **退避而非永久封禁**：窗口过期自动恢复。永久封禁意味着一旦误撞
//     （或被撞库）就再也登不进，只能上机器改配置。
//   - **成功即清零**：否则「手滑输错三次」会被永久记账。惩罚应只针对
//     持续失败的行为。
//   - **不做账号级锁定**：本系统 webui 只有单一管理员账号，账号级锁定
//     相比 IP 级没有额外收益，却多一个误伤面。
//
// 刻意不做：全局失败告警、验证码、账号锁定 —— 要么超出本系统规模所需，
// 要么会引入新的误伤面。留待真需要时再说。

// loginLimiter 是按来源的失败计数器。
//
// 并发：登录是公网入口，必须扛得住并发爆破，计数用互斥量保护。
// 读路径（Allow）也要加锁 —— 无锁读在多核下会漏计，反而让限流可被
// 多核并发绕过。
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*loginFailRecord
	// maxFails 是触发限流的失败次数上限。
	maxFails int
	// window 是失败计数的存活窗口。
	window time.Duration
	// now 可注入，便于测试窗口过期而不必 sleep。
	now func() time.Time
	// offsetInTest 仅测试用：虚拟时钟的累计偏移（生产恒为 0）。
	offsetInTest time.Duration
}

type loginFailRecord struct {
	count int
	first time.Time // 本轮计数的起点（**不是**最后一次失败的时间）
}

func newLoginLimiter(maxFails int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		fails:    map[string]*loginFailRecord{},
		maxFails: maxFails,
		window:   window,
		now:      time.Now,
	}
}

// Allow 报告该来源此刻是否允许再试一次登录。
// 被限流时同时返回建议的重试等待时长。
func (l *loginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[key]
	if !ok {
		return true, 0
	}
	now := l.now()
	if now.Sub(rec.first) >= l.window {
		// 窗口已过：这条记录不再代表「近期持续失败」，直接丢弃。
		delete(l.fails, key)
		return true, 0
	}
	if rec.count < l.maxFails {
		return true, 0
	}
	// 还能再等多久 —— 用于 Retry-After。下限 1 秒：算出 0 会让客户端
	// 立即重试，那等于没有限流。
	retry := l.window - now.Sub(rec.first)
	if retry < time.Second {
		retry = time.Second
	}
	return false, retry
}

// Fail 记一次失败。**成功登录不调用它**，由 Reset 清零。
func (l *loginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	rec, ok := l.fails[key]
	if !ok || now.Sub(rec.first) >= l.window {
		// 新一轮失败：计数从 1 重新开始，窗口也重新起算。
		l.fails[key] = &loginFailRecord{count: 1, first: now}
		return
	}
	rec.count++
}

// Reset 登录成功后清零该来源的计数。
func (l *loginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}

// prune 丢弃过期记录。
//
// 为什么需要：不调用它就没有别的触发点，map 会随来源无限增长 ——
// 攻击者轮换大量来源 IP 就能把它喂成内存泄漏。
func (l *loginLimiter) prune() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, rec := range l.fails {
		if now.Sub(rec.first) >= l.window {
			delete(l.fails, k)
		}
	}
}

// liveKeys 返回当前记录数（仅测试用；生产路径不读它，避免为测试留后门）。
func (l *loginLimiter) liveKeys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}

// clockAdvance 仅测试用：推进虚拟时钟，使窗口可在不 sleep 的情况下过期。
func (l *loginLimiter) clockAdvance(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.offsetInTest += d
	l.now = func() time.Time { return time.Now().Add(l.offsetInTest) }
}

// sourceKey 取请求的来源标识，用于按来源隔离限流计数。
//
// ★ 这里有一个我在生产上亲手踩过的坑，务必先读：
//
// webui 经 frp/nginx 穿透到公网时，**所有外部请求的 RemoteAddr 都是
// 127.0.0.1**（日志实证 from=127.0.0.1）。若只用 RemoteAddr 计数，
// 所有人共用一个桶 —— 任何人爆破 5 次就把**所有人（含管理员）**一起
// 锁死 10 分钟。限流于是从防护变成了 DoS。我最初就是这么写的，
// 并且在用 8 次错误登录做「验证」时真的把管理员锁在了外面。
//
// 反过来，无条件采信 X-Forwarded-For 也不行：该头由客户端可伪造，
// 攻击者换一个头就能绕过限流，甚至把限流当成打别人来源的武器。
//
// 正确做法是中间路线：**只信任受信反代发来的 XFF**。判定「是否来自
// 受信反代」不能靠内网/回环 IP 猜 —— 穿透部署下反代恰恰就在本机
// 127.0.0.1，跟直连请求完全同源。所以必须由部署方**显式声明**受信
// 反代网段（webui.trusted_proxies 设置），未声明则一律不采信 XFF。
//
// 权衡写清楚：未声明受信反代时，穿透场景下限流会退化成「全局」。
// 这不是 bug 而是**刻意的保守默认** —— 宁可限流偏保守（并发场景下
// 容易误伤），也不要因为采信伪造头而形同虚设。部署方按需开启。
func (h *Handler) sourceKey(r *http.Request) string {
	if h.trustedProxies != nil {
		if remote := hostOnly(r.RemoteAddr); h.isTrustedProxy(remote) {
			if xff := firstForwardedIP(r.Header.Get("X-Forwarded-For")); xff != "" {
				return xff
			}
			if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
				return xr
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// hostOnly 去掉端口。
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// isTrustedProxy 判断来源地址是否落在受信反代网段内。
func (h *Handler) isTrustedProxy(ip string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, cidr := range h.trustedProxies {
		if _, ipnet, err := net.ParseCIDR(cidr); err == nil && ipnet.Contains(parsed) {
			return true
		}
		// 也接受裸 IP 写法（配置更省事）
		if single := net.ParseIP(cidr); single != nil && single.Equal(parsed) {
			return true
		}
	}
	return false
}

// firstForwardedIP 取 X-Forwarded-For 里最接近客户端的地址。
//
// 语义：XFF 是逐跳追加的列表，**最左边**是原始客户端。右边那些是中间
// 代理自报的，可被伪造。取第一个即可 —— 它由受信反代写入（我们只在
// 受信来源才走到这里）。
func firstForwardedIP(xff string) string {
	if xff == "" {
		return ""
	}
	if i := strings.IndexByte(xff, ','); i >= 0 {
		return strings.TrimSpace(xff[:i])
	}
	return strings.TrimSpace(xff)
}

// 限流参数。取「够宽容又不至于被爆破」的值：
//
//	5 次失败后开始拦 —— 记错口令、输错用户名都可能连错几次；持续的
//	  失败才可疑。
//	10 分钟窗口 —— 10 分钟内 5 次错基本可断定是爆破；窗口过长会让一次
//	  撞库把来源锁很久。
//	1MB 请求体上限 —— 真实登录请求只有几十字节，1MB 已有极大余量；
//	  不限制则单个请求就能吃光内存。
const (
	loginMaxFails  = 5
	loginWindow    = 10 * time.Minute
	loginBodyLimit = 1 << 20
)

// enforceLoginRateLimit 是 handleLogin 的限流闸门。
// 放行返回 true；已超限则已写好 429 并返回 false。
func (h *Handler) enforceLoginRateLimit(w http.ResponseWriter, r *http.Request) bool {
	if h.loginLimiter == nil {
		return true
	}
	key := h.sourceKey(r)
	if ok, retry := h.loginLimiter.Allow(key); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{
			"error": "登录尝试过于频繁，请稍后再试",
		})
		return false
	}
	return true
}

// bodyOverLimit 报告请求体是否已超过登录体上限。
//
// 存在理由（重要）：MaxBytesReader 只在**读超限**时通过 MaxBytesError 报错，
// 而 json.Decoder 遇到非法 JSON 会在第 0 字节就返回语法错误，根本不往下读。
// 于是「超大 + 非法」这种最省力的攻击载荷只会被当成 400，而数据已进缓冲。
// 这里显式检查 ContentLength 补上这个缺口。
func bodyOverLimit(r *http.Request) bool {
	return r.ContentLength > loginBodyLimit
}

// parseTrustedProxies 解析受信反代网段设置（逗号分隔的 CIDR 或裸 IP）。
//
// 为什么**不去掉非法项而是整份拒绝**：一份含拼写错误的受信列表会静默
// 退化成「不采信 XFF」—— 而表现是「限流把所有人锁了」，运维很难联想到
// 是这里写错了。宁可启动时报错。
func parseTrustedProxies(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			if _, _, err := net.ParseCIDR(part); err != nil {
				log.Printf("[webui] trusted_proxies: invalid CIDR %q ignored (%v)", part, err)
				continue
			}
		} else if net.ParseIP(part) == nil {
			log.Printf("[webui] trusted_proxies: invalid IP %q ignored", part)
			continue
		}
		out = append(out, part)
	}
	return out
}
