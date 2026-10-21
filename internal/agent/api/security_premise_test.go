package api

// 前提验证：**安全模块能否稳在 provider 层**。
//
// 设计依据：docs/zh/kernel-secret-firewall-design.md §5.1。
//
// 整件事成立的前提是两条：
//
//	P1. 所有发往 LLM 的调用都经 `Provider.Chat` / `Provider.ChatStream`
//	    （没有绕过接口的直发路径）
//	P2. `ProviderManager.Register` 是所有 provider 的**唯一入口**
//	    ⇒ 在 Register 处包一层装饰器，就能覆盖全部取用路径
//	       （Get / Default / OrderedProviders / ResolveForModel 都从同一 map 取）
//
// 若 P1 或 P2 不成立，安全模块必须下沉到更底层（HTTP 客户端），改动面大得多。
// ⇒ 先验它，再写实现（本仓纪律：前提不成立时不要继续设计上层）。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// countingProvider 记录它被调用了几次、收到什么消息。
type countingProvider struct {
	calls  int
	seen   []string
	stream int
}

func (c *countingProvider) Name() string { return "counting" }

func (c *countingProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	c.calls++
	for _, m := range req.Messages {
		c.seen = append(c.seen, m.Content)
	}
	return &CompletionResponse{Content: "ok"}, nil
}

func (c *countingProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	c.stream++
	ch := make(chan StreamChunk, 1)
	close(ch)
	return ch, nil
}

func (c *countingProvider) MaxContextTokens() int { return 8192 }

// ★★ P2：装饰 Register ⇒ **所有取用路径都拿到装饰后的实例**。
//
// 这是安全模块能"一处生效、处处覆盖"的机制保证。
func TestPremise_RegisterDecorateCoversAllAccessPaths(t *testing.T) {
	m := NewProviderManager()

	inner := &countingProvider{}
	var wrapped Provider
	// 模拟安全模块：在 Register 处包一层
	m.Register("p1", &proxyProvider{inner: inner, onCall: func() { wrapped = nil }})
	_ = wrapped

	// ① Get 拿到的是装饰后的
	got := m.Get("p1")
	if got == nil {
		t.Fatal("Get 返回 nil")
	}
	if _, isProxy := got.(*proxyProvider); !isProxy {
		t.Fatalf("Get 未拿到装饰后的实例，实际类型 %T", got)
	}
	// ② Default 也拿到装饰后的
	if _, isProxy := m.Default().(*proxyProvider); !isProxy {
		t.Fatal("Default 未拿到装饰后的实例")
	}
	// ③ OrderedProviders 也是
	ord := m.OrderedProviders()
	if len(ord) != 1 {
		t.Fatalf("OrderedProviders 数量 %d", len(ord))
	}
	if _, isProxy := ord[0].(*proxyProvider); !isProxy {
		t.Fatal("OrderedProviders 未拿到装饰后的实例")
	}
	// ④ ResolveForModel 也是
	res := m.ResolveForModel("")
	if len(res) == 0 {
		t.Fatal("ResolveForModel 为空")
	}
	if _, isProxy := res[0].(*proxyProvider); !isProxy {
		t.Fatal("ResolveForModel 未拿到装饰后的实例")
	}
}

