//go:build onnxruntime

// Package qwen3vlgen implements the generation.Provider SPI on the same
// Qwen3-VL weights this package already uses for embeddings.
//
// 一份权重，两种模式
// ------------------
// 向量（高频，检索时）：TokenEmbedding.onnx + Transformer.onnx（末 token 池化）
// 生成（低频，蒸馏时）：TokenEmbedding.onnx + SequenceWithHead.onnx（tied head）
//
// 两者共享 TokenEmbedding 段与全部 28 层 Transformer 的权重语义；生成侧
// 多出来的那张图只是「不池化 + 接 tied head」。embed_config.json 的
// `generation` 段声明了这个能力，Open 时据此判断模型目录是否可用作生成。
//
// tied lm_head：零新增权重
// ----------------------
// Qwen3-VL 的 tie_word_embeddings=True，输出层就是 embed_tokens 转置
// （实测 safetensors 里 625 个张量没有独立 lm_head）。embed_tokens 已在
// TokenEmbedding.onnx 里，因此输出层不需要另一份权重。
//
// 没有 KV cache
// -------------
// 自回归解码若每步全量前向是 O(N²)。拆分任务的输出极短（实测 3~7 行，
// N≈200），且蒸馏是定时低频任务，不是检索热路径。为此引入 KV cache
// 状态机会让实现复杂度翻倍，收益只在这条低频路径上。
//
// 若生成侧将来进入热路径，先实测 O(N²) 的实际延迟再决定。
package qwen3vlgen

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/JianFeeeee/HologramAgent/pkg/generation"
	"github.com/JianFeeeee/HologramAgent/providers/qwen3vl"
)

func init() {
	generation.Register("qwen3vl", func(cfg generation.Config) (generation.Provider, error) {
		return New(cfg.Options["model_dir"])
	})
}

type genConfig struct {
	Generation struct {
		Graph      string `json:"graph"`
		TiedLMHead bool   `json:"tied_lm_head"`
		KVCache    bool   `json:"kv_cache"`
		Outputs    string `json:"outputs"`
	} `json:"generation"`
	Dim          int     `json:"dim"`
	MaxLength    int     `json:"max_length"`
	RopeTheta    float64 `json:"rope_theta"`
	MRopeSection []int   `json:"mrope_section"`
}

// Generator 是同一份 Qwen3-VL 权重上的生成侧。
type Generator struct {
	mu sync.Mutex

	dir     string
	cfg     genConfig
	tok     *qwen3vl.Tokenizer
	token   *ort.DynamicAdvancedSession
	seqHead *ort.DynamicAdvancedSession
	fp      string
	close   sync.Once
}

// New 打开一个生成侧实例。modelDir 必须是含 embed_config.json 与
// SequenceWithHead.onnx 的导出目录。
func New(modelDir string) (*Generator, error) {
	if modelDir == "" {
		return nil, fmt.Errorf("qwen3vlgen: model dir not specified")
	}
	raw, err := os.ReadFile(filepath.Join(modelDir, "embed_config.json"))
	if err != nil {
		return nil, fmt.Errorf("qwen3vlgen: read embed_config.json: %w", err)
	}
	var cfg genConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("qwen3vlgen: parse embed_config.json: %w", err)
	}
	if cfg.Generation.Graph == "" {
		return nil, fmt.Errorf("qwen3vlgen: 该模型目录未导出生成侧图（embed_config.json 缺 generation.graph）；" +
			"用 scripts/export_qwen3vl_embedding_onnx.py 重新导出")
	}
	if !cfg.Generation.TiedLMHead {
		return nil, fmt.Errorf("qwen3vlgen: 只支持 tied lm_head（该模型声明 tied_lm_head=false）")
	}
	if cfg.Generation.KVCache {
		return nil, fmt.Errorf("qwen3vlgen: 该导出带 KV cache，本实现尚未支持（会把生成跑成错的）")
	}
	if cfg.Dim <= 0 || len(cfg.MRopeSection) != 3 {
		return nil, fmt.Errorf("qwen3vlgen: incompatible config dim=%d mrope=%v", cfg.Dim, cfg.MRopeSection)
	}

	tok, err := qwen3vl.LoadTokenizer(modelDir)
	if err != nil {
		return nil, fmt.Errorf("qwen3vlgen: %w", err)
	}
	if !ort.IsInitialized() {
		if lib := qwen3vl.FindOnnxLib(); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("qwen3vlgen: init onnx env: %w", err)
		}
	}

	token, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "TokenEmbedding.onnx"),
		[]string{"input_ids"}, []string{"hidden"}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("qwen3vlgen: create token session: %w", err)
	}
	seqHead, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, cfg.Generation.Graph),
		[]string{"hidden", "deepstack_0", "deepstack_1", "deepstack_2",
			"rotary_cos", "rotary_sin", "causal_mask"},
		[]string{"logits"}, nil,
	)
	if err != nil {
		token.Destroy()
		return nil, fmt.Errorf("qwen3vlgen: create %s session: %w", cfg.Generation.Graph, err)
	}

	return &Generator{
		dir: modelDir, cfg: cfg, tok: tok, token: token, seqHead: seqHead,
		fp: fingerprint(modelDir),
	}, nil
}

