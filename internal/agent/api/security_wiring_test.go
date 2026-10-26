package api

// 装配顺序判据。
//
// ## 为什么这个顺序值得单独立判据
//
// 安全模块的初始化必须在**任何 provider 注册之前**。装晚了的后果是：
// 先注册的那批 provider 没被装饰 ⇒ **静默漏掉一条出网路径**，
// 而且没有任何报错、日志也正常 —— 属于最难发现的那类失效。
//
// 而这个坑在本仓是真实存在的：
//   · `cmd/homed/main.go` 的 initLLMProviders（主路径）
//   · `internal/sdk/llm_impl.go` 的 ReloadFromConfig（配置热重载）
//   · `cmd/homed/failback.go` 的 runFailbackRecovery（降级救援）
//
// 三个地方都会 Register。前两个在 security 之后；failback 也必须之后
// （main.go 里它是第 174 行，安全模块是 162 行）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// ★★ 判据：**晚注册的 provider 也自动被装饰**（因为装饰挂在 Register 上）。
//
// 这条钉住的是"挂载点选择正确"：如果装饰挂在某个初始化函数里
// （而不是 Register），那么后注册的 provider 就会漏掉。
func TestLateRegistrationStillDecorated(t *testing.T) {
	dir := t.TempDir()
	store, err := security.InitGlobal(security.Options{
		Path:    filepath.Join(dir, "v.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	m := NewProviderManager()

	// ① 先注册一个
	var first string
	m.Register("early", &recordingProvider{name: "early", capture: &first})

	// ② 模拟"热重载"：Reset 后注册全新一批（ReloadFromConfig 的真实流程）
	m.Reset()
	var second string
	m.Register("late", &recordingProvider{name: "late", capture: &second})

	// ③ 再模拟 failback 的 rescue provider
	var third string
	m.Register("rescue", &recordingProvider{name: "rescue", capture: &third})

	// 三个都必须被装饰
	for _, tc := range []struct {
		name string
		got  *string
	}{{"late", &second}, {"rescue", &third}} {
		if _, err := m.Get(tc.name).Chat(context.Background(),
			&CompletionRequest{Messages: []Message{{Role: "user", Content: "t=" + tok}}}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(*tc.got, tok) {
			t.Errorf("provider %s 未被装饰（明文泄漏）: %s", tc.name, *tc.got)
		}
	}
	_ = first
	if store.Count() == 0 {
		t.Error("应登记变量")
	}
}

// ★ 判据：**已装饰的不再包第二层**（避免热路径白跑）。
func TestNoDoubleWrapping(t *testing.T) {
	dir := t.TempDir()
	if _, err := security.InitGlobal(security.Options{
		Path:    filepath.Join(dir, "v.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })

	m := NewProviderManager()
	base := &recordingProvider{name: "x", capture: new(string)}
	m.Register("a", base)
	inner := m.Get("a")

	// 把已装饰的实例再注册一次（某些装配路径会这么干）
	m.Register("b", inner)
	if _, isDecorator := m.Get("b").(*secureProviderDecorator); !isDecorator {
		t.Fatal("应当仍是装饰实例")
	}
	// 再包一层的话，类型仍是装饰器但会嵌套 —— 用指针比较确认是同一个
	if m.Get("b") != inner {
		t.Error("已装饰的实例不应被再包一层（会白跑一遍正则扫描）")
	}
}

// ★★ 判据：**main.go 里安全模块的初始化必须早于 initLLMProviders**。
//
// 这是源码级判据（读文件而不是跑逻辑），因为顺序错误在单测里
// 构造不出来（main 的装配流程没法在测试里复现）。
// 源码判据在此是有意为之：它盯的是一个**真实的、曾经坑过的**装配顺序。
func TestMainInitsSecurityBeforeProviders(t *testing.T) {
	src, err := os.ReadFile("../../../cmd/homed/main.go")
	if err != nil {
		t.Skipf("读不到 main.go（相对路径变了？）: %v", err)
	}
	text := string(src)

	// ★ 2026-10-26：安全装配搬进正式内核模块 internal/kernelmod/security。
	// main.go 里的真实装配点是 `securitymod.Prepare(...)`，而
	// `security.InitGlobal` 在模块实现里。判据必须同时盯住**两个真实点**：
	//   · main.go 的 Prepare 调用（装配时机）
	//   · 模块实现里的 InitGlobal（Prepare 不是空壳）
	// 只盯其中一个都会变成"改了实现就静默变绿"。
	iSec := strings.Index(text, "securitymod.Prepare(")
	if iSec < 0 {
		iSec = strings.Index(text, "security.InitGlobal")
	}
	modSrc, modErr := os.ReadFile("../../kernelmod/security/mod.go")
	modText := string(modSrc)
	if modErr != nil || !strings.Contains(modText, "sec.InitGlobal(") {
		t.Fatalf("security 模块实现里找不到 InitGlobal（%v）—— "+
			"Prepare() 退化成空壳，安全审查不会生效", modErr)
	}
	// ★ 锚点已随"内核模块化"改写（2026-10-16）。
	//
	// 原先锚 `initLLMProviders(cfg,`（那时它直接写在 main 里）。
	// 模块化后 **provider 注册搬进了 llm 模块**（`internal/kernelmod/llm`），
	// 它在 `RunKernelModules` 内部执行 ⇒ 旧的字符串在 main.go 里**已经不存在**，
	// 判据因"找不到"而失败（假红）。
	//
	// **注意：这不是放宽判据，而是把锚点换到新的真实装配点。**
	// 不变量本身没变：安全模块必须早于**任何 provider 注册**。
	// 新的等价表述：安全模块早于 `RunKernelModules`（llm 模块在它内部）。
	//
	// 为防止"换个锚点就变绿"式的偷懒，另加两条硬校验（见下）。
	iProv := strings.Index(text, "RunKernelModules(")
	if iSec < 0 {
		t.Fatal("main.go 里找不到 securitymod.Prepare / security.InitGlobal —— 安全模块根本没被装配")
	}
	if iProv < 0 {
		t.Fatal("main.go 里找不到 RunKernelModules —— 内核模块装配点没了")
	}
	if iSec > iProv {
		t.Fatalf("装配顺序错误：security.InitGlobal 在第 %d 字节，RunKernelModules 在第 %d 字节\n"+
			"必须先初始化安全模块 —— 内核模块装配时会注册 LLM provider，"+
			"晚于它则 provider 不会被装饰（静默漏掉一条出网路径）",
			iSec, iProv)
	}
	// ★ 加固 ①：provider 注册**确实**在内核模块里（否则新锚点就是空的 ——
	// 那等于把判据锚在一个与 provider 无关的位置上）。
	// 判据：kernelmod/llm 模块必须注册，且其 Init 里出现 initLLMProviders。
	llmSrc, lerr := os.ReadFile("../../kernelmod/llm/mod.go")
	if lerr != nil {
		t.Fatalf("读不到 llm 模块（provider 注册应在这里）: %v", lerr)
	}
	if !strings.Contains(string(llmSrc), "initLLMProviders") {
		t.Fatal("★ llm 模块里没有 initLLMProviders —— " +
			"那么上面锚 RunKernelModules 就与 provider 注册无关了（判据会变空）")
	}
	// ★ 加固 ②：llm 模块必须是**真注册过**的（不是死代码）
	allSrc, aerr := os.ReadFile("../../kernelmod/all/all.go")
	if aerr != nil {
		t.Fatalf("读不到模块总表: %v", aerr)
	}
	if !strings.Contains(string(allSrc), "kernelmod/llm") {
		t.Fatal("★ llm 模块没被 import ⇒ 它的 init() 不跑 ⇒ provider 永不注册")
	}

	// failback 也要在之后
	iFail := strings.Index(text, "runFailbackRecovery")
	if iFail >= 0 && iSec > iFail {
		t.Error("runFailbackRecovery 在安全模块之前 —— rescue provider 会漏装饰")
	}
}

// ★ 判据：阶段断言 —— 源码里装饰发生在 Register 内部（挂载点正确）。
func TestDecorateHappensInsideRegister(t *testing.T) {
	src, err := os.ReadFile("provider.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	// Register 函数体内必须出现 WrapProviderForSecurity
	i := strings.Index(text, "func (m *ProviderManager) Register(")
	if i < 0 {
		t.Fatal("找不到 Register")
	}
	// 取到下一个顶层 func 为止
	rest := text[i:]
	if j := strings.Index(rest[1:], "\nfunc "); j > 0 {
		rest = rest[:j+1]
	}
	if !strings.Contains(rest, "WrapProviderForSecurity") {
		t.Fatal("Register 函数体内没有装饰调用 —— 装饰被挪走了（后注册的 provider 会漏掉）")
	}
}
