package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// recordingProvider 记录核心传给 provider 的原始请求，用来断言
// 「核心不解释内容、只搬字节」这一契约。
type recordingProvider struct {
	got    []embedding.Input
	dim    int
	closed bool
}

func (p *recordingProvider) Embed(_ context.Context, in embedding.Input) ([]float64, error) {
	p.got = append(p.got, in)
	return make([]float64, p.dim), nil
}

func (p *recordingProvider) Info() embedding.Info {
	return embedding.Info{Dimension: p.dim, Fingerprint: "recording:1"}
}

func (p *recordingProvider) Close() { p.closed = true }

func TestProviderAdapterPassesOpaqueDataUnchanged(t *testing.T) {
	inner := &recordingProvider{dim: 3}
	adapted, err := AdaptProvider(inner)
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.Close()

	// 核心把媒体当作不透明字节搬运：既不解码也不改字节。
	raw := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}
	if _, err := adapted.EmbedImageDense(raw, "image/png"); err != nil {
		t.Fatal(err)
	}
	got := inner.got[0]
	if string(got.Data) != string(raw) {
		t.Fatalf("provider 收到的字节被改动: %v", got.Data)
	}
	if got.Modality != embedding.ModalityImage || got.MIME != "image/png" {
		t.Fatalf("模态/MIME 未原样传递: %+v", got)
	}
	if got.Purpose != embedding.PurposeDocument {
		t.Fatalf("用途应为 document: %q", got.Purpose)
	}

	if _, err := adapted.VectorizeDense("hello"); err != nil {
		t.Fatal(err)
	}
	if inner.got[1].Modality != embedding.ModalityText || inner.got[1].Text != "hello" {
		t.Fatalf("文本请求不正确: %+v", inner.got[1])
	}
}

func TestProviderAdapterRejectsWrongDimensionFromProvider(t *testing.T) {
	// provider 声明 3 维却返回 2 维：必须在进入存储前被拦下，
	// 否则一个维度错的向量会污染整个余弦检索。
	bad := &badDimProvider{}
	adapted, err := AdaptProvider(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer adapted.Close()
	if _, err := adapted.VectorizeDense("x"); err == nil {
		t.Fatal("维度不符时应返回错误")
	}
}

type badDimProvider struct{}

func (badDimProvider) Embed(context.Context, embedding.Input) ([]float64, error) {
	return []float64{1, 2}, nil
}
func (badDimProvider) Info() embedding.Info {
	return embedding.Info{Dimension: 3, Fingerprint: "bad:1"}
}
func (badDimProvider) Close() {}

func TestProviderAdapterCloseIsIdempotentAndStopsUse(t *testing.T) {
	inner := &recordingProvider{dim: 2}
	adapted, err := AdaptProvider(inner)
	if err != nil {
		t.Fatal(err)
	}
	adapted.Close()
	adapted.Close() // 重复关闭不应 panic 或二次 Close provider
	if !inner.closed {
		t.Fatal("Close 未传递到 provider")
	}
	if _, err := adapted.VectorizeDense("x"); err == nil {
		t.Fatal("关闭后应拒绝调用")
	}
	if adapted.Loaded() {
		t.Fatal("关闭后 Loaded() 应为 false")
	}
}

func TestModalityUnsupportedSentinelIsShared(t *testing.T) {
	// 内核侧的哨兵与公共契约的哨兵必须是同一个：provider 返回公共哨兵时，
	// 内核仍能用自己原有的名字识别。
	if !errors.Is(ErrModalityUnsupported, embedding.ErrUnsupportedModality) {
		t.Fatal("vector.ErrModalityUnsupported 与 embedding.ErrUnsupportedModality 未打通")
	}
	wrapped := errors.Join(embedding.ErrUnsupportedModality, errors.New("audio/wav"))
	if !errors.Is(wrapped, ErrModalityUnsupported) {
		t.Fatal("包装后的错误无法用内核哨兵识别")
	}
}
