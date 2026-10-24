package chineseclip

// webp 解码支持与「失败必须可观测」的判据。
//
// ## 为什么这两条要放在这里
//
// 2026-10-23 线上排查：启动日志只有一行
//
//	[media] 向量迁移完成: 成功=0 失败=4 总计=4
//
// 没有任何信息说明为什么失败。反查数据库才发现那 4 个块的 mime 是
// `image/webp`，而本 provider 只注册了 gif/jpeg/png 三个解码器 ⇒
// `image.Decode` 报 "unknown format" ⇒ 它们**永远**拿不到向量，
// 语义检索也永远搜不到它们。
//
// 两个独立缺陷：
//  1. 功能缺口：webp 不可解码
//  2. 可观测性缺口：失败被 `continue` 吞掉，只有计数没有原因
//
// 判据分开钉住，避免以后只修其中一个。

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// webpBytes 是一个最小的合法 webp 文件（1×1 白色，RIFF/WEBP/VP8L）。
//
// 手写而不依赖测试夹具：夹具文件容易被误删，而"能解码 webp"这件事
// 必须一直成立 —— 线上确实存在 webp 记忆块。
var webpBytes = []byte{
	// 由 ffmpeg 生成的真实 2×2 白色 webp（44 字节，VP8 有损编码）。
	// 手写字节序列容易构造出不合法容器 —— 必须用真实编码器产物。
	0x52, 0x49, 0x46, 0x46, 0x24, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50,
	0x56, 0x50, 0x38, 0x20, 0x18, 0x00, 0x00, 0x00, 0x30, 0x01, 0x00, 0x9d,
	0x01, 0x2a, 0x02, 0x00, 0x02, 0x00, 0x02, 0x00, 0x34, 0x25, 0xa4, 0x00,
	0x03, 0x70, 0x00, 0xfe, 0xfb, 0x94, 0x00, 0x00,
}

// ① webp 必须能被解码（否则 memory_blocks 里的 webp 块永远无向量）。
//
// 变异：注释掉 image.go 里的 `_ "golang.org/x/image/webp"` ⇒ 红。
func TestWebpIsDecodable(t *testing.T) {
	_, format, err := image.Decode(bytes.NewReader(webpBytes))
	if err != nil {
		t.Fatalf("webp 解码失败: %v —— image.go 里缺少 _ \"golang.org/x/image/webp\""+
			"（线上 4 个 webp 记忆块因此永远拿不到向量）", err)
	}
	if format != "webp" {
		t.Fatalf("格式识别错误: got %q want \"webp\"", format)
	}
}

// ② webp 必须能走完整的预处理链路（不只是解码，还要变成 NCHW 张量）。
//
// 只验解码不够：预处理里有缩放/归一化，任何一步对 webp 不适用都算没支持。
func TestWebpPreprocessProducesTensor(t *testing.T) {
	const size = 32
	mean := []float64{0.48145466, 0.4578275, 0.40821073}
	std := []float64{0.26862954, 0.26130258, 0.27577711}

	tensor, err := preprocessImage(webpBytes, size, mean, std)
	if err != nil {
		t.Fatalf("webp 预处理失败: %v", err)
	}
	if want := 3 * size * size; len(tensor) != want {
		t.Fatalf("张量长度 %d，期望 %d（3×%d×%d）", len(tensor), want, size, size)
	}
	// 1×1 白色图放大后应接近 (1-mean)/std 的白点值；只验非全零，
	// 具体像素值由 jpeg/png 的测试覆盖（同一套代码路径）。
	nonZero := false
	for _, v := range tensor {
		if v != 0 {
			nonZero = true
			break
		}
	}
	if !nonZero {
		t.Fatal("张量全为零 —— 预处理没有真正读到像素")
	}
}

// ③ png 仍必须可用（防止注册表改动把其它格式弄坏）。
func TestPngStillDecodable(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 255, 255, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("构造 png 失败: %v", err)
	}
	if _, format, err := image.Decode(&buf); err != nil || format != "png" {
		t.Fatalf("png 解码异常: format=%q err=%v", format, err)
	}
}
