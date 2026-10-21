//go:build onnxruntime

package chineseclip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

func init() {
	embedding.Register("chineseclip", func(cfg embedding.Config) (embedding.Provider, error) {
		return New(cfg.Options["model_dir"])
	})
}

// Embedder 是 Chinese-CLIP ViT-B/16 的进程内 provider。
//
// 并发：ONNX Runtime 的会话不保证多次 Run 可并发，故运行时用 runMu 串行化；
// 创建/销毁会话用 mu 保护。SPI 要求实现可安全并发调用，这里由我们自己保证。
type Embedder struct {
	mu    sync.RWMutex
	runMu sync.Mutex

	dir    string
	config embedConfig
	tok    *Tokenizer

	text   *ort.DynamicAdvancedSession
	vision *ort.DynamicAdvancedSession

	fp        string
	closeOnce sync.Once
}

// findModelDir 解析产物目录：配置值缺失/不存在时按候选链回退，
// 都不命中时返回空串与**可执行**的指引（错误信息要能直接照着做，
// 而不是一句「读取失败」让用户自己去猜模型从哪来）。
//
// 背景教训（2026-09-30 跑分实测）：全新数据目录启动时模型缺失，
// 内核只打一行 warning 就静默禁用稠密检索，照常服务 —— 用户不知道
// 自己少了什么，跑分台跑了 50 分钟残废配置才发现。
func findModelDir(configured string) (string, string) {
	var candidates []string
	if configured = strings.TrimSpace(configured); configured != "" {
		candidates = append(candidates, configured)
	}
	candidates = append(candidates,
		"/usr/lib/homeagent/models/chinese-clip-vit-b16-onnx", // 发行包安装位置
	)
	for _, p := range candidates {
		if _, err := os.Stat(filepath.Join(p, "embed_config.json")); err == nil {
			return p, ""
		}
	}
	hint := "chineseclip: 产物目录不可用（依次尝试: " + strings.Join(candidates, ", ") + "）。获得模型任选其一:\n" +
		"  ① 运行 scripts/export_chineseclip_onnx.py 导出，产物放到上述任一路径\n" +
		"  ② 从已有实例的 <dataDir>/models/chinese-clip-vit-b16-onnx 拷贝或 symlink\n" +
		"  ③ 安装发行包（自带 /usr/lib/homeagent/models/）"
	return "", hint
}

// New 从产物目录构造 provider。
func New(modelDir string) (*Embedder, error) {
	resolved, hint := findModelDir(modelDir)
	if resolved == "" {
		return nil, fmt.Errorf("%s", hint)
	}
	modelDir = resolved
	cfg, err := loadConfig(modelDir)
	if err != nil {
		return nil, err
	}
	tok, err := LoadTokenizer(modelDir, cfg.MaxLength)
	if err != nil {
		return nil, err
	}
	if !ort.IsInitialized() {
		if lib := findOnnxLib(); lib != "" {
			ort.SetSharedLibraryPath(lib)
		}
		if err := ort.InitializeEnvironment(); err != nil {
			return nil, fmt.Errorf("chineseclip: 初始化 onnx 环境: %w", err)
		}
	}

	text, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, cfg.TextONNX),
		[]string{"input_ids", "attention_mask"},
		[]string{"text_features"}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 创建文本塔会话（%s）: %w", cfg.TextONNX, err)
	}
	vision, err := ort.NewDynamicAdvancedSession(
		filepath.Join(modelDir, cfg.VisionONNX),
		[]string{"pixel_values"},
		[]string{"image_features"}, nil,
	)
	if err != nil {
		text.Destroy()
		return nil, fmt.Errorf("chineseclip: 创建视觉塔会话（%s）: %w", cfg.VisionONNX, err)
	}

	return &Embedder{
		dir:    modelDir,
		config: cfg,
		tok:    tok,
		text:   text,
		vision: vision,
		fp:     computeFingerprint(modelDir, cfg),
	}, nil
}

// Embed 按模态分派。audio/video 一律返回 ErrUnsupportedModality——
// 本空间没有它们的原生编码器，用别的模型向量冒充会污染整个向量空间。
func (e *Embedder) Embed(ctx context.Context, in embedding.Input) ([]float64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch in.Modality {
	case embedding.ModalityText:
		if strings.TrimSpace(in.Text) == "" {
			return nil, fmt.Errorf("chineseclip: 文本输入为空")
		}
		return e.embedText(in.Text)
	case embedding.ModalityImage:
		if len(in.Data) == 0 {
			return nil, fmt.Errorf("chineseclip: 图像输入为空（modality=image 需要 Data）")
		}
		return e.embedImage(in.Data)
	default:
		return nil, fmt.Errorf("chineseclip: %w: %s", embedding.ErrUnsupportedModality, in.Modality)
	}
}

