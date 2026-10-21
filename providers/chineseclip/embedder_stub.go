//go:build !onnxruntime

package chineseclip

import (
	"context"
	"errors"
	"fmt"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// 未启用 onnxruntime 构建标签时，chineseclip 仍注册到名字表，但打开即报错：
// 这样「provider 名写错」与「本次构建没带 ONNX」是两种可区分的失败，
// 而不是一句含糊的 unknown provider。
func init() {
	embedding.Register("chineseclip", func(embedding.Config) (embedding.Provider, error) {
		return nil, fmt.Errorf("chineseclip provider requires build tag 'onnxruntime' " +
			"(go build -tags onnxruntime)")
	})
}

// Embedder 在未启用 onnxruntime 时不可用；保留类型是为了让引用它的代码在
// 默认构建下也能编译。真正的 ONNX 实现见 embedder.go。
type Embedder struct{}

func (e *Embedder) Embed(context.Context, embedding.Input) ([]float64, error) {
	return nil, errors.New("chineseclip provider not available in this build")
}

func (e *Embedder) Info() embedding.Info { return embedding.Info{} }

func (e *Embedder) Close() {}
