//go:build onnxruntime

// Package qwen3vl provides the optional Qwen3-VL-Embedding ONNX provider.
// Model-specific tokenization, preprocessing, graph layout, and runtime code
// live here rather than in the HologramAgent core.
package qwen3vl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

func init() {
	embedding.Register("qwen3vl", func(cfg embedding.Config) (embedding.Provider, error) {
		modelDir := cfg.Options["model_dir"]
		e, err := New(modelDir)
		if err != nil {
			return nil, err
		}
		return e, nil
	})
}

type embedConfig struct {
	Arch          string    `json:"arch"`
	Dimension     int       `json:"dim"`
	MaxLength     int       `json:"max_length"`
	Instruction   string    `json:"instruction"`
	Pooling       string    `json:"pooling"`
	ImageSize     int       `json:"image_size"`
	PatchSize     int       `json:"patch_size"`
	TemporalPatch int       `json:"temporal_patch_size"`
	SpatialMerge  int       `json:"spatial_merge_size"`
	ImageMean     []float64 `json:"image_mean"`
	ImageStd      []float64 `json:"image_std"`
	RopeTheta     float64   `json:"rope_theta"`
	MRopeSection  []int     `json:"mrope_section"`
}

type Embedder struct {
	mu sync.RWMutex

	loaded    bool
	dir       string
	config    embedConfig
	tok       *Tokenizer
	token     *ort.DynamicAdvancedSession
	transform *ort.DynamicAdvancedSession

	// vision 按时间组数缓存视觉图会话：1 = 单图（Vision.onnx），
	// G>1 = 视频（Vision_g{G}.onnx）。每张图约 1.6GB，因此按需加载而不是
	// 启动时全开；未用到的档位不占内存。
	visionMu sync.Mutex
	vision   map[int]*ort.DynamicAdvancedSession

	fp    string
	close sync.Once
}

func New(modelDir string) (*Embedder, error) {
	if modelDir == "" {
		return nil, fmt.Errorf("qwen model dir not specified")
	}
	cfgRaw, err := os.ReadFile(filepath.Join(modelDir, "embed_config.json"))
	if err != nil {
		return nil, fmt.Errorf("read embed_config.json: %w", err)
	}
	var cfg embedConfig
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		return nil, fmt.Errorf("parse embed_config.json: %w", err)
	}
	if cfg.Dimension != 2048 || cfg.MaxLength < 598 || cfg.Pooling != "last_token" {
		return nil, fmt.Errorf("qwen: incompatible config dim=%d max_length=%d pooling=%q", cfg.Dimension, cfg.MaxLength, cfg.Pooling)
	}
	if cfg.ImageSize != qwenImageSize || cfg.PatchSize != qwenPatchSize || cfg.TemporalPatch != qwenTemporalPatch || cfg.SpatialMerge != qwenSpatialMerge {
		return nil, fmt.Errorf("qwen: incompatible vision layout image=%d patch=%d temporal=%d merge=%d", cfg.ImageSize, cfg.PatchSize, cfg.TemporalPatch, cfg.SpatialMerge)
	}
	if cfg.RopeTheta <= 0 || len(cfg.MRopeSection) != 3 || cfg.MRopeSection[0]+cfg.MRopeSection[1]+cfg.MRopeSection[2] != qwenRotaryHalfDim {
		return nil, fmt.Errorf("qwen: incompatible rope theta=%g section=%v", cfg.RopeTheta, cfg.MRopeSection)
	}

	tok, err := LoadTokenizer(modelDir)
	if err != nil {
		return nil, err
	}
	if !ort.IsInitialized() {
		if lib := FindOnnxLib(); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("init onnx env: %w", err)
		}
	}

	token, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "TokenEmbedding.onnx"),
		[]string{"input_ids"}, []string{"hidden"}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create qwen token embedding session: %w", err)
	}
	transform, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "Transformer.onnx"),
		[]string{"hidden", "deepstack_0", "deepstack_1", "deepstack_2", "rotary_cos", "rotary_sin", "causal_mask"},
		[]string{"embedding"}, nil,
	)
	if err != nil {
		token.Destroy()
		return nil, fmt.Errorf("create qwen transformer session: %w", err)
	}
	vision, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, "Vision.onnx"), []string{"pixel_values"},
		[]string{"deepstack_feature_0", "deepstack_feature_1", "deepstack_feature_2", "vision_hidden_states"}, nil,
	)
	if err != nil {
		token.Destroy()
		transform.Destroy()
		return nil, fmt.Errorf("create qwen vision session: %w", err)
	}

	return &Embedder{
		loaded: true, dir: modelDir, config: cfg, tok: tok,
		token: token, transform: transform,
		vision: map[int]*ort.DynamicAdvancedSession{1: vision},
		fp:     computeFingerprint(modelDir),
	}, nil
}

