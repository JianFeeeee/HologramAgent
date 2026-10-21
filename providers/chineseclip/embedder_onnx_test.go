//go:build onnxruntime

package chineseclip

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
)

// cosine 计算两个向量的**真余弦**：两边都先归一化。
//
// 参考向量存的是 ONNX 的原始输出（未归一化，模长 10~36），而 provider 的输出是
// L2 归一化后的。直接点积会得到参考向量的模长（例如 13.6），既不是余弦，
// 也会让单侧阈值判定变成假通过。
func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return math.NaN()
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return math.NaN()
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// solidPNG 生成与导出脚本 FIXTURE_IMAGES 一致的纯色图（320×320）。
func solidPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := 0; y < 320; y++ {
		for x := 0; x < 320; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成测试图: %v", err)
	}
	return buf.Bytes()
}

func openTestProvider(t *testing.T) embedding.Provider {
	t.Helper()
	dir := modelDir(t)
	p, err := embedding.Open("chineseclip", embedding.Config{
		Options: map[string]string{"model_dir": dir},
	})
	if err != nil {
		t.Fatalf("embedding.Open(chineseclip): %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// provider 必须通过公共 SPI 可见，并声明正确的向量空间身份。
func TestProviderInfo(t *testing.T) {
	p := openTestProvider(t)
	info := p.Info()
	if info.Dimension != 512 {
		t.Errorf("维度应为 512，得到 %d", info.Dimension)
	}
	if len(info.Fingerprint) != 64 {
		t.Errorf("指纹应为 64 位十六进制，得到 %q", info.Fingerprint)
	}
	want := map[embedding.Modality]bool{embedding.ModalityText: true, embedding.ModalityImage: true}
	if len(info.Modalities) != len(want) {
		t.Fatalf("模态应为 %v，得到 %v", want, info.Modalities)
	}
	for _, m := range info.Modalities {
		if !want[m] {
			t.Errorf("声明了未支持的模态 %q", m)
		}
	}
}

// 文本向量必须与官方 PyTorch 参考一致。
//
// 文本侧没有预处理歧义（分词器已逐 token 对齐），所以要求非常严：
// 余弦与参考的偏差应小于 1e-9。
func TestEmbedTextMatchesReference(t *testing.T) {
	dir := modelDir(t)
	p := openTestProvider(t)
	ref := loadReference(t, dir)
	if len(ref.Texts) == 0 {
		t.Fatal("参考里没有文本用例")
	}
	ctx := context.Background()
	for _, c := range ref.Texts {
		got, err := p.Embed(ctx, embedding.Input{
			Modality: embedding.ModalityText,
			Purpose:  embedding.PurposeQuery,
			Text:     c.Text,
		})
		if err != nil {
			t.Fatalf("Embed(text=%q): %v", c.Text, err)
		}
		if err := embedding.ValidateVector(got, 512); err != nil {
			t.Fatalf("向量不合法 %q: %v", c.Text, err)
		}
		cos := cosine(got, c.Vector)
		if math.Abs(cos-1.0) > 1e-9 {
			t.Errorf("文本向量与参考不一致 %q: cos=%.12f", c.Text, cos)
		}
		t.Logf("文本 cos=%.12f  %s", cos, c.Text[:min(len(c.Text), 24)])
	}
}

// 图像向量与官方参考一致（容忍缩放实现差异）。
//
// Go 侧自写 bicubic（PIL 系数）与官方预处理不会逐位相同，故用余弦阈值；
// 0.999 足以证明「同一条管线」，同时不会掩盖把像素顺序或归一化写错这类错误
// （那类错误会直接掉到 0.9 以下）。
func TestEmbedImageMatchesReference(t *testing.T) {
	dir := modelDir(t)
	p := openTestProvider(t)
	ref := loadReference(t, dir)
	ctx := context.Background()

	colors := map[string]color.RGBA{
		"red":   {R: 220, G: 30, B: 30, A: 255},
		"green": {R: 60, G: 120, B: 60, A: 255},
		"blue":  {R: 30, G: 30, B: 220, A: 255},
		"gray":  {R: 128, G: 128, B: 128, A: 255},
	}
	checked := 0
	for _, c := range ref.Images {
		rgba, ok := colors[c.Name]
		if !ok {
			continue // gradient 在 Go 侧不便逐位复刻，跳过（仍由导出脚本覆盖）
		}
		got, err := p.Embed(ctx, embedding.Input{
			Modality: embedding.ModalityImage,
			Purpose:  embedding.PurposeDocument,
			Data:     solidPNG(t, rgba),
			MIME:     "image/png",
		})
		if err != nil {
			t.Fatalf("Embed(image=%s): %v", c.Name, err)
		}
		cos := cosine(got, c.Vector)
		// 双侧判定：单侧下界挡不住「模长缩放」这类错误（未归一化的参考向量
		// 会让点积恰好远大于 1 而“通过”）。
		if math.Abs(cos-1.0) > 1e-3 {
			t.Errorf("图像向量与参考不一致 %s: cos=%.6f", c.Name, cos)
		}
		t.Logf("图像 cos=%.6f  %s", cos, c.Name)
		checked++
	}
	if checked == 0 {
		t.Fatal("没有比对任何图像用例（参考里缺少纯色样例）")
	}
}

// 跨模态必须真的有区分度：红图对"红色"文本应高于"蓝色"文本。
// 这条防的是「向量塌缩但 cos 检查全过」那类假通过。
func TestCrossModalDiscrimination(t *testing.T) {
	dir := modelDir(t)
	p := openTestProvider(t)
	ref := loadReference(t, dir)
	ctx := context.Background()

	var redText, blueText string
	for _, c := range ref.Texts {
		switch c.Text {
		case "一张红色方块的图片":
			redText = c.Text
		case "蓝色的天空":
			blueText = c.Text
		}
	}
	if redText == "" || blueText == "" {
		t.Skip("参考里缺少用于跨模态判别的文本")
	}

	imgVec, err := p.Embed(ctx, embedding.Input{
		Modality: embedding.ModalityImage, Data: solidPNG(t, color.RGBA{R: 220, G: 30, B: 30, A: 255}),
		MIME: "image/png",
	})
	if err != nil {
		t.Fatalf("Embed(image): %v", err)
	}
	redVec, err := p.Embed(ctx, embedding.Input{Modality: embedding.ModalityText, Text: redText})
	if err != nil {
		t.Fatalf("Embed(red text): %v", err)
	}
	blueVec, err := p.Embed(ctx, embedding.Input{Modality: embedding.ModalityText, Text: blueText})
	if err != nil {
		t.Fatalf("Embed(blue text): %v", err)
	}
	if cosine(imgVec, redVec) <= cosine(imgVec, blueVec) {
		t.Errorf("跨模态判别失败：红图-红文本 %.4f 应高于 红图-蓝文本 %.4f",
			cosine(imgVec, redVec), cosine(imgVec, blueVec))
	}
}

// 音频/视频必须明确拒绝，绝不用别的模型向量冒充。
func TestEmbedRejectsUnsupportedModalities(t *testing.T) {
	p := openTestProvider(t)
	ctx := context.Background()
	for _, m := range []embedding.Modality{embedding.ModalityAudio, embedding.ModalityVideo} {
		_, err := p.Embed(ctx, embedding.Input{Modality: m, Data: []byte("x"), MIME: "application/octet-stream"})
		if err == nil {
			t.Fatalf("模态 %s 应被拒绝", m)
		}
		if !errors.Is(err, embedding.ErrUnsupportedModality) {
			t.Errorf("模态 %s 的错误应可判定为 ErrUnsupportedModality，得到: %v", m, err)
		}
	}
}

// 空输入必须报错而不是产出垃圾向量。
func TestEmbedRejectsEmptyInput(t *testing.T) {
	p := openTestProvider(t)
	ctx := context.Background()
	if _, err := p.Embed(ctx, embedding.Input{Modality: embedding.ModalityText, Text: "   "}); err == nil {
		t.Error("空文本应报错")
	}
	if _, err := p.Embed(ctx, embedding.Input{Modality: embedding.ModalityImage}); err == nil {
		t.Error("空图像应报错")
	}
}

// 指纹必须稳定且对产物内容敏感（同目录两次打开一致）。
func TestFingerprintStable(t *testing.T) {
	dir := modelDir(t)
	first, err := embedding.Open("chineseclip", embedding.Config{Options: map[string]string{"model_dir": dir}})
	if err != nil {
		t.Fatalf("首次打开: %v", err)
	}
	defer first.Close()
	second, err := embedding.Open("chineseclip", embedding.Config{Options: map[string]string{"model_dir": dir}})
	if err != nil {
		t.Fatalf("二次打开: %v", err)
	}
	defer second.Close()
	if first.Info().Fingerprint != second.Info().Fingerprint {
		t.Errorf("同一产物两次打开指纹不一致: %s vs %s",
			first.Info().Fingerprint, second.Info().Fingerprint)
	}
}

// 缺 model_dir 时不得静默成功打开一个“空产物”。
//
// 2026-10-01 语义变更：New 增加了 findModelDir 回退链（configured →
// /usr/lib/homeagent/models）。若本机装了系统级产物，回退命中属合法行为，
// 但打开的必须是那个真实产物（指纹非空）；若系统目录也没有，则必须报错，
// 且错误里要带可执行指引（export 脚本名）。
func TestOpenRejectsMissingModelDir(t *testing.T) {
	got, err := embedding.Open("chineseclip", embedding.Config{})
	if err == nil {
		// 回退命中了系统目录：允许，但必须是真的产物而不是空壳。
		defer got.Close()
		if got.Info().Fingerprint == "" {
			t.Fatal("回退打开成功但指纹为空（打开了个空壳？）")
		}
		return
	}
	// 未命中回退 ⇒ 必须报错且给可执行指引。
	if !strings.Contains(err.Error(), "export_chineseclip_onnx.py") {
		t.Fatalf("错误应带可执行指引（export 脚本名），got: %v", err)
	}
}

// 目录存在但不是本模型产物时，必须报出缺哪个文件，而不是静默用默认值。
func TestOpenRejectsIncompleteArtifacts(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/embed_config.json", []byte(`{"dim":512,"max_length":52,"image_size":224,"image_mean":[0.5,0.5,0.5],"image_std":[0.5,0.5,0.5],"text_onnx":"TextEncoder.onnx","vision_onnx":"VisionEncoder.onnx"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := embedding.Open("chineseclip", embedding.Config{Options: map[string]string{"model_dir": dir}})
	if err == nil {
		t.Fatal("产物不完整时应打开失败")
	}
}