// Generate 跑一次自回归解码。
//
// 文本侧走纯文本输入：生成蒸馏拆分的输入永远是文本（对话记录），不进
// 视觉塔 —— 那条路的 DeepStack 相加是为视觉 token 准备的，纯文本时三项全零。
func (g *Generator) Generate(ctx context.Context, req generation.Request) (generation.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if req.JSONSchema != "" {
		// 这条路径的约束靠解码时的语法/采样实现，不能靠 ONNX 图 ——
		// 图只到 logits。schema 约束由 provider 之外的机制（如结构化采样）
		// 完成；本 provider 报不支持而不是静默忽略。
		return generation.Response{}, fmt.Errorf(
			"qwen3vlgen: 不支持 json schema 约束（返回 ErrSchemaUnsupported 会让调用方改用自由文本，"+
				"那正是拆分场景要避免的）：%w", generation.ErrSchemaUnsupported)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 256
	}
	ids := g.tok.Encode(req.Prompt)
	if len(ids) == 0 {
		return generation.Response{}, fmt.Errorf("qwen3vlgen: prompt 为空")
	}
	maxLen := g.cfg.MaxLength
	if maxLen <= 0 {
		maxLen = 2560
	}
	if len(ids) > maxLen-1 {
		ids = ids[:maxLen-1]
	}

	var out strings.Builder
	truncated := true
	for step := 0; step < maxTokens; step++ {
		if err := ctx.Err(); err != nil {
			return generation.Response{}, err
		}
		next, err := g.forward(ids)
		if err != nil {
			return generation.Response{}, err
		}
		tokenText, isStop := g.decodeToken(next)
		if isStop {
			truncated = false
			break
		}
		out.WriteString(tokenText)
		ids = append(ids, next)
		if len(ids) >= maxLen-1 {
			truncated = false
			break
		}
	}
	return generation.Response{Text: out.String(), Truncated: truncated}, nil
}

// forward 跑一次前向，返回 argmax token id。
//
// 全量前向：没有 KV cache，每步重算全序列。蒸馏输出短（N≈200），
// 这个代价是可接受的 —— 见包注释。
func (g *Generator) forward(ids []int) (int, error) {
	seq := len(ids)
	hidden, err := g.runTokenEmbedding(ids)
	if err != nil {
		return 0, err
	}
	cos, sin := rotary(g.cfg.RopeTheta, g.cfg.MRopeSection, seq)
	causal := causalMask(seq)
	zero := make([]float32, len(hidden))

	var deepTensors []*ort.Tensor[float32]
	defer func() {
		for _, t := range deepTensors {
			t.Destroy()
		}
	}()
	tensors := make([]ort.Value, 0, 7)
	hT, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(g.cfg.Dim)}, hidden)
	if err != nil {
		return 0, fmt.Errorf("qwen3vlgen: hidden tensor: %w", err)
	}
	tensors = append(tensors, hT)
	for i := 0; i < 3; i++ {
		t, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(g.cfg.Dim)}, zero)
		if err != nil {
			return 0, fmt.Errorf("qwen3vlgen: deepstack tensor: %w", err)
		}
		deepTensors = append(deepTensors, t)
		tensors = append(tensors, t)
	}
	cosT, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, cos)
	if err != nil {
		return 0, err
	}
	tensors = append(tensors, cosT)
	sinT, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, sin)
	if err != nil {
		return 0, err
	}
	tensors = append(tensors, sinT)
	maskT, err := ort.NewTensor(ort.Shape{1, 1, int64(seq), int64(seq)}, causal)
	if err != nil {
		return 0, err
	}
	tensors = append(tensors, maskT)
	defer func() {
		hT.Destroy()
		cosT.Destroy()
		sinT.Destroy()
		maskT.Destroy()
	}()

	outs := make([]ort.Value, 1)
	if err := g.seqHead.Run(tensors, outs); err != nil {
		return 0, fmt.Errorf("qwen3vlgen: run: %w", err)
	}
	defer outs[0].Destroy()
	lg, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return 0, fmt.Errorf("qwen3vlgen: logits type %T", outs[0])
	}
	data := lg.GetData()
	shape := lg.GetShape()
	if len(shape) != 3 || shape[0] != 1 || shape[1] != int64(seq) {
		return 0, fmt.Errorf("qwen3vlgen: logits shape %v（期望 [1 %d vocab]）", shape, seq)
	}
	vocab := int(shape[2])
	// argmax over last position
	last := data[(seq-1)*vocab : seq*vocab]
	best, bestVal := 0, float32(math.Inf(-1))
	for i, v := range last {
		if v > bestVal {
			best, bestVal = i, v
		}
	}
	return best, nil
}