func (e *Embedder) VectorizeDense(text string) ([]float64, error) {
	e.mu.RLock()
	loaded, cfg, tok := e.loaded, e.config, e.tok
	e.mu.RUnlock()
	if !loaded {
		return nil, fmt.Errorf("qwen embedder not loaded")
	}
	ids, _, position, _, err := tok.textModelInput(cfg.Instruction, text, cfg.MaxLength)
	if err != nil {
		return nil, err
	}
	hidden, err := e.runTokenEmbedding(ids)
	if err != nil {
		return nil, err
	}
	deep := make([][]float32, 3)
	for i := range deep {
		deep[i] = make([]float32, len(hidden))
	}
	return e.runTransformer(hidden, deep, position, len(ids))
}

// EmbedImageDense 把一张图片编码到统一空间。
//
// mime 决定这个模态是否在本空间的原生覆盖范围内：Qwen3-VL 能原生编码文本、
// 图像与视频，但**不原生支持音频**（模型卡与 config 双重确认：没有
// audio_token_id / audio_config）。音频必须返回 ErrModalityUnsupported，
// 而不是拿视觉塔去编码——那会往统一空间里灌入语义错误的坐标，而错误是静默的。
//
// video/*（视频文件）也在这里拒绝：本函数的入参是**单帧字节**，Go 侧没有
// 视频解码器；多帧请走 EmbedVideoDense。
func (e *Embedder) EmbedImageDense(raw []byte, mime string) ([]float64, error) {
	if err := checkImageMime(mime); err != nil {
		return nil, err
	}
	pixels, err := preprocessImage(raw)
	if err != nil {
		return nil, err
	}
	return e.embedVision(pixels, 1)
}

// EmbedVideoDense 把已按时间排序的视频帧编码到统一空间（原生跨帧时序）。
//
// frames 是已解码的帧（PNG/JPEG 字节），相邻两帧构成一个时间组；
// groups = len(frames)/2 必须恰好是导出时固定的某一档（Vision_g{G}.onnx），
// 否则本方法明确报错并告知已导出哪些档位。
//
// 为何必须精确匹配而不能“差不多就行”：视觉塔的注意力按 grid 划分，
// 用 G=2 的图喂 G=3 的数据是未定义行为。实测（onnxruntime）会因维度不符
// 报 InvalidArgument，因此不会静默算错——但也不该依赖那次报错来兜底。
//
// 帧数为奇数时只用得上前 2×floor(n/2) 帧，多余一帧被丢弃（不补重复帧：
// 那会改变跨帧注意力看到的运动）。
func (e *Embedder) EmbedVideoDense(frames [][]byte, mime string) ([]float64, error) {
	// 暂时硬失败，而不是算出一个「看起来正常但语义错」的向量。
	//
	// 原因：本 provider 的 Go 侧视频提示词模板与 HuggingFace processor 不等价
	// ——processor 会按时间组插入字面时间戳文本
	//（`<0.0 seconds>` / `<1.0 seconds>`），而 Go 侧只拼
	// `<|vision_start|>{G×576 pads}<|vision_end|>`。实测同一输入 Python seq=1190
	//（1152 视觉 + 38 文本）、Go 只有 22 个文本 token；而这些时间戳文本也会
	// 占用 M-RoPE 位置。
	//
	// 差异的后果是向量整体偏移，**不报错**：检索出来的相似度没有任何意义，
	// 而且看不出是谁的错。视觉图本身（Vision_g2/g3/g4）已逐档对过 PyTorch
	//（cos≥0.999999），差的只是模板。
	//
	// 修复后删掉这个硬失败，并让 TestEmbedderVideoMatchesONNXReference 不再跳过。
	return nil, fmt.Errorf("%w: Qwen3-VL 视频路径尚未完成（Go 模板缺 processor 的分组时间戳文本，"+
		"会静默产生语义错的向量）；详见 docs/zh/multimodal-space.md 与 providers/qwen3vl/embedder_onnx_test.go",
		embedding.ErrUnsupportedModality)
}

