package api

// 模态能力必须由**配置层**声明，而不是靠 adapter 的类型断言去猜。
//
// 现场（反复出现的"配了视觉却说没有视觉"）：内核用
// `provider.(ModalProvider)` 断言判定能力，而该断言要穿过 adapter 与
// 安全装饰器两层 —— 任一层漏实现方法，能力就静默变成 false。
//
// 用户指正（2026-10-27）：是否支持视觉是**配置事实**
//（core.llm.sources.<name>.vision=true），adapter 只负责格式转换。

import (
	"context"
	"testing"
)

// plainProvider 故意**不实现** ModalProvider —— 模拟"装饰器漏方法"的情形。
type plainProvider struct{ name string }

func (p *plainProvider) Name() string          { return p.name }
func (p *plainProvider) MaxContextTokens() int { return 8192 }
func (p *plainProvider) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}

func (p *plainProvider) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	close(ch)
	return ch, nil
}

func TestProviderManagerCapsDeclaredByConfig(t *testing.T) {
	m := NewProviderManager()
	p := &plainProvider{name: "proxy"}
	m.Register("proxy", p)

	// 断言兜底：plainProvider 不是 ModalProvider ⇒ false
	if ProviderSupportsVision(p) {
		t.Fatal("前提不成立：plainProvider 不该被断言成支持视觉")
	}
	// 配置声明后 ⇒ true（这就是"配置层说了算"）
	m.SetCapabilities("proxy", ProviderCaps{Vision: true})
	if !m.SupportsVision("proxy") {
		t.Fatal("配置声明 vision=true 后应判定为支持视觉，不能因为 provider 没实现 ModalProvider 就丢掉能力")
	}
	if m.SupportsAudio("proxy") {
		t.Fatal("未声明 audio 时不应判定支持音频")
	}
}

func TestProviderManagerCapsFallBackToAssertion(t *testing.T) {
	m := NewProviderManager()
	p := &plainProvider{name: "legacy"}
	m.Register("legacy", p)
	// 没声明（ok=false）⇒ 退回旧路径的断言结果
	if _, ok := m.Capabilities("legacy"); ok {
		t.Fatal("未声明时 Capabilities 应返回 ok=false")
	}
	if m.SupportsVision("legacy") {
		t.Fatal("未声明且非 ModalProvider 时应为 false")
	}
}
