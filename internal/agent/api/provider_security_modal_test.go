package api

// 装饰器必须**透传模态能力声明**（2026-10-17 生产缺陷）。
//
// ## 缺陷链（完整复现）
//
//	配置 vision=true（本来就是对的）
//	  → LuaAdaptedProvider.Vision = true
//	  → ProviderManager.Register 包成 *secureProviderDecorator
//	  → ProviderSupportsVision 做 p.(ModalProvider) 断言
//	  → 装饰器漏实现 SupportsVision ⇒ 断言失败 ⇒ false
//	  → 判定"主模型不支持图片"
//	  → see_image 的 image_url 被塞进回退链
//	  → 找不到视觉源 ⇒ 「没有可用的视觉回退源」
//
// 症状极误导：模型明明能看图，内核一口咬定不支持；用户会去怀疑配置、
// 怀疑缓存、怀疑要重启 —— 全都不对，是装饰器把能力吃掉了。
//
// ## 判据怎么咬
//
// **走真实的 Register 包装路径**（不是直接构造装饰器），这样如果有人把
// 包装逻辑改到别处、或新增别的装饰器，判据仍然覆盖。

import (
	"context"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// newTestStore 造一个**真正会触发包装**的 store。
//
// ★ 关键：WrapProviderForSecurity 在 store == nil 时**直接返回原对象**
//
//	（见其实现：`if p == nil || store == nil { return p }`）。
//	我第一版判据传 nil，于是根本没包装，测的是未包装的 stub ⇒ 判据假绿、
//	变异删掉透传也不红。必须用非 nil store。
func newTestStore(t *testing.T) *security.Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return security.New(security.Options{
		Path:    t.TempDir() + "/secrets.db",
		KeyFn:   func() ([]byte, error) { return key, nil },
		Enabled: true,
	})
}

// modalStub 同时实现 Provider 与 ModalProvider。
type modalStub struct {
	name          string
	vision, audio bool
}

func (s *modalStub) Name() string          { return s.name }
func (s *modalStub) MaxContextTokens() int { return 1000 }
func (s *modalStub) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}
func (s *modalStub) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	return nil, nil
}
func (s *modalStub) SupportsVision() bool { return s.vision }
func (s *modalStub) SupportsAudio() bool  { return s.audio }

// ★★★ 判据：经 Register 包装后，ProviderSupportsVision 仍必须为 true。
func TestRegisterPreservesVisionCapability(t *testing.T) {
	inner := &modalStub{name: "visionllm", vision: true, audio: false}

	// 直接包一层装饰器（等价于 Register 内部做的事）
	wrapped := WrapProviderForSecurity(inner, newTestStore(t))

	// ★ 前置断言：必须真的被包装了，否则下面的测试毫无意义（我踩过这个坑）
	if _, ok := wrapped.(*secureProviderDecorator); !ok {
		t.Fatalf("前置失败：没有被包装（类型 %T）⇒ 判据会假绿", wrapped)
	}
	if !ProviderSupportsVision(wrapped) {
		t.Fatal("★ 装饰器吃掉了 vision 能力 —— " +
			"ProviderManager.Register 包装后 ProviderSupportsVision 变 false，" +
			"导致 see_image 的图被误判为需要回退（真事故 2026-10-17）")
	}
	if !wrapped.(ModalProvider).SupportsVision() {
		t.Fatal("★ 断言成 ModalProvider 后 SupportsVision 仍为 false")
	}
}

// ★★★ 判据：vision=false 的源不能被装饰器"变成" true。
//
// 反向保证：透传是照实传，不是一律返回 true（否则不支持视觉的源
// 会被当成能看，图片原样发出去被上游忽略 ⇒ 又一次静默剥离）。
func TestRegisterPreservesNonVisionCapability(t *testing.T) {
	inner := &modalStub{name: "textonly", vision: false, audio: false}
	wrapped := WrapProviderForSecurity(inner, newTestStore(t))

	if ProviderSupportsVision(wrapped) {
		t.Fatal("★ vision=false 的源被装饰器报成能看图 —— 图片会原样发出被上游忽略")
	}
}

// ★★★ 判据：audio 能力同样必须透传（同一类缺陷）。
func TestRegisterPreservesAudioCapability(t *testing.T) {
	inner := &modalStub{name: "audiollm", vision: false, audio: true}
	wrapped := WrapProviderForSecurity(inner, newTestStore(t))

	if !ProviderSupportsAudio(wrapped) {
		t.Fatal("★ 装饰器吃掉了 audio 能力 —— hear_audio 会被误判需要回退")
	}
	if ProviderSupportsVision(wrapped) {
		t.Fatal("audio-only 源不该报能看图")
	}
}

// ★★★ 判据：内层**没有**实现 ModalProvider 时，装饰器不得 panic 且报 false。
func TestDecoratorHandlesNonModalProvider(t *testing.T) {
	plain := &plainStub{name: "plain"}
	wrapped := WrapProviderForSecurity(plain, newTestStore(t))

	if ProviderSupportsVision(wrapped) {
		t.Fatal("内层没有 ModalProvider 却报能看图")
	}
	if ProviderSupportsAudio(wrapped) {
		t.Fatal("内层没有 ModalProvider 却报能听音频")
	}
}

type plainStub struct{ name string }

func (s *plainStub) Name() string          { return s.name }
func (s *plainStub) MaxContextTokens() int { return 1000 }
func (s *plainStub) Chat(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	return nil, nil
}
func (s *plainStub) ChatStream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	return nil, nil
}