// checkImageMime 把「不在本空间覆盖范围内」与「参数用错」分开报。
func checkImageMime(mime string) error {
	switch {
	case strings.HasPrefix(mime, "audio/"):
		return fmt.Errorf("%w: audio (%s) 不在 Qwen3-VL 原生模态内（无 audio_token_id），需真正的统一音频模型",
			embedding.ErrUnsupportedModality, mime)
	case strings.HasPrefix(mime, "video/"):
		return fmt.Errorf("%w: 视频文件 (%s) 无法解码；多帧请用 EmbedVideoDense",
			embedding.ErrUnsupportedModality, mime)
	}
	return nil
}

func checkVideoMime(mime string) error {
	if strings.HasPrefix(mime, "audio/") {
		return fmt.Errorf("%w: audio (%s)", embedding.ErrUnsupportedModality, mime)
	}
	return nil
}

// visionFor 返回指定时间组数的视觉图会话，按需创建。
func (e *Embedder) visionFor(groups int) (*ort.DynamicAdvancedSession, error) {
	e.visionMu.Lock()
	defer e.visionMu.Unlock()
	if s, ok := e.vision[groups]; ok {
		return s, nil
	}
	name := "Vision.onnx"
	if groups > 1 {
		name = fmt.Sprintf("Vision_g%d.onnx", groups)
	}
	path := filepath.Join(e.dir, name)
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("qwen: 缺少 %s（时间组数 %d 未导出，已加载档位 %v）；"+
			"用 scripts/export_qwen3vl_embedding_onnx.py 按所需 --video-groups 重新导出: %w",
			name, groups, e.visionGroupsLocked(), err)
	}
	s, err := ort.NewDynamicAdvancedSession(path, []string{"pixel_values"},
		[]string{"deepstack_feature_0", "deepstack_feature_1", "deepstack_feature_2", "vision_hidden_states"}, nil)
	if err != nil {
		return nil, fmt.Errorf("create qwen vision session %s: %w", name, err)
	}
	e.vision[groups] = s
	return s, nil
}

// visionGroupsLocked 返回已加载的档位（调用方需持 visionMu）。
func (e *Embedder) visionGroupsLocked() []int {
	out := make([]int, 0, len(e.vision))
	for g := range e.vision {
		out = append(out, g)
	}
	sort.Ints(out)
	return out
}

// embedVision 是图像与视频共用的后半段：视觉塔 → 散射到 hidden → Transformer。
func (e *Embedder) embedVision(pixels []float32, groups int) ([]float64, error) {
	e.mu.RLock()
	loaded, cfg, tok := e.loaded, e.config, e.tok
	e.mu.RUnlock()
	if !loaded {
		return nil, fmt.Errorf("qwen embedder not loaded")
	}

	vision, err := e.visionFor(groups)
	if err != nil {
		return nil, err
	}
	wantTokens := groups * qwenVisualTokens
	features, err := e.runVision(vision, pixels, wantTokens, cfg.Dimension)
	if err != nil {
		return nil, err
	}

	var ids []int
	var position []int64
	var visual []bool
	if groups == 1 {
		ids, _, position, visual, err = tok.imageModelInput(cfg.Instruction, cfg.MaxLength)
	} else {
		ids, _, position, visual, err = tok.videoModelInput(cfg.Instruction, groups, cfg.MaxLength)
	}
	if err != nil {
		return nil, err
	}

	hidden, err := e.runTokenEmbedding(ids)
	if err != nil {
		return nil, err
	}
	deep := make([][]float32, 3)
	for i := range deep {
		deep[i] = make([]float32, len(hidden))
	}
	// 视觉特征按占位符出现顺序就地替换 token embedding：
	// 图像一个组、视频 G 个组，展开后长度都是 groups×576，与视觉塔输出一致。
	visualIndex := 0
	for tokenIndex, isVisual := range visual {
		if !isVisual {
			continue
		}
		dst := tokenIndex * cfg.Dimension
		src := visualIndex * cfg.Dimension
		copy(hidden[dst:dst+cfg.Dimension], features[3][src:src+cfg.Dimension])
		for layer := range deep {
			copy(deep[layer][dst:dst+cfg.Dimension], features[layer][src:src+cfg.Dimension])
		}
		visualIndex++
	}
	if visualIndex != wantTokens {
		return nil, fmt.Errorf("qwen: injected visual tokens=%d, want %d (groups=%d)", visualIndex, wantTokens, groups)
	}
	return e.runTransformer(hidden, deep, position, len(ids))
}

