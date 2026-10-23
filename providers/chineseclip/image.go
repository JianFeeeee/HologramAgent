package chineseclip

import (
	"bytes"
	"fmt"
	"image"

	// 契约要求 provider 自行解码 Data，所以这里注册常见图像格式。
	//
	// ★ webp 必须注册（2026-10-23 实测）：线上 551 个图像块里有 4 个是
	// `image/webp`（由 see_image 抓取的第三方图片），它们**永远嵌不进去** ——
	// 启动时向量迁移报「失败=4」，但错误被 continue 吞掉、没有任何日志，
	// 只看到数字对不上。根因就是这里少了 webp 注册：
	//
	//     image.Decode(...) → "image: unknown format"
	//
	// Go 标准库不含 webp 解码器（那是 x/image 的子包），所以这是新增依赖的
	// 唯一理由。x/image 是纯 Go、无 cgo，不改变二进制形态。
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// plane 是单通道浮点平面。
type plane struct {
	w, h int
	data []float32
}

// preprocessImage 把原始图像字节变成 ONNX 需要的 NCHW 张量：
// 缩放到 size×size（双三次，复刻 PIL 的系数）→ 归一化（x/255 - mean）/ std。
//
// 缩放在 RGB 三个通道上分别进行，与官方 ChineseCLIPFeatureExtractor 一致
// （do_resize=true、do_center_crop=false、resample=BICUBIC、rescale 1/255）。
func preprocessImage(data []byte, size int, mean, std []float64) ([]float32, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("chineseclip: 解码图像: %w", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, fmt.Errorf("chineseclip: 图像尺寸非法 %dx%d", bounds.Dx(), bounds.Dy())
	}

	planes := [3]plane{}
	for c := range planes {
		planes[c] = plane{w: bounds.Dx(), h: bounds.Dy(), data: make([]float32, bounds.Dx()*bounds.Dy())}
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			idx := (y-bounds.Min.Y)*bounds.Dx() + (x - bounds.Min.X)
			// RGBA() 返回的是 16 位预乘值；不透明图像下右移 8 位即得 8 位分量。
			planes[0].data[idx] = float32(r >> 8)
			planes[1].data[idx] = float32(g >> 8)
			planes[2].data[idx] = float32(b >> 8)
		}
	}

	out := make([]float32, 3*size*size)
	for c := range planes {
		resized := resizeBicubic(planes[c], size, size)
		for i, v := range resized.data {
			scaled := float64(v) / 255.0
			out[c*size*size+i] = float32((scaled - mean[c]) / std[c])
		}
	}
	return out, nil
}

// resizeBicubic 复刻 PIL 的可分离双三次缩放（系数来自 PIL 的
// precompute_coeffs + bicubic_filter，a=-0.5）。
//
// 为什么不引第三方 resize：本机 x/image 未进模块缓存，而它最新版还要求把整个
// 工具链升到 Go 1.26；为一个缩放函数动工具链不划算。PIL 的算法只有几十行，
// 照抄系数能保证与官方预处理足够接近（已用端到端 cos 验证）。
func resizeBicubic(src plane, dstW, dstH int) plane {
	if src.w == dstW && src.h == dstH {
		return src
	}
	wsX := buildWeights(src.w, dstW)
	tmp := plane{w: dstW, h: src.h, data: make([]float32, dstW*src.h)}
	for y := 0; y < src.h; y++ {
		row := y * src.w
		for dx := 0; dx < dstW; dx++ {
			var sum float32
			for _, t := range wsX[dx] {
				sum += t.w * src.data[row+t.i]
			}
			tmp.data[y*dstW+dx] = sum
		}
	}

	wsY := buildWeights(src.h, dstH)
	dst := plane{w: dstW, h: dstH, data: make([]float32, dstW*dstH)}
	for dy := 0; dy < dstH; dy++ {
		for x := 0; x < dstW; x++ {
			var sum float32
			for _, t := range wsY[dy] {
				sum += t.w * tmp.data[t.i*dstW+x]
			}
			dst.data[dy*dstW+x] = sum
		}
	}
	return dst
}

type weightTerm struct {
	i int
	w float32
}

// buildWeights 按 PIL 的 precompute_coeffs 计算每个目标像素的源像素权重。
func buildWeights(srcLen, dstLen int) [][]weightTerm {
	const support = 2.0 // BICUBIC 的支撑半径

	filterScale := float64(srcLen) / float64(dstLen)
	if filterScale < 1.0 {
		filterScale = 1.0
	}
	scale := filterScale
	filterSupport := support * filterScale
	invScale := 1.0 / filterScale

	out := make([][]weightTerm, dstLen)
	for d := 0; d < dstLen; d++ {
		center := (float64(d) + 0.5) * scale
		xmin := int(center - filterSupport + 0.5)
		if xmin < 0 {
			xmin = 0
		}
		xmax := int(center + filterSupport + 0.5)
		if xmax > srcLen {
			xmax = srcLen
		}
		if xmax <= xmin {
			// 极端缩放下的兜底：退化为最近邻，避免空权重导致除零。
			idx := int(center)
			if idx < 0 {
				idx = 0
			}
			if idx >= srcLen {
				idx = srcLen - 1
			}
			out[d] = []weightTerm{{i: idx, w: 1}}
			continue
		}
		terms := make([]weightTerm, 0, xmax-xmin)
		var total float64
		for x := xmin; x < xmax; x++ {
			w := bicubicKernel((float64(x) - center + 0.5) * invScale)
			if w == 0 {
				continue
			}
			terms = append(terms, weightTerm{i: x, w: float32(w)})
			total += w
		}
		if total != 0 {
			for i := range terms {
				terms[i].w = float32(float64(terms[i].w) / total)
			}
		}
		out[d] = terms
	}
	return out
}

// bicubicKernel 是 PIL 的 bicubic_filter（a = -0.5）。
func bicubicKernel(x float64) float64 {
	const a = -0.5
	if x < 0 {
		x = -x
	}
	switch {
	case x < 1.0:
		return ((a+2.0)*x-(a+3.0))*x*x + 1.0
	case x < 2.0:
		return (((x-5.0)*x+8.0)*x - 4.0) * a
	}
	return 0
}
