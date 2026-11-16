package test

// internal/agent/api 的**多源调度**判据 —— 面向接口的黑盒。
//
// 入口：NewProviderManager + Register/Get/List/SetDefault/ResolveForModel/
// OrderedProviders/ReportStatus/MarkPermanent/ResetAvailability，断言只看
// 返回的 Provider 序列与 IsAvailable 的真值。
//
// ## 原判据的缺口
//
// `api_contract_test.go` 已有一部分 ProviderManager 判据（注册/默认/熔断
// 单点），但**候选链的排序规则**零覆盖。而排序是这个模块的本体职责：
//
//	匹配模型名 > Priority 高 > 默认源 > 名字字典序
//
// 排错了不会报错，只会让"该用的源没用上/不该用的先上了"。

import (
	"context"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// plainProvider 是只有名字的源（不实现 RoutableProvider）。
type plainProvider struct{ name string }

func (p *plainProvider) Name() string          { return p.name }
func (p *plainProvider) MaxContextTokens() int { return 8192 }
func (p *plainProvider) Chat(ctx context.Context, req *api.CompletionRequest) (*api.CompletionResponse, error) {
	return &api.CompletionResponse{Content: p.name}, nil
}
func (p *plainProvider) ChatStream(ctx context.Context, req *api.CompletionRequest) (<-chan api.StreamChunk, error) {
	return nil, context.Canceled
}

// routableProvider 声明模型名与优先级（实现 RoutableProvider）。
type routableProvider struct {
	plainProvider
	model string
	prio  int
}

func (p *routableProvider) Model() string { return p.model }
func (p *routableProvider) Priority() int { return p.prio }

// names 把候选链压成名字，便于断言。
func names(ps []api.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

// eq 比较两个切片。
func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- 判据 1：模型名完全匹配的源排最前 ----

// TestResolveForModelPutsExactMatchFirst：请求指定模型时，声明该模型的源
// 必须排在最前，无论它的 Priority 与默认源如何。
//
// 排错了的后果：用户显式指定了模型，却用了另一个源的同名（或任意）模型
// 的输出 —— 看起来"能用"，但实际没按配置走。
func TestResolveForModelPutsExactMatchFirst(t *testing.T) {
	m := api.NewProviderManager()
	// 默认源 prio 最高、且不是匹配的那个。
	m.Register("alpha", &routableProvider{plainProvider: plainProvider{name: "alpha"}, model: "model-a", prio: 10})
	m.Register("beta", &routableProvider{plainProvider: plainProvider{name: "beta"}, model: "model-b", prio: 1})
	m.Register("gamma", &routableProvider{plainProvider: plainProvider{name: "gamma"}, model: "model-c", prio: 5})
	if err := m.SetDefault("alpha"); err != nil {
		t.Fatal(err)
	}

	got := names(m.ResolveForModel("model-b"))
	if len(got) == 0 || got[0] != "beta" {
		t.Fatalf("★ 指定 model-b，候选链首位是 %v（期望 beta）", got)
	}
	// 其余源仍作为回退（不是被裁掉）
	if len(got) != 3 {
		t.Fatalf("★ 候选链应包含全部 3 个源（匹配的是优先，不是独占），实际 %v", got)
	}
}

// ---- 判据 2：没有匹配时按 Priority 降序 ----

// TestResolveOrdersByPriorityWithoutMatch：不指定模型（或模型无匹配）时，
// 候选链按 Priority 从高到低。
//
// 这是"主 agent 要强推理、子 agent要低延迟"能够各自配置的基础。
func TestResolveOrdersByPriorityWithoutMatch(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("low", &routableProvider{plainProvider: plainProvider{name: "low"}, model: "x", prio: 1})
	m.Register("high", &routableProvider{plainProvider: plainProvider{name: "high"}, model: "y", prio: 9})
	m.Register("mid", &routableProvider{plainProvider: plainProvider{name: "mid"}, model: "z", prio: 5})

	got := names(m.ResolveForModel(""))
	if !eq(got, []string{"high", "mid", "low"}) {
		t.Fatalf("★ 无模型匹配时候选链 = %v，期望按 Priority 降序 [high mid low]", got)
	}
	// "auto" 与空串等价（不当作模型名去匹配）
	if got := names(m.ResolveForModel("auto")); !eq(got, []string{"high", "mid", "low"}) {
		t.Fatalf("★ model=%q 时应等同不指定，实际 %v", "auto", got)
	}
}

// ---- 判据 3：Priority 相同时默认源优先 ----

// TestDefaultProviderWinsOnPriorityTie：Priority 相同的源之间，默认源排前。
//
// 否则"设了默认源"这件事在候选链里看不出来 —— 用户改了配置却没有任何效果。
func TestDefaultProviderWinsOnPriorityTie(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("aaa", &routableProvider{plainProvider: plainProvider{name: "aaa"}, model: "m1", prio: 5})
	m.Register("zzz", &routableProvider{plainProvider: plainProvider{name: "zzz"}, model: "m2", prio: 5})
	if err := m.SetDefault("zzz"); err != nil {
		t.Fatal(err)
	}

	if got := names(m.ResolveForModel("")); got[0] != "zzz" {
		t.Fatalf("★ Priority 相同时默认源应排首位，实际候选链 %v（aaa 名字更小却在前）", got)
	}
}

// TestTransientFailureCoolsDownThenRecovers：5xx 之类判成临时 ⇒ 熔断，
// 显式 Reset 后恢复可用。
//
// 冷却时长由实现的退避策略决定，不是契约（钉死秒数会在调参时无意义地红）。
// 但要钉住**可恢复**：不可恢复的熔断等于把源永久踢出服务，
// 一次网络抖动就能造成这个后果。
func TestTransientFailureCoolsDownThenRecovers(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("p", &plainProvider{name: "p"})

	m.ReportStatus("p", 503)
	if m.IsAvailable("p") {
		t.Fatal("★ 503 之后仍报告可用 —— 熔断没生效，故障源会一直被选中")
	}
	m.ResetAvailability("p")
	if !m.IsAvailable("p") {
		t.Fatal("★ ResetAvailability 之后仍不可用 —— 熔断无法解除")
	}
}

// ---- 判据 5：不可用 ≠ 从候选链消失 ----

// TestUnavailableProviderStillInChain：被熔断的源仍留在候选链（只是不可用），
// 而不被静默移除。
//
// 为何区别重要：链里留着它，调用方可以记日志/展示"该源正在冷却"；
// 静默移除则让"配置了三个源但只有一个在跑"无从发现。
func TestUnavailableProviderStillInChain(t *testing.T) {
	m := api.NewProviderManager()
	m.Register("a", &plainProvider{name: "a"})
	m.Register("b", &plainProvider{name: "b"})
	m.MarkUnavailable("a")

	if got := m.List(); len(got) != 2 {
		t.Fatalf("★ 熔断后 List 只剩 %v —— 配置的源不该消失", got)
	}
	if len(m.OrderedProviders()) != 2 {
		t.Fatalf("★ 熔断后候选链只剩 %d 个", len(m.OrderedProviders()))
	}
	if m.IsAvailable("a") {
		t.Fatal("★ MarkUnavailable 后仍判可用")
	}
}

// ---- 判据 6：能力声明优先于探测，未声明时回落到探测 ----

// TestExplicitCapabilitiesOverrideProbe：SetCapabilities 声明过的源，
// SupportsVision 用声明值；没声明的源回落到 Provider 自身能力。
//
// 这条守的是"配置说了算"：某些源的 vision 支持无法从接口探测（例如同一个
// adapter 接不同上游），只能靠声明。
func TestExplicitCapabilitiesOverrideProbe(t *testing.T) {
	m := api.NewProviderManager()
	// 该 provider 的 MaxContextTokens 能探测，但 Vision 探测无从得知 ⇒ 默认 false。
	m.Register("declared", &plainProvider{name: "declared"})
	m.SetCapabilities("declared", api.ProviderCaps{Vision: true})

	if !m.SupportsVision("declared") {
		t.Fatal("★ 声明了 Vision=true，SupportsVision 却报 false —— 配置被探测覆盖")
	}
	// 未声明 + 不可探测 ⇒ 保守为 false（不许乐观猜）
	m.Register("unknown", &plainProvider{name: "unknown"})
	if m.SupportsVision("unknown") {
		t.Fatal("★ 未声明的源被乐观判为支持 vision")
	}
}

// ---- 判据 7：空管理器不 panic，且如实报"没有默认源" ----

// TestEmptyManagerReportsNoDefault：没有注册任何源时 QuickChat 必须返回
// 明确错误，而不是 panic 或空回复。
//
// 空管理器出现在"配置全空/首次启动"场景，那里最需要可操作的错误。
func TestEmptyManagerReportsNoDefault(t *testing.T) {
	m := api.NewProviderManager()
	resp, err := m.QuickChat(context.Background(), "ping")
	if err == nil {
		t.Fatalf("★ 空管理器 QuickChat 返回成功（resp=%+v）", resp)
	}
	if m.Default() != nil {
		t.Fatal("★ 空管理器 Default() 非 nil")
	}
	if m.ProviderCount() != 0 {
		t.Fatalf("★ 空管理器 ProviderCount = %d", m.ProviderCount())
	}
	// 未注册的名字查询必须安全（返回 nil），不 panic。
	//
	// ★ 刻意**不**断言 IsAvailable("ghost")：那个方法回答的是"有没有被熔断"
	// （无熔断记录 ⇒ 不拦），存在性由 Get 回答。要求它同时管存在性，
	// 等于给一个查询塞不该它承担的职责。
	if m.Get("ghost") != nil {
		t.Fatal("★ Get 未注册的源返回非 nil")
	}
}

// ---- 支撑 ----
