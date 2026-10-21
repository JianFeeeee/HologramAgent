package pluginmgr

// 插件包大小上限的判据。
//
// 核心命题（本次修复的实质）：
//   ① 上限**可配置**（而非硬编码 100MB）—— 内嵌运行时的包能装进来
//   ② 上限在**读入内存之前**生效 —— 否则限制等于没有
//   ③ 0 = 不限制
//
// ★ ② 是这次真正的修复。原实现在 io.ReadAll 之后才判大小，
// 一个声明超大 Content-Length 的请求会先把内存吃满再收到"包太大"。
// 判据必须能咬住"位置"这个属性，而不只是"超限会被拒绝"。

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// countingReader 记录**实际被读走了多少字节**。
//
// 这是判断"限流在读前生效"的关键工具：若实现先全量读再检查，
// 计数会等于源总长；若在读中限流，计数只到 limit+1 就停。
type countingReader struct {
	r    io.Reader
	read int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}

// ★★★ 判据：超限时**没有把整个 source 读进内存**。
//
// 这条咬的是"防线的位置"，是本次修复的核心性质。
func TestPackageLimitStopsBeforeReadingEverything(t *testing.T) {
	const limit = 1024
	// 源远大于 limit（1MB）
	source := bytes.Repeat([]byte("A"), 1<<20)
	cr := &countingReader{r: bytes.NewReader(source)}

	_, err := readLimited(cr, limit)
	if err == nil {
		t.Fatal("超限应报错")
	}
	// ★ 关键断言：读取量必须远小于源大小
	// （允许多读 1 字节用于边界判定）
	if cr.read > limit+1 {
		t.Fatalf("★ 超限时读了 %d 字节（源 %d，上限 %d）—— 说明是先全量读再检查，防线位置错了",
			cr.read, len(source), limit)
	}
}

// ★★ 判据：恰好等于上限的包**不被误拒**（边界差一）。
func TestPackageLimitBoundaryIsInclusive(t *testing.T) {
	const limit = 512
	exact := bytes.Repeat([]byte("A"), limit)
	if _, err := readLimited(bytes.NewReader(exact), limit); err != nil {
		t.Errorf("恰好等于上限应通过: %v", err)
	}
	over := bytes.Repeat([]byte("A"), limit+1)
	if _, err := readLimited(bytes.NewReader(over), limit); err == nil {
		t.Error("超过上限 1 字节应被拒")
	}
}

// ★★ 判据：0 = 不限制（内嵌运行时可能要更大的包）。
func TestPackageLimitZeroMeansUnlimited(t *testing.T) {
	big := bytes.Repeat([]byte("A"), 4<<20) // 4MB
	got, err := readLimited(bytes.NewReader(big), 0)
	if err != nil {
		t.Fatalf("0 应为不限制: %v", err)
	}
	if len(got) != len(big) {
		t.Errorf("不限时长度应一致: %d vs %d", len(got), len(big))
	}
	// 负数同样视为不限（防御式：配置项误填负数不该变成"拒一切"）
	if _, err := readLimited(bytes.NewReader(big), -1); err != nil {
		t.Errorf("负数应视为不限制: %v", err)
	}
}

// ★★ 判据：默认上限**足够容纳内嵌运行时并留出真实余量**。
//
// 实测数据（2026-10-15，commondev 内嵌 node+pi）：
//   - 包大小 103,795,462 字节 = 99.0 MiB
//   - 旧的硬编码上限 100<<20 = 104,857,600 字节
//     ⇒ 余量仅 1,062,138 字节（1.01 MiB）
//
// ★ 这条判据的措辞经过修正。我最初写的是"内嵌运行时需要 >100MB，
// 所以旧上限会挡住它"——那是**错的**：实测包 99.0 MiB 并未超过
// 100 MiB。我自己写的对照项判据（TestRealEmbeddedRuntimeRejectedByOldLimit）
// 把这一点咬了出来。
//
// 所以真实情况是：旧上限**没挡住当前这个包**，但它只剩 1.01 MiB 余量 ——
// 任何新增一个资源文件就会爆。放宽到 512MB 的意义是把"随时会爆"
// 变成"有合理空间"，而不是"从装不进变成装得进"。
func TestDefaultLimitLeavesRealHeadroom(t *testing.T) {
	const observedPackageBytes = 103795462 // 实测 commondev 包
	limit := int64(defaultMaxPackageMB) << 20
	if limit <= observedPackageBytes {
		t.Fatalf("★ 默认上限 %dMB 装不下实测的内嵌运行时包（%.1f MiB）",
			defaultMaxPackageMB, float64(observedPackageBytes)/(1<<20))
	}
	// 余量要求：至少 2 倍于实测包大小（否则"随时会爆"的问题没解决）
	if limit < observedPackageBytes*2 {
		t.Errorf("★ 余量不足：上限 %dMB 对实测 %.1f MiB 的包余量不够（应至少 2 倍）",
			defaultMaxPackageMB, float64(observedPackageBytes)/(1<<20))
	}
}