func (g *Generator) runTokenEmbedding(ids []int) ([]float32, error) {
	in := make([]int64, len(ids))
	for i, id := range ids {
		in[i] = int64(id)
	}
	t, err := ort.NewTensor(ort.Shape{1, int64(len(ids))}, in)
	if err != nil {
		return nil, err
	}
	defer t.Destroy()
	outs := make([]ort.Value, 1)
	if err := g.token.Run([]ort.Value{t}, outs); err != nil {
		return nil, fmt.Errorf("qwen3vlgen: token run: %w", err)
	}
	defer outs[0].Destroy()
	tensor, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("qwen3vlgen: token output %T", outs[0])
	}
	return append([]float32(nil), tensor.GetData()...), nil
}

// decodeToken 判定是否为停止 token，并返回其文本。
func (g *Generator) decodeToken(id int) (string, bool) {
	for _, stopID := range []int{151645, 151643} { // <|im_end|>, <|endoftext|>
		if id == stopID {
			return "", true
		}
	}
	return g.tok.DecodeOne(id), false
}

func (g *Generator) Info() generation.Info {
	return generation.Info{
		Model:              "qwen3vl/" + filepath.Base(g.dir),
		SupportsJSONSchema: false, // 图只到 logits；schema 约束未实现
	}
}

// Close 释放两张图的 session。close sync.Once 保证重复调用安全。
func (g *Generator) Close() {
	g.close.Do(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.seqHead != nil {
			g.seqHead.Destroy()
		}
		if g.token != nil {
			g.token.Destroy()
		}
	})
}

const (
	qwenRotaryHalfDim = 64
	qwenRotaryDim     = 128
)

func rotary(theta float64, section []int, seq int) ([]float32, []float32) {
	cos := make([]float32, seq*qwenRotaryDim)
	sin := make([]float32, seq*qwenRotaryDim)
	inv := make([]float64, qwenRotaryHalfDim)
	for i := range inv {
		inv[i] = 1 / math.Pow(theta, float64(2*i)/qwenRotaryDim)
	}
	for token := 0; token < seq; token++ {
		freq := make([]float64, qwenRotaryHalfDim)
		for i := range freq {
			freq[i] = float64(token) * inv[i]
		}
		// M-RoPE：三个分段共用同一 base 频率，替换各自区间。
		// 纯文本输入下三段位置相同（文本视觉混合才不同），但仍按
		// embedder.go 的同一算法算，保持与向量侧逐位一致。
		for _, dim := range []int{1, 2} {
			if dim >= len(section) {
				continue
			}
			limit := section[dim] * 3
			for i := dim; i < limit && i < qwenRotaryHalfDim; i += 3 {
				freq[i] = float64(token) * inv[i]
			}
		}
		for i, f := range freq {
			c, s := float32(math.Cos(f)), float32(math.Sin(f))
			cos[token*qwenRotaryDim+i] = c
			cos[token*qwenRotaryDim+qwenRotaryHalfDim+i] = c
			sin[token*qwenRotaryDim+i] = s
			sin[token*qwenRotaryDim+qwenRotaryHalfDim+i] = s
		}
	}
	return cos, sin
}

func causalMask(seq int) []float32 {
	m := make([]float32, seq*seq)
	min := float32(math.Inf(-1))
	for i := 0; i < seq; i++ {
		for j := 0; j < seq; j++ {
			if j > i {
				m[i*seq+j] = min
			}
		}
	}
	return m
}

func fingerprint(dir string) string {
	// 指纹只用 embed_config.json：它声明了 arch/dim/generation，
	// 足以区分「同一份权重能否跨版本互比向量」。
	p := filepath.Join(dir, "embed_config.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return "unreadable"
	}
	var h uint64 = 14695981039346656037
	for _, b := range data {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%016x", h)
}