func (e *Embedder) runTokenEmbedding(ids []int) ([]float32, error) {
	inputIDs := make([]int64, len(ids))
	for i, id := range ids {
		inputIDs[i] = int64(id)
	}
	in, err := ort.NewTensor(ort.Shape{1, int64(len(ids))}, inputIDs)
	if err != nil {
		return nil, fmt.Errorf("qwen token input: %w", err)
	}
	defer in.Destroy()
	outs := make([]ort.Value, 1)
	if err := e.token.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("qwen token embedding run: %w", err)
	}
	if outs[0] == nil {
		return nil, fmt.Errorf("qwen token embedding output is nil")
	}
	defer outs[0].Destroy()
	tensor, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("qwen token embedding output type %T", outs[0])
	}
	shape := tensor.GetShape()
	if len(shape) != 3 || shape[0] != 1 || shape[1] != int64(len(ids)) || shape[2] != int64(e.config.Dimension) {
		return nil, fmt.Errorf("qwen token embedding shape=%v", shape)
	}
	return append([]float32(nil), tensor.GetData()...), nil
}

func (e *Embedder) runVision(sess *ort.DynamicAdvancedSession, pixels []float32, wantTokens, dim int) ([][]float32, error) {
	wantPatches := int64(wantTokens) * qwenSpatialMerge * qwenSpatialMerge
	if int64(len(pixels)) != wantPatches*qwenPatchVectorSize {
		return nil, fmt.Errorf("qwen vision input: %d floats, want %d patches × %d",
			len(pixels), wantPatches, qwenPatchVectorSize)
	}
	in, err := ort.NewTensor(ort.Shape{wantPatches, qwenPatchVectorSize}, pixels)
	if err != nil {
		return nil, fmt.Errorf("qwen vision input: %w", err)
	}
	defer in.Destroy()
	outs := make([]ort.Value, 4)
	if err := sess.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("qwen vision run: %w", err)
	}
	features := make([][]float32, 4)
	for i, value := range outs {
		if value == nil {
			return nil, fmt.Errorf("qwen vision output %d is nil", i)
		}
		defer value.Destroy()
		tensor, ok := value.(*ort.Tensor[float32])
		if !ok {
			return nil, fmt.Errorf("qwen vision output %d type %T", i, value)
		}
		shape := tensor.GetShape()
		if len(shape) != 2 || shape[0] != int64(wantTokens) || shape[1] != int64(dim) {
			return nil, fmt.Errorf("qwen vision output %d shape=%v, want [%d %d]", i, shape, wantTokens, dim)
		}
		features[i] = append([]float32(nil), tensor.GetData()...)
	}
	return features, nil
}

