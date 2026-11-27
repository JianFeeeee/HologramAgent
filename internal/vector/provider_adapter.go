package vector

import (
	"context"
	"sync"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// ProviderAdapter translates the public model-neutral embedding.Provider SPI
// to the small internal interface used by the existing memory consumers.
// Model selection, media decoding, preprocessing, and runtime details remain
// entirely inside the selected provider.
type ProviderAdapter struct {
	provider embedding.Provider
	info     embedding.Info

	mu     sync.RWMutex
	closed bool
}

// AdaptProvider validates and wraps a public provider for internal memory use.
func AdaptProvider(provider embedding.Provider) (*ProviderAdapter, error) {
	info := provider.Info()
	if err := embedding.ValidateInfo(info); err != nil {
		return nil, err
	}
	return &ProviderAdapter{provider: provider, info: info}, nil
}

func (a *ProviderAdapter) VectorizeDense(text string) ([]float64, error) {
	return a.embed(embedding.Input{
		Modality: embedding.ModalityText,
		Purpose:  embedding.PurposeQuery,
		Text:     text,
	})
}

func (a *ProviderAdapter) EmbedImageDense(data []byte, mime string) ([]float64, error) {
	return a.embed(embedding.Input{
		Modality: embedding.ModalityImage,
		Purpose:  embedding.PurposeDocument,
		Data:     data,
		MIME:     mime,
	})
}

func (a *ProviderAdapter) embed(input embedding.Input) ([]float64, error) {
	a.mu.RLock()
	closed := a.closed
	a.mu.RUnlock()
	if closed {
		return nil, context.Canceled
	}
	vec, err := a.provider.Embed(context.Background(), input)
	if err != nil {
		return nil, err
	}
	if err := embedding.ValidateVector(vec, a.info.Dimension); err != nil {
		return nil, err
	}
	return vec, nil
}

func (a *ProviderAdapter) Fingerprint() string { return a.info.Fingerprint }
func (a *ProviderAdapter) Dim() int            { return a.info.Dimension }

// Modalities 报告该空间支持的输入模态（text/image/...）。
//
// 模态是**可选能力**：MultimodalEmbedder 契约里没有它，状态查询按接口断言取用，
// 所以这里既不改公开接口，也不影响其它实现（核心也不硬编码任何模型名）。
func (a *ProviderAdapter) Modalities() []string {
	out := make([]string, 0, len(a.info.Modalities))
	for _, m := range a.info.Modalities {
		out = append(out, string(m))
	}
	return out
}

func (a *ProviderAdapter) Loaded() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.closed
}

func (a *ProviderAdapter) Close() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	a.provider.Close()
}
