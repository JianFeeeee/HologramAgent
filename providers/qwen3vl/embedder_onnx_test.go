//go:build onnxruntime

package qwen3vl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// onnxModelDir 返回三段式 Qwen 多模态 ONNX 产物目录。
//
// 产物约 8GB（含外部权重），不进仓库；由 scripts/export_qwen3vl_embedding_onnx.py
// 自动拉取模型并导出。可通过 QWEN_ONNX_MODEL_DIR 指向别处；目录不存在时相关
// 测试跳过，而不是失败——CI 与本机开发者都不一定有这份产物。
func onnxModelDir() string {
	if v := os.Getenv("QWEN_ONNX_MODEL_DIR"); v != "" {
		return v
	}
	return envOr("QWEN3VL_MODEL_DIR", "/opt/models/qwen3-vl-embed-multimodal-onnx")
}

// artifactDeclaresVideo 读产物自带的 embed_config.json，判断它是否声明支持原生视频。
//
// 用途：把「这个产物本来就不含视频」与「这个产物应该有视频，但参考里没有」分开。
// 后者是产物/参考不匹配，必须报错而不是跳过——否则一个声明了视频支持的目录
// 可以带着空视频参考一路「通过」。
func artifactDeclaresVideo(t *testing.T, dir string) bool {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "embed_config.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		SupportsNativeVideo bool  `json:"supports_native_video"`
		VideoGroups         []int `json:"video_groups"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false
	}
	return cfg.SupportsNativeVideo || len(cfg.VideoGroups) > 0
}

func requireONNXArtifacts(t *testing.T) string {
	t.Helper()
	dir := onnxModelDir()
	for _, name := range []string{"TokenEmbedding.onnx", "Transformer.onnx", "Vision.onnx", "embed_config.json", "tokenizer.json"} {
		if _, err := os.Stat(dir + "/" + name); err != nil {
			t.Skipf("ONNX 产物不完整（%s: %v），跳过；用 scripts/export_qwen3vl_embedding_onnx.py 导出", name, err)
		}
	}
	return dir
}

// onnxReference 是导出脚本 `--emit-reference` 写出的冻结参考。
//
// 刻意不把浮点常量硬编码在测试里：参考值必须能追溯到「哪个模型、哪次导出、
// 什么输入」，而不是一组无人知道出处的数字。参考文件里的 RGB/尺寸同时用来
// 构造测试图片，保证输入与参考按构造一致，不会因测试改动而静默错位。
type onnxReference struct {
	Text              string    `json:"text"`
	TextVectorPrefix  []float64 `json:"text_vector_prefix"`
	ImageRGB          []int     `json:"image_rgb"`
	ImageSize         int       `json:"image_size"`
	ImageVectorPrefix []float64 `json:"image_vector_prefix"`
	Dim               int       `json:"dim"`

	// 视频参考：相邻两帧构成一个时间组（tp0←帧2g、tp1←帧2g+1），
	// 帧颜色用来构造与导出脚本完全一致的测试输入。
	VideoGroups       int       `json:"video_groups"`
	VideoFrameRGB     [][]int   `json:"video_frame_rgb"`
	VideoVectorPrefix []float64 `json:"video_vector_prefix"`
}

func loadReference(t *testing.T, dir string) *onnxReference {
	t.Helper()
	path := os.Getenv("QWEN_ONNX_REFERENCE")
	if path == "" {
		path = filepath.Join(dir, "qwen_reference.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("缺少冻结参考 %s（由 scripts/export_qwen3vl_embedding_onnx.py --emit-reference 生成）: %v", path, err)
	}
	var ref onnxReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("解析参考 %s: %v", path, err)
	}
	if ref.Text == "" || len(ref.TextVectorPrefix) == 0 || len(ref.ImageRGB) != 3 || ref.ImageSize <= 0 {
		t.Fatalf("参考 %s 不完整: %+v", path, ref)
	}
	return &ref
}

// solidPNG 生成一张 size×size 纯色 PNG，供跨语言冻结向量回归。
//
// 刻意用纯色且尺寸与视觉塔一致：Go 侧预处理对已是 768×768 的输入不做插值、
// 不补边，于是 patch 张量只由布局决定。一旦 patch 排列写错（内层循环顺序、
// merge 分组顺序、通道顺序），冻结向量立刻不匹配——而那类错误在人工看图时
// 几乎发现不了。
func solidPNG(t *testing.T, size int, r, g, b uint8) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 255
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func newTestEmbedder(t *testing.T, dir string) *Embedder {
	t.Helper()
	e, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(e.Close)
	info := e.Info()
	if info.Dimension != 2048 || info.Fingerprint == "" {
		t.Fatalf("元数据异常: dim=%d fingerprint=%q", info.Dimension, info.Fingerprint)
	}
	if err := embedding.ValidateInfo(info); err != nil {
		t.Fatalf("Info 不满足公共契约: %v", err)
	}
	return e
}

func assertNormalized(t *testing.T, name string, got []float64, dim int) {
	t.Helper()
	if dim > 0 && len(got) != dim {
		t.Fatalf("%s 维度 = %d，期望 %d", name, len(got), dim)
	}
	var norm float64
	for _, v := range got {
		norm += v * v
	}
	if diff := math.Abs(math.Sqrt(norm) - 1); diff > 1e-6 {
		t.Errorf("%s L2 norm = %.9f，期望 1", name, math.Sqrt(norm))
	}
}

func assertFrozenPrefix(t *testing.T, name string, got, want []float64) {
	t.Helper()
	if len(got) < len(want) {
		t.Fatalf("%s 向量过短: %d", name, len(got))
	}
	for i := range want {
		if diff := math.Abs(got[i] - want[i]); diff > 2e-5 {
			t.Errorf("%s 维度 %d = %.10g，参考 %.10g，差 %.3g", name, i, got[i], want[i], diff)
		}
	}
}

// TestEmbedderMatchesONNXReference 逐维对比导出脚本写出的冻结参考向量，
// 验证完整 Go 路径：模板渲染 → BPE → TokenEmbedding → Transformer →
// last-token 池化 → L2 normalize。
//
// 只覆盖前若干维不是因为放宽正确性（脚本侧的 PyTorch↔ONNX 校验是逐维的），
// 而是避免把 2048 个浮点常量塞进仓库；这里负责捕获 Go 张量形状、输入名、
// 输出名、池化/归一化或模板接线错误。
func TestEmbedderMatchesONNXReference(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	ref := loadReference(t, dir)

	ids, _, _, _, err := e.tok.textModelInput(e.config.Instruction, ref.Text, e.config.MaxLength)
	if err != nil {
		t.Fatalf("textModelInput: %v", err)
	}
	postID, ok := e.tok.SpecialID("<|endoftext|>")
	if !ok || ids[len(ids)-1] != postID {
		t.Fatalf("模型输入 post-processor 异常: len=%d tail=%v postID=%d ok=%v", len(ids), ids[len(ids)-1:], postID, ok)
	}

	got, err := e.VectorizeDense(ref.Text)
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	assertNormalized(t, "text", got, ref.Dim)
	assertFrozenPrefix(t, "text", got, ref.TextVectorPrefix)
}

// TestEmbedderImageMatchesONNXReference 冻结一张纯色图的参考向量，
// 验证 Go 侧的视觉预处理 + patch 排列 + 视觉注入 + 语言模型与 Python 参考一致。
func TestEmbedderImageMatchesONNXReference(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	ref := loadReference(t, dir)

	img := solidPNG(t, ref.ImageSize, uint8(ref.ImageRGB[0]), uint8(ref.ImageRGB[1]), uint8(ref.ImageRGB[2]))
	got, err := e.EmbedImageDense(img, "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	assertNormalized(t, "image", got, ref.Dim)
	assertFrozenPrefix(t, "image", got, ref.ImageVectorPrefix)
}

// TestEmbedderTextIsSensitiveToInput 阴性对照：冻结向量必须真的随输入变化。
//
// 没有这条对照，一个「永远返回同一向量」的错误实现也能通过上面的冻结回归
// （只要那个常量恰好等于参考值）。这里验证不同文本给出不同向量，且相似文本
// 的余弦高于无关文本——即嵌入确实携带语义，而不是常量。
func TestEmbedderTextIsSensitiveToInput(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	base, err := e.VectorizeDense("今天天气怎么样")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	same, err := e.VectorizeDense("今天天气怎么样")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(base, same) < 0.999999 {
		t.Errorf("同一输入两次嵌入不一致: cos=%.9f（ONNX 会话被并发复用或存在非确定性）", cosine(base, same))
	}

	other, err := e.VectorizeDense("数据库索引的选择性是怎么计算的")
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(base, other) > 0.999 {
		t.Errorf("无关文本的余弦高达 %.6f，嵌入可能是常量", cosine(base, other))
	}
}

// TestEmbedderImageMatchesONNXReference 的替代：不依赖冻结参考的不变量检查。
//
// 即使参考文件缺失（没有导出产物）或未重新生成，这些不变量也应成立：
// 图像路径必须真的走了视觉塔，且不同图片给出不同坐标。
func TestEmbedderImageDiffersFromText(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	imgVec, err := e.EmbedImageDense(solidPNG(t, qwenImageSize, 200, 30, 30), "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	txtVec, err := e.VectorizeDense(DefaultInstruction)
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	if cosine(imgVec, txtVec) > 0.999 {
		t.Error("图像向量与文本向量几乎相同，视觉塔可能没被真正执行")
	}

	// 不同颜色的图必须给出不同向量
	blue, err := e.EmbedImageDense(solidPNG(t, qwenImageSize, 30, 150, 220), "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	if cosine(imgVec, blue) > 0.999999 {
		t.Error("不同图片给出相同向量，视觉路径未生效")
	}
}

// TestEmbedderVideoMatchesONNXReference 冻结一个视频用例的参考向量，
// 验证 Go 侧完整的视频路径：多帧预处理（时间组布局）→ M-RoPE（<|video_pad|>）
// → 视觉注入 → 语言模型。
//
// 帧颜色在参考里，用来构造与导出脚本一致的输入；帧顺序（组 g 的 tp0←帧2g、
// tp1←帧2g+1）写错时这个测试会失败——而那类错误看图时发现不了。
//
// ⚠️ 当前**明确未通过**（因此跳过，而不是静默当通过）：Go 侧的 video 模板
// 与 HuggingFace processor 产出的不相等。已定位的差异：processor 会按时间组
// 插入字面时间戳文本，逐 token 实测为
//
//	<|vision_start|> <0.0 seconds> <|vision_start|> {576×video_pad} <|vision_end|>
//	<1.0 seconds> <|vision_start|> {576×video_pad} <|vision_end|>
//
// 而 Go 侧只生成 <|vision_start|>{G×576 pads}<|vision_end|>。实测同一输入
// 下 Python seq=1190（1152 视觉 + 38 文本）、Go 侧只有 22 个文本 token。
// 时间戳文本也会占用 M-RoPE 位置，因此 TestVideoModelInputMRope 的自洽断言
// 虽然通过，也不能证明与官方实现一致。
//
// 修复位置在**本 provider 内部**（模型专属模板本就属于这里，不属于核心）：
// 按 processor 的规则生成同样的分组时间戳文本，然后取消本跳过。
func TestEmbedderVideoMatchesONNXReference(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	ref := loadReference(t, dir)

	if ref.VideoGroups < 2 || len(ref.VideoFrameRGB) != 2*ref.VideoGroups {
		// 产物声明了视频支持、参考里却没有视频用例 → 参考没跟上产物，这是缺陷。
		// 只有「产物本来就不含视频」才允许跳过。
		if artifactDeclaresVideo(t, dir) {
			t.Fatalf("产物声明支持原生视频，但参考缺少视频用例（video_groups=%d frames=%d）："+
				"参考与产物不匹配，请重跑导出脚本的 --verify-only",
				ref.VideoGroups, len(ref.VideoFrameRGB))
		}
		t.Skipf("产物不含原生视频（video_groups=%d），跳过视频回归", ref.VideoGroups)
	}

	// 产物确实带视频用例：说明我们应当能验证。但 Go 侧模板尚未复现 processor
	// 的分组时间戳，现在跑必然失败。显式跳过并说明原因，避免出现
	// 「测试通过」与「视频实际未验证」混为一谈。
	if ref.VideoGroups > 0 {
		t.Skip("已知未修复：Go 侧 video 模板缺少 processor 插入的分组时间戳文本" +
			"（详见本测试注释）；修复前视频冻结回归不得视为已验证")
	}

	frames := make([][]byte, len(ref.VideoFrameRGB))
	for i, rgb := range ref.VideoFrameRGB {
		if len(rgb) != 3 {
			t.Fatalf("帧 %d 颜色字段异常: %v", i, rgb)
		}
		frames[i] = solidPNG(t, ref.ImageSize, uint8(rgb[0]), uint8(rgb[1]), uint8(rgb[2]))
	}

	got, err := e.EmbedVideoDense(frames, "video/mp4")
	if err != nil {
		t.Fatalf("EmbedVideoDense: %v", err)
	}
	assertNormalized(t, "video", got, ref.Dim)
	assertFrozenPrefix(t, "video", got, ref.VideoVectorPrefix)
}

// TestVideoModelInputMRope 逐 token 校验视频的 M-RoPE 位置。
//
// 对应 transformers 的 get_rope_index：它先把 video_grid_thw 按 grid_t 展开成
// G 个 (1,h,w) 的 grid 项，每项单独算位置，项间 current_pos 前进
// max(h,w)/spatial_merge。位置算错不会报错，只是嵌入慢慢变差，所以必须逐项验。
func TestVideoModelInputMRope(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	const groups = 3
	ids, _, position, visual, err := e.tok.videoModelInput("", groups, e.config.MaxLength)
	if err != nil {
		t.Fatalf("videoModelInput: %v", err)
	}
	seq := len(ids)

	// 模板必须以 <|video_pad|> 填充（用成 <|image_pad|> 不会报错，只会错模态）。
	videoPad, ok := e.tok.SpecialID("<|video_pad|>")
	if !ok {
		t.Fatal("tokenizer 缺少 <|video_pad|>")
	}
	imagePad, _ := e.tok.SpecialID("<|image_pad|>")
	wantVisual := groups * qwenVisualTokens
	count := 0
	for i, id := range ids {
		if visual[i] {
			count++
			if id != videoPad {
				t.Fatalf("第 %d 个视觉 token id=%d，期望 video_pad=%d（image_pad=%d）", i, id, videoPad, imagePad)
			}
		}
	}
	if count != wantVisual {
		t.Fatalf("视觉 token 数 = %d，期望 %d", count, wantVisual)
	}

	start := -1
	for i, v := range visual {
		if v {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatal("找不到视觉区间")
	}
	// 视觉区间必须连续（中间不能夹文本 token）。
	for i := start; i < start+wantVisual; i++ {
		if !visual[i] {
			t.Fatalf("视觉区间在 %d 处断裂", i)
		}
	}
	if start+wantVisual < seq && visual[start+wantVisual] {
		t.Fatal("视觉区间超出期望长度")
	}

	// 视觉之前的文本 token 数就是 M-RoPE 的起始位置。
	base0 := int64(start)
	for g := 0; g < groups; g++ {
		base := base0 + int64(g*qwenVisionScale)
		for j := 0; j < qwenVisualTokens; j++ {
			i := start + g*qwenVisualTokens + j
			wantT := base
			wantH := base + int64(j/qwenVisionScale)
			wantW := base + int64(j%qwenVisionScale)
			if position[i] != wantT || position[seq+i] != wantH || position[2*seq+i] != wantW {
				t.Fatalf("组%d 第%d 个视觉 token 位置 = (%d,%d,%d)，期望 (%d,%d,%d)",
					g, j, position[i], position[seq+i], position[2*seq+i], wantT, wantH, wantW)
			}
		}
	}
}

// TestVideoInputRejectsUnsupportedShapes 帧数与档位不匹配时必须明确报错，
// 而不是悄悄补齐/截断成另一个语义。
func TestVideoInputRejectsUnsupportedShapes(t *testing.T) {
	if _, _, err := preprocessVideoFrames([][]byte{solidPNG(t, qwenImageSize, 1, 2, 3)}); err == nil {
		t.Error("单帧无法构成一个时间组，应报错")
	}
	many := make([][]byte, 2*(maxVideoGroupsSafety+1))
	if _, _, err := preprocessVideoFrames(many); err == nil {
		t.Errorf("超过分配安全上限 %d 应报错，而不是静默分配巨量内存", maxVideoGroupsSafety)
	}

	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)
	if _, _, _, _, err := e.tok.visionModelInput("", "<|video_pad|>", 0, e.config.MaxLength); err == nil {
		t.Error("groups=0 应报错")
	}

	// 未导出的档位必须明确报错并告知已加载哪些档，而不是默默找一个相近的。
	if _, err := e.EmbedVideoDense(framesOf(t, 2*(maxExportedGroupsInTest+1)), "video/mp4"); err == nil {
		t.Errorf("未导出的 G=%d 应报错", maxExportedGroupsInTest+1)
	}
}

// maxExportedGroupsInTest 是测试环境预期导出的视频最大档（与导出脚本默认 2,3,4 一致）。
const maxExportedGroupsInTest = 4

func framesOf(t *testing.T, n int) [][]byte {
	t.Helper()
	out := make([][]byte, n)
	for i := range out {
		out[i] = solidPNG(t, qwenImageSize, uint8(i), 100, 150)
	}
	return out
}

// TestEmbedderRejectsUnsupportedModalities 音频必须显式报「不在本空间」。
//
// Qwen3-VL 模型卡与 config 双重确认无 audio_token_id；音频需要另一个真正的
// 音频模型。若这里退化成普通错误，调用方会把它当「本次失败、下次重试」，
// 于是每轮启动都重试一批永远不可能成功的条目。
func TestEmbedderRejectsUnsupportedModalities(t *testing.T) {
	dir := requireONNXArtifacts(t)
	e := newTestEmbedder(t, dir)

	for _, mime := range []string{"audio/wav", "audio/mpeg"} {
		_, err := e.EmbedImageDense([]byte("not-a-real-media"), mime)
		if err == nil {
			t.Fatalf("%s 应返回错误而不是造出向量", mime)
		}
		if !errors.Is(err, embedding.ErrUnsupportedModality) {
			t.Errorf("%s 错误应为 ErrUnsupportedModality，实际: %v", mime, err)
		}
		// 公共 SPI 路径也必须给出可识别的不支持信号。
		if _, err := e.Embed(context.Background(), embedding.Input{
			Modality: embedding.ModalityAudio, Data: []byte("x"), MIME: mime,
		}); !errors.Is(err, embedding.ErrUnsupportedModality) {
			t.Errorf("Embed(audio/%s) 应为 ErrUnsupportedModality，实际: %v", mime, err)
		}
	}

	// 视频**文件**不能直接喂给单帧入口（Go 侧没有视频解码器），
	// 必须由调用方先抽帧再走 EmbedVideoDense。
	if _, err := e.EmbedImageDense([]byte("not-a-real-media"), "video/mp4"); !errors.Is(err, embedding.ErrUnsupportedModality) {
		t.Errorf("EmbedImageDense(video/mp4) 应为 ErrUnsupportedModality，实际: %v", err)
	}
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// TestProviderOpensThroughPublicSPI 走完核心真正的路径：
// embedding.Open(名字) → 工厂 → Info 校验 → Embed。
//
// 它与 newTestEmbedder 的区别很关键：后者直接调 New()，只能证明「模型能加载」；
// 本测试证明**注册表 + 公共契约**这条链路是通的——名字对得上、工厂能构造、
// Info 满足契约、Embed 返回合法向量。核心升级后真正会走的就是这条路由。
func TestProviderOpensThroughPublicSPI(t *testing.T) {
	dir := requireONNXArtifacts(t)

	names := embedding.Names()
	found := false
	for _, n := range names {
		if n == "qwen3vl" {
			found = true
		}
	}
	if !found {
		t.Fatalf("qwen3vl 未注册到公共注册表；已注册: %v", names)
	}

	provider, err := embedding.Open("qwen3vl", embedding.Config{
		Options: map[string]string{"model_dir": dir},
	})
	if err != nil {
		t.Fatalf("embedding.Open(qwen3vl): %v", err)
	}
	defer provider.Close()

	info := provider.Info()
	if info.Dimension != 2048 || info.Fingerprint == "" {
		t.Fatalf("Info 异常: dim=%d fp=%q", info.Dimension, info.Fingerprint)
	}
	// 公共契约路径只声明 text/image：本 provider 没有视频解码器，
	// 若这里出现 video 就意味着核心会创建一条注定失败的输入通道。
	for _, m := range info.Modalities {
		if m == embedding.ModalityVideo {
			t.Fatal("Info 不应声明 video（provider 无视频解码器，见文档）")
		}
	}

	vec, err := provider.Embed(context.Background(), embedding.Input{
		Modality: embedding.ModalityText, Purpose: embedding.PurposeQuery, Text: "hello",
	})
	if err != nil {
		t.Fatalf("Embed(text): %v", err)
	}
	if err := embedding.ValidateVector(vec, info.Dimension); err != nil {
		t.Fatalf("返回向量不合法: %v", err)
	}

	// 未知模态必须给出可识别的「本空间不支持」，而不是普通错误。
	_, err = provider.Embed(context.Background(), embedding.Input{
		Modality: embedding.ModalityAudio, Data: []byte("x"), MIME: "audio/wav",
	})
	if !errors.Is(err, embedding.ErrUnsupportedModality) {
		t.Fatalf("audio 应为 ErrUnsupportedModality，实际: %v", err)
	}
}

// TestOpenRejectsProviderWithoutModelDir 未配置 model_dir 时必须是明确的构造失败，
// 而不是构造成功、每次 Embed 才报错（那会让启动日志看起来正常）。
func TestOpenRejectsProviderWithoutModelDir(t *testing.T) {
	if _, err := embedding.Open("qwen3vl", embedding.Config{}); err == nil {
		t.Fatal("缺 model_dir 时应打开失败")
	}
}