func (e *Embedder) runTransformer(hidden []float32, deep [][]float32, position []int64, seq int) ([]float64, error) {
	if len(hidden) != seq*e.config.Dimension || len(deep) != 3 || len(position) != 3*seq {
		return nil, fmt.Errorf("qwen: invalid transformer inputs hidden=%d deep=%d position=%d seq=%d", len(hidden), len(deep), len(position), seq)
	}
	cos, sin := e.rotary(position, seq)
	causal := causalMask(seq)

	hiddenTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(e.config.Dimension)}, hidden)
	if err != nil {
		return nil, fmt.Errorf("qwen hidden tensor: %w", err)
	}
	defer hiddenTensor.Destroy()
	inputs := []ort.Value{hiddenTensor}
	var deepTensors []*ort.Tensor[float32]
	for i, data := range deep {
		if len(data) != len(hidden) {
			return nil, fmt.Errorf("qwen deepstack %d length=%d, want %d", i, len(data), len(hidden))
		}
		t, err := ort.NewTensor(ort.Shape{1, int64(seq), int64(e.config.Dimension)}, data)
		if err != nil {
			return nil, fmt.Errorf("qwen deepstack %d tensor: %w", i, err)
		}
		deepTensors = append(deepTensors, t)
		inputs = append(inputs, t)
	}
	defer func() {
		for _, t := range deepTensors {
			t.Destroy()
		}
	}()
	cosTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, cos)
	if err != nil {
		return nil, fmt.Errorf("qwen rotary cos: %w", err)
	}
	defer cosTensor.Destroy()
	sinTensor, err := ort.NewTensor(ort.Shape{1, int64(seq), qwenRotaryDim}, sin)
	if err != nil {
		return nil, fmt.Errorf("qwen rotary sin: %w", err)
	}
	defer sinTensor.Destroy()
	causalTensor, err := ort.NewTensor(ort.Shape{1, 1, int64(seq), int64(seq)}, causal)
	if err != nil {
		return nil, fmt.Errorf("qwen causal mask: %w", err)
	}
	defer causalTensor.Destroy()
	inputs = append(inputs, cosTensor, sinTensor, causalTensor)

	out, err := ort.NewEmptyTensor[float32](ort.Shape{1, int64(e.config.Dimension)})
	if err != nil {
		return nil, fmt.Errorf("qwen output tensor: %w", err)
	}
	defer out.Destroy()
	if err := e.transform.Run(inputs, []ort.Value{out}); err != nil {
		return nil, fmt.Errorf("qwen transformer run: %w", err)
	}
	return normalize(out.GetData()), nil
}

const (
	qwenRotaryHalfDim = 64
	qwenRotaryDim     = 128
)