// ★★ P1：经 manager 取到的 provider，其 Chat 必经装饰器。
//
// 这条证明"安全模块能看见每一条出网请求"。
func TestPremise_DecoratorSeesEveryChat(t *testing.T) {
	m := NewProviderManager()
	inner := &countingProvider{}
	var seen int
	m.Register("p1", &proxyProvider{inner: inner, onCall: func() { seen++ }})

	p := m.Get("p1")
	if _, err := p.Chat(context.Background(), &CompletionRequest{
		Messages: []Message{{Role: "user", Content: "hello"}},
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 1 {
		t.Fatalf("装饰器应看到 1 次 Chat，实际 %d", seen)
	}
	if inner.calls != 1 {
		t.Fatalf("内层应被调 1 次，实际 %d", inner.calls)
	}
	// ChatStream 同样要经过
	if _, err := p.ChatStream(context.Background(), &CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("ChatStream 也应经装饰器，实际 seen=%d", seen)
	}
}

// ★★ 关键能力验证：装饰器**能改写请求**（这正是安全模块要做的替换）。
func TestPremise_DecoratorCanRewriteMessages(t *testing.T) {
	m := NewProviderManager()
	inner := &countingProvider{}
	// 模拟安全模块：把明文换成别名
	m.Register("p1", &proxyProvider{inner: inner, onCall: func() {}})

	// 直接用能改写的装饰器
	m.Register("p2", &rewriteProvider{inner: inner})

	req := &CompletionRequest{Messages: []Message{
		{Role: "user", Content: "token=SECRET-XYZ"},
	}}
	if _, err := m.Get("p2").Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// 内层必须收到**改写后**的内容（证明改写真的生效、不是只读）
	found := false
	for _, s := range inner.seen {
		if strings.Contains(s, "SECRET-XYZ") {
			t.Fatal("明文被原样透传，改写未生效")
		}
		if strings.Contains(s, "@{sec_") {
			found = true
		}
	}
	if !found {
		t.Fatalf("内层未收到改写后的别名，实际收到: %v", inner.seen)
	}
}

// ★ 覆盖面：ChatStream 也必须能被装饰（流式是主路径之一）。
func TestPremise_StreamAlsoDecorated(t *testing.T) {
	m := NewProviderManager()
	inner := &countingProvider{}
	m.Register("p1", &proxyProvider{inner: inner, onCall: func() {}})
	if _, err := m.Get("p1").ChatStream(context.Background(), &CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if inner.stream != 1 {
		t.Fatalf("内层 ChatStream 应被调 1 次，实际 %d", inner.stream)
	}
}

// ---------------- 测试替身 ----------------

// proxyProvider 是"只观察不修改"的装饰器。
type proxyProvider struct {
	inner  Provider
	onCall func()
}

func (p *proxyProvider) Name() string { return p.inner.Name() }
func (p *proxyProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if p.onCall != nil {
		p.onCall()
	}
	return p.inner.Chat(ctx, req)
}
func (p *proxyProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	if p.onCall != nil {
		p.onCall()
	}
	return p.inner.ChatStream(ctx, req)
}
func (p *proxyProvider) MaxContextTokens() int { return p.inner.MaxContextTokens() }

// rewriteProvider 是"会改写请求"的装饰器（模拟安全模块）。
type rewriteProvider struct{ inner Provider }

func (p *rewriteProvider) Name() string { return p.inner.Name() }
func (p *rewriteProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	// 模拟：把明文换成别名（真实实现会用模式库 + HMAC 键）
	for i := range req.Messages {
		if strings.Contains(req.Messages[i].Content, "SECRET-XYZ") {
			req.Messages[i].Content = "token=@{sec_deadbeef}"
		}
	}
	return p.inner.Chat(ctx, req)
}
func (p *rewriteProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	return p.inner.ChatStream(ctx, req)
}
func (p *rewriteProvider) MaxContextTokens() int { return p.inner.MaxContextTokens() }

// ---------------- 接线判据（装饰器真的挂在 Register 上） ----------------

// ★★ 判据：经 Register 登记后，取出的 provider 就是**装饰过的**，
// 且出向明文被换成了别名。
//
// 这是端到端接线判据：它同时验证
//
//	① Register 挂上了装饰器
//	② 装饰器能拿到全局单例
//	③ 替换真的发生了
func TestWired_RegisterDecoratesAndScrubs(t *testing.T) {
	dir := t.TempDir()
	store, err := security.InitGlobal(security.Options{
		Path:    filepath.Join(dir, "vars.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	var got string
	inner := &recordingProvider{name: "wired", capture: &got}
	m := NewProviderManager()
	m.Register("p", inner)

	// 取出（任何一条取用路径都应拿到装饰后的实例）
	p := m.Default()
	if p == nil {
		t.Fatal("Default 返回 nil")
	}
	req := &CompletionRequest{Messages: []Message{{Role: "user", Content: "token=" + tok}}}
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(got, tok) {
		t.Fatalf("明文泄漏到 provider: %s", got)
	}
	if !strings.Contains(got, "安全模块自动拦截") {
		t.Errorf("未见拦截提示: %s", got)
	}
	if store.Count() != 1 {
		t.Errorf("应登记 1 条变量，实际 %d", store.Count())
	}
	// 记录里不得有明文
	b, _ := json.Marshal(store.List())
	if strings.Contains(string(b), tok) {
		t.Error("变量记录泄露明文")
	}
}

// ★ 判据：安全模块未初始化时，Register **不介入**（零成本）。
func TestWired_NoStoreMeansNoInterference(t *testing.T) {
	security.SetGlobal(nil)
	t.Cleanup(func() { security.SetGlobal(nil) })

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	var got string
	m := NewProviderManager()
	m.Register("p", &recordingProvider{name: "plain", capture: &got})

	if _, err := m.Default().Chat(context.Background(),
		&CompletionRequest{Messages: []Message{{Role: "user", Content: "token=" + tok}}}); err != nil {
		t.Fatal(err)
	}
	if got != "token="+tok {
		t.Errorf("未启用安全模块时不得改动内容: %q", got)
	}
}

// ★ 判据：回填把别名换回明文（入向通道）。
func TestWired_FillToolArgs(t *testing.T) {
	dir := t.TempDir()
	store, err := security.InitGlobal(security.Options{
		Path:    filepath.Join(dir, "vars.json"),
		KeyFn:   func() ([]byte, error) { return []byte("0123456789abcdef0123456789abcdef"), nil },
		Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { security.SetGlobal(nil) })

	const tok = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	alias, err := store.Submit(tok, "")
	if err != nil {
		t.Fatal(err)
	}

	args := map[string]interface{}{"token": "@{" + alias + "}", "other": "unchanged"}
	n, used := security.FillToolArgs("some_plugin_tool", args)
	if n != 1 {
		t.Fatalf("应回填 1 处，实际 %d", n)
	}
	if args["token"] != tok {
		t.Errorf("回填结果不对: %v", args["token"])
	}
	if args["other"] != "unchanged" {
		t.Error("无关字段不应被改动")
	}
	if len(used) != 1 || used[0] != alias {
		t.Errorf("应报告别名: %v", used)
	}
}

// recordingProvider 记录它收到的最后一条消息内容。
type recordingProvider struct {
	name    string
	capture *string
}

func (r *recordingProvider) Name() string { return r.name }
func (r *recordingProvider) Chat(_ context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	if req != nil && len(req.Messages) > 0 {
		*r.capture = req.Messages[0].Content
	}
	return &CompletionResponse{}, nil
}
func (r *recordingProvider) ChatStream(_ context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	if req != nil && len(req.Messages) > 0 {
		*r.capture = req.Messages[0].Content
	}
	ch := make(chan StreamChunk)
	close(ch)
	return ch, nil
}
func (r *recordingProvider) MaxContextTokens() int { return 1000 }