// ★★★ 判据：HTTP 上传超限返回 **413**，且错误信息给出可执行提示。
//
// 413 而非 400/500：这是语义正确的状态码，调用方能据此区分
// "包太大"与"包损坏"。
func TestHTTPUploadOverLimitReturns413(t *testing.T) {
	p := &Plugin{maxPackageBytes: 1024} // 故意设小
	body := bytes.Repeat([]byte("A"), 4096)
	req := httptest.NewRequest(http.MethodPost, "/plugins", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	// 直接调用 handler（真实入口，不是 helper）
	req.Body = http.MaxBytesReader(rec, req.Body, 1<<30) // 不干扰本判据
	p.handlePlugins(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("★ 超限应返回 413，实际 %d（body=%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_package_mb") {
		t.Errorf("错误信息应指出如何放宽（max_package_mb）: %s", rec.Body.String())
	}
}

// ★★ 判据：本地文件路径也执行上限（先 Stat 快速拒绝）。
func TestInstallFromPathEnforcesLimit(t *testing.T) {
	dir := t.TempDir()
	pth := filepath.Join(dir, "big.hmap")
	if err := os.WriteFile(pth, bytes.Repeat([]byte("A"), 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plugin{maxPackageBytes: 1024}
	_, err := p.installFromPath(pth, false)
	if err == nil {
		t.Fatal("本地文件超限应报错")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("应为大小错误，实际: %v", err)
	}
}

// ★★ 判据：放宽上限后，**真正约 100MB 的包能装进来** —— 本次的目的。
//
// 用"读入 + 校验"而非完整安装：完整安装要写盘，成本高且与本判据无关。
// 这里钉住的是"读入阶段不再拦它"。
func TestLargePackagePassesWhenLimitRaised(t *testing.T) {
	const size = 101 << 20 // 101MB，旧上限下必被拒
	// 用惰性 reader 避免真的分配 101MB
	lr := io.LimitReader(zeroReader{}, size)
	p := &Plugin{maxPackageBytes: int64(defaultMaxPackageMB) << 20}

	data, err := readLimited(lr, p.maxPackageBytes)
	if err != nil {
		t.Fatalf("★ 放宽后 101MB 包仍被拒: %v", err)
	}
	if int64(len(data)) != size {
		t.Fatalf("大小不对: %d", len(data))
	}
	// 对照：旧的 100MB 硬编码上限会拒它
	if _, err := readLimited(io.LimitReader(zeroReader{}, size), 100<<20); err == nil {
		t.Error("对照项：100MB 上限应拒绝 101MB 包")
	}
}

// zeroReader 无限供应零字节（惰性，不占内存）。
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// ★ 判据：错误类型可判定（供上层区分"太大"与其他失败）。
func TestTooLargeErrorIsIdentifiable(t *testing.T) {
	err := errPackageTooLarge(42 << 20)
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "42MB") {
		t.Errorf("错误应包含上限值: %v", err)
	}
	// 不应是 io.EOF 之类的伪装
	if errors.Is(err, io.EOF) {
		t.Error("不应是 io.EOF")
	}
}

// ★★★ 判据：配置值按**真实形态**（string）被解析。
//
// 这是本仓反复出现的坑：配置从 DB 读出来是字符串
// （config.PluginSettings.Get → openFromStorage 返回 string）。
// 初版照抄 agentcli 的 `v.(float64)` 假设，switch 全不匹配 ⇒
// **配置被静默忽略** —— 用户改了上限却不生效，且无任何提示。
//
// 判据的形态很重要：**必须喂 string**。如果这里喂 int，
// 判据会绿，而生产仍然不生效。
func TestLimitConfigParsedFromString(t *testing.T) {
	cases := []struct {
		in   interface{}
		want int64 // MB
		desc string
	}{
		{"64", 64, "字符串（真实形态）"},
		{"0", 0, "0 = 不限制"},
		{" 128 ", 128, "带空白的字符串"},
		{"-5", 0, "负数视为不限制（不是拒一切）"},
		{int(256), 256, "int（兼容）"},
		{int64(300), 300, "int64（兼容）"},
		{float64(32), 32, "float64（兼容）"},
	}
	for _, c := range cases {
		got := parseLimitMB(c.in)
		if got != c.want<<20 {
			t.Errorf("parseLimitMB(%#v) [%s] = %d 字节，期望 %dMB", c.in, c.desc, got, c.want)
		}
	}
	// 非法输入回退默认（不静默变成 0=不限，那会悄悄关掉防护）
	if got := parseLimitMB("abc"); got != int64(defaultMaxPackageMB)<<20 {
		t.Errorf("非法输入应回退默认，实际 %d", got)
	}
	if got := parseLimitMB(""); got != int64(defaultMaxPackageMB)<<20 {
		t.Errorf("空串应回退默认，实际 %d", got)
	}
	if got := parseLimitMB(struct{}{}); got != int64(defaultMaxPackageMB)<<20 {
		t.Errorf("意外类型应回退默认，实际 %d", got)
	}
}

// limitSettings 是能返回**真实形态**（string）配置值的 settings mock。
//
// 用真实 SettingsAPI 接口，走 p.Start() 这条生产路径，
// 从而验证"配置值 → p.maxPackageBytes"的完整链路。
type limitSettings struct {
	pmSettings
	maxMB string
	dir   string
}

func (s *limitSettings) Get(key string) (interface{}, error) {
	if key == "max_package_mb" {
		// ★ 返回 string：这是 config.PluginSettings.Get 的真实形态
		// （openFromStorage 返回 string）。返回 int 会让判据失去意义。
		return s.maxMB, nil
	}
	return nil, nil
}
func (s *limitSettings) GetCore(key string) (interface{}, error) {
	if key == "plugin.dir" {
		return s.dir, nil
	}
	return nil, nil
}
func (s *limitSettings) RegisterDef(sdk.ConfigDef) {}

// ★★★ 判据：配置项经 **真实 Start()** 生效。
//
// 这条覆盖的是我初版真正出错的环节：Get 返回 string，
// 而代码按 float64 断言 ⇒ switch 全不匹配 ⇒ 配置被静默忽略。
// 上面的 parseLimitMB 单测也喂 string，但只有走 Start() 才能证明
// "装配路径确实把 string 交给了它"。
func TestLimitConfigAppliedThroughStart(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		cfg  string
		want int64
	}{
		{"64", 64 << 20},
		{"0", 0},
		{"-1", 0},
	} {
		s := &limitSettings{maxMB: tc.cfg, dir: dir}
		sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
			Settings: s,
			EventBus: events.NewBus(),
		})
		p := &Plugin{name: "pluginmgr", mux: http.NewServeMux(), httpAddr: ""}
		if err := p.Start(sdkInst); err != nil {
			// HTTP 关闭（httpAddr 空）时 Start 仍应成功
			t.Fatalf("Start 失败: %v", err)
		}
		if p.maxPackageBytes != tc.want {
			t.Errorf("★ max_package_mb=%q 未生效: got %d, want %d（配置被静默忽略）",
				tc.cfg, p.maxPackageBytes, tc.want)
		}
	}
}

// ★★ 判据：**未配置时用默认值**（不能变成 0=不限而悄悄关掉防护）。
func TestLimitDefaultsWhenNotConfigured(t *testing.T) {
	s := &limitSettings{maxMB: "", dir: t.TempDir()}
	sdkInst := sdk.New("pluginmgr", sdk.SDKConfig{
		Settings: s,
		EventBus: events.NewBus(),
	})
	p := &Plugin{name: "pluginmgr", mux: http.NewServeMux(), httpAddr: ""}
	if err := p.Start(sdkInst); err != nil {
		t.Fatal(err)
	}
	if p.maxPackageBytes != int64(defaultMaxPackageMB)<<20 {
		t.Errorf("★ 未配置时应为默认 %dMB，实际 %d 字节（可能被误设为不限）",
			defaultMaxPackageMB, p.maxPackageBytes)
	}
}
