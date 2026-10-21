//go:build !onnxruntime

package qwen3vl

import (
	"context"
	"errors"
	"fmt"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

func init() {
	embedding.Register("qwen3vl", func(embedding.Config) (embedding.Provider, error) {
		return nil, fmt.Errorf("qwen3vl provider requires build tag 'onnxruntime' (go build -tags onnxruntime)")
	})
}

// Embedder 在未启用 onnxruntime 时不可用；保留类型是为了让引用它的代码在
// 默认构建下也能编译。真正的 ONNX 实现见 embedder_onnx.go。
type Embedder struct{}

func (e *Embedder) Embed(context.Context, embedding.Input) ([]float64, error) {
	return nil, errors.New("qwen3vl provider not available in this build")
}

func (e *Embedder) Info() embedding.Info { return embedding.Info{} }
func (e *Embedder) Close()               {}