func (e *Embedder) embedText(text string) ([]float64, error) {
	e.mu.RLock()
	sess, tok, dim := e.text, e.tok, e.config.Dimension
	e.mu.RUnlock()
	if sess == nil {
		return nil, fmt.Errorf("chineseclip: provider 已关闭")
	}

	ids, mask := tok.Encode(text)
	shape := ort.Shape{1, int64(len(ids))}
	idTensor, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 构造 input_ids 张量: %w", err)
	}
	defer idTensor.Destroy()
	maskTensor, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 构造 attention_mask 张量: %w", err)
	}
	defer maskTensor.Destroy()

	outs := make([]ort.Value, 1)
	e.runMu.Lock()
	err = sess.Run([]ort.Value{idTensor, maskTensor}, outs)
	e.runMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 文本塔推理: %w", err)
	}
	if outs[0] == nil {
		return nil, fmt.Errorf("chineseclip: 文本塔输出为空")
	}
	defer outs[0].Destroy()
	return normalizeOutput(outs[0], 1, dim, "text_features")
}

func (e *Embedder) embedImage(data []byte) ([]float64, error) {
	e.mu.RLock()
	sess, cfg, dim := e.vision, e.config, e.config.Dimension
	e.mu.RUnlock()
	if sess == nil {
		return nil, fmt.Errorf("chineseclip: provider 已关闭")
	}

	pixels, err := preprocessImage(data, cfg.ImageSize, cfg.ImageMean, cfg.ImageStd)
	if err != nil {
		return nil, err
	}
	shape := ort.Shape{1, 3, int64(cfg.ImageSize), int64(cfg.ImageSize)}
	in, err := ort.NewTensor(shape, pixels)
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 构造 pixel_values 张量: %w", err)
	}
	defer in.Destroy()

	outs := make([]ort.Value, 1)
	e.runMu.Lock()
	err = sess.Run([]ort.Value{in}, outs)
	e.runMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 视觉塔推理: %w", err)
	}
	if outs[0] == nil {
		return nil, fmt.Errorf("chineseclip: 视觉塔输出为空")
	}
	defer outs[0].Destroy()
	return normalizeOutput(outs[0], 1, dim, "image_features")
}

// normalizeOutput 取出 [batch, dim] 输出并做 L2 归一化。
//
// 官方 Chinese-CLIP 的检索用法就是余弦相似度（归一化后点积），
// 归档前统一归一化可以避免下游反复判断。
func normalizeOutput(value ort.Value, batch, dim int, name string) ([]float64, error) {
	tensor, ok := value.(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("chineseclip: %s 输出类型 %T，期望 float32 张量", name, value)
	}
	shape := tensor.GetShape()
	if len(shape) != 2 || shape[0] != int64(batch) || shape[1] != int64(dim) {
		return nil, fmt.Errorf("chineseclip: %s 形状 %v，期望 [%d %d]", name, shape, batch, dim)
	}
	raw := tensor.GetData()
	if len(raw) < batch*dim {
		return nil, fmt.Errorf("chineseclip: %s 数据长度 %d，期望 %d", name, len(raw), batch*dim)
	}
	out := make([]float64, dim)
	var norm float64
	for i := 0; i < dim; i++ {
		v := float64(raw[i])
		out[i] = v
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, fmt.Errorf("chineseclip: %s 向量范数为 %v（模型输出异常）", name, norm)
	}
	for i := range out {
		out[i] /= norm
	}
	return out, nil
}

// Info 只声明本 provider 能通过公共契约提供的模态。
//
// 契约要求 provider 自行解码 Data；这里没有视频/音频解码器，列进来只会让核心
// 据以创建输入、然后在运行时全部失败。audio/video 必须返回 ErrUnsupportedModality。
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
	e.closeOnce.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.text != nil {
			e.text.Destroy()
			e.text = nil
		}
		if e.vision != nil {
			e.vision.Destroy()
			e.vision = nil
		}
	})
}

// computeFingerprint 覆盖**全部**影响向量语义的产物：两个 ONNX 图、词表与配置。
// 漏掉任何一个都会让「换了模型但指纹没变」，历史向量不会重算。
func computeFingerprint(modelDir string, cfg embedConfig) string {
	h := sha256.New()
	for _, name := range []string{cfg.TextONNX, cfg.VisionONNX, "vocab.txt", "embed_config.json"} {
		data, err := os.ReadFile(filepath.Join(modelDir, name))
		if err != nil {
			// 读不到就写名字+错误，绝不跳过：跳过等于指纹对缺件不敏感。
			fmt.Fprintf(h, "%s:MISSING:%v\n", name, err)
			continue
		}
		fmt.Fprintf(h, "%s:%d\n", name, len(data))
		h.Write(data)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func findOnnxLib() string {
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