func (e *Embedder) rotary(position []int64, seq int) ([]float32, []float32) {
	cos := make([]float32, seq*qwenRotaryDim)
	sin := make([]float32, seq*qwenRotaryDim)
	inv := make([]float64, qwenRotaryHalfDim)
	for i := range inv {
		inv[i] = 1 / math.Pow(e.config.RopeTheta, float64(2*i)/qwenRotaryDim)
	}
	for token := 0; token < seq; token++ {
		freq := make([]float64, qwenRotaryHalfDim)
		for i := range freq {
			freq[i] = float64(position[token]) * inv[i]
		}
		for dim, offset := range []int{0, 1, 2} {
			if dim == 0 {
				continue
			}
			limit := e.config.MRopeSection[dim] * 3
			for i := offset; i < limit; i += 3 {
				freq[i] = float64(position[dim*seq+token]) * inv[i]
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
	out := make([]float32, seq*seq)
	for row := 0; row < seq; row++ {
		for col := row + 1; col < seq; col++ {
			out[row*seq+col] = -math.MaxFloat32
		}
	}
	return out
}

func normalize(raw []float32) []float64 {
	out := make([]float64, len(raw))
	var norm float64
	for i, v := range raw {
		out[i] = float64(v)
		norm += out[i] * out[i]
	}
	if norm > 0 {
		norm = math.Sqrt(norm)
		for i := range out {
			out[i] /= norm
		}
	}
	return out
}

func (e *Embedder) Embed(_ context.Context, in embedding.Input) ([]float64, error) {
	switch in.Modality {
	case embedding.ModalityText:
		return e.VectorizeDense(in.Text)
	case embedding.ModalityImage:
		return e.EmbedImageDense(in.Data, in.MIME)
	case embedding.ModalityVideo:
		// 公共契约把视频交给 provider 自行解码，而本 provider 没有视频解码器
		// （Go 标准库不含 H.264/MP4）。这里必须明确说「本 provider 不提供视频
		// 文件编码」，而不是假装支持后再拿错数据算出一个语义错误的向量。
		//
		// 可用的视频路径是本 provider 自己的 EmbedVideoDense：调用方先抽帧，
		// 由本 provider 按自己的时序窗口分组。
		return nil, fmt.Errorf("%w: video（本 provider 不内嵌视频解码器；"+
			"请先抽帧并调用 qwen3vl 的 EmbedVideoDense）", embedding.ErrUnsupportedModality)
	default:
		return nil, fmt.Errorf("%w: %s", embedding.ErrUnsupportedModality, in.Modality)
	}
}

// Info 只声明本 provider 能通过**公共契约**提供的模态。
//
// 视频不在其中：契约要求 provider 自行解码 Data，而本 provider 没有视频
// 解码器；列进来会让核心据以创建 video 输入，然后在运行时全部失败。
// 视频能力由本 provider 自己的 EmbedVideoDense（接收已解码帧）提供，
// 待核心有了对 provider 不透明的多帧容器后再纳入契约。
func (e *Embedder) Info() embedding.Info {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return embedding.Info{
		Dimension:   e.config.Dimension,
		Fingerprint: e.fp,
		Modalities: []embedding.Modality{
			embedding.ModalityText,
			embedding.ModalityImage,
		},
	}
}
func (e *Embedder) Close() {
	e.close.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.token != nil {
			e.token.Destroy()
			e.token = nil
		}
		if e.transform != nil {
			e.transform.Destroy()
			e.transform = nil
		}
		e.visionMu.Lock()
		for groups, sess := range e.vision {
			if sess != nil {
				sess.Destroy()
			}
			delete(e.vision, groups)
		}
		e.visionMu.Unlock()
		e.loaded = false
	})
}

func computeFingerprint(modelDir string) string {
	h := sha256.New()
	graphNames := []string{"TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx", "embed_config.json"}
	// 视频是按时间组数各导一张图，因此每一张都必须进入指纹：
	// 漏掉它们会让「换了视频图但指纹没变」，历史向量不会重算。
	if entries, err := os.ReadDir(modelDir); err == nil {
		var extra []string
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "Vision_g") && strings.HasSuffix(entry.Name(), ".onnx") {
				extra = append(extra, entry.Name())
			}
		}
		sort.Strings(extra)
		graphNames = append(graphNames, extra...)
	}
	for _, name := range graphNames {
		if data, err := os.ReadFile(filepath.Join(modelDir, name)); err == nil {
			h.Write([]byte(name))
			h.Write([]byte{0})
			h.Write(data)
			h.Write([]byte{0})
		}
	}
	entries, _ := os.ReadDir(modelDir)
	var names []string
	for _, entry := range entries {
		n := entry.Name()
		if strings.HasPrefix(n, "embed_tokens.") || strings.HasPrefix(n, "layers.") || strings.HasPrefix(n, "onnx__") || strings.HasSuffix(n, ".onnx.data") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if info, err := os.Stat(filepath.Join(modelDir, n)); err == nil {
			fmt.Fprintf(h, "%s:%d\n", n, info.Size())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// FindOnnxLib 返回本机 onnxruntime 共享库路径，找不到时返回空串。
//
// 导出给同模型族的其它 provider 用（如 providers/qwen3vlgen）：
// 库路径的候选清单（含发行包内置位置与两个历史环境变量）必须**只有一份** ——
// 各写一份的话，新增安装位置时必然漏掉某一个，而漏掉的表现是
// 「初始化失败」这种看不出原因的错误。
func FindOnnxLib() string {
	var candidates []string
	for _, env := range []string{"ONNXRUNTIME_DIR", "ONNX_ML_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(env)); dir != "" {
			candidates = append(candidates, filepath.Join(dir, "libonnxruntime.so"))
		}
	}
	candidates = append(candidates,
		"/usr/lib/homeagent/onnxruntime/libonnxruntime.so", // 发行包内置位置
		"/opt/onnxruntime/libonnxruntime.so",
		"/usr/local/lib/libonnxruntime.so",
		"/usr/lib/libonnxruntime.so",
	)
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
