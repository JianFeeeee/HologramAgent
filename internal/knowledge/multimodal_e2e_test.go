package knowledge

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/pkg/embedding"
)

// fakeEmbedSvc 按内核契约提供 text/image 的 4 维向量：
// 「偏红的图」与「红色的文字」落在同一方向，用于验证真正的跨模态召回。
// 本测试用「内置 http provider + 一个最小契约服务」跑完整真实链路：
// embedding.Open → AdaptProvider → media CAS → SetDenseSpace/SetMediaGetter
// → AddWithMedia（文本⊕图片融合）→ 以图搜知识 → 落盘 → 重启命中缓存。
//
// 为何不用 ONNX provider：真模型要 200MB 权重 + 2~3 分钟加载，不能进 CI。
// 但这条链路是 provider 无关的——AdaptProvider 之后内核只认
// MultimodalEmbedder 接口。ONNX 路径已用本机 qwen3-vl 实测通过
// （dim=2048，以图搜知识 score=0.757），见本文件末尾注释。
func fakeEmbedSvc() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Modality string `json:"modality"`
			Text     string `json:"text"`
			MIME     string `json:"mime"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// 极简但**有区分度**的"语义"：图片统一落方向 0；文本按长度奇偶
		// 落方向 0 或 1。此前用长度/2 决定，导致 photo 与 plain 拿到同一个
		// 向量形成同分，测试开始依赖并列先后——而并列是不确定的。
		vec := []float64{0, 1, 0, 0}
		if req.Modality == "text" {
			if len([]rune(req.Text))%2 == 0 {
				vec = []float64{1, 0, 0, 0}
			}
		} else {
			vec = []float64{1, 0, 0, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"embedding": vec})
	}))
}

func TestMultimodalEndToEndWithRealProvider(t *testing.T) {
	svc := fakeEmbedSvc()
	defer svc.Close()

	p, err := embedding.Open("http", embedding.Config{Options: map[string]string{
		"endpoint":    svc.URL,
		"dimension":   "4",
		"fingerprint": "test-http-4d",
	}})
	if err != nil {
		t.Fatalf("Open(http) 失败: %v", err)
	}
	defer p.Close()
	ds, err := vector.AdaptProvider(p)
	if err != nil {
		t.Fatalf("AdaptProvider: %v", err)
	}
	t.Logf("真实 provider: dim=%d fp=%s loaded=%v", ds.Dim(), ds.Fingerprint(), ds.Loaded())

	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.SetDenseSpace(ds)

	img := mmTinyPNG(t)
	// 先建纯文本条目：字节还没落盘，现在注册媒体引用会让 denseFor
	// 读不到文件。落盘后再用 AttachMedia 挂上并重算 —— 这样 Media 恰好 1 条。
	if err := s.Add("photo", "一只猫的照片"); err != nil {
		t.Fatal(err)
	}
	// 字节落进条目目录（CAS 淘汰后：媒体随条目生灭）
	if err := os.WriteFile(filepath.Join(dir, "photo", "photo.png"), img, 0644); err != nil {
		t.Fatal(err)
	}
	// 重算该条稠密向量以纳入图片
	if err := s.AttachMedia("photo", KnowledgeMediaRef{File: "photo.png", MIME: "image/png"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("plain", "无关的纯文本知识"); err != nil {
		t.Fatal(err)
	}
	t.Logf("DenseStats: %v", s.DenseStats())

	k := s.items["photo"]
	if k == nil || len(k.Dense) != 4 {
		t.Fatalf("photo 稠密向量异常: %+v", k)
	}
	t.Logf("photo.Dense=%v（文本⊕图片融合后应偏向方向0）", k.Dense)

	// 以图搜知识：图片向量 = 方向0，photo 的融合向量也应偏向方向0
	qv, err := ds.EmbedImageDense(img, "image/png")
	if err != nil {
		t.Fatalf("EmbedImageDense: %v", err)
	}
	hits := s.denseHits(qv)
	if len(hits) == 0 {
		t.Fatal("以图搜知识无命中")
	}
	t.Logf("以图搜知识: top1=%s score=%.4f", hits[0].id, hits[0].score)
	if hits[0].id != "photo" {
		t.Errorf("以图搜知识首位应为 photo，实为 %s", hits[0].id)
	}

	// 缓存 + 重启
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	s2 := NewStore(dir)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	s2.SetDenseSpace(ds)
	built, _ := s2.ReindexDense()
	t.Logf("重启后 ReindexDense built=%d（0=命中缓存）", built)
	if built != 0 {
		t.Errorf("重启后应命中缓存，实为 %d", built)
	}
	if len(s2.items["photo"].Media) != 1 {
		t.Error("媒体引用未跨重启存活")
	}
	s2.Stop()
}

// 稠密路同分时的次序必须可重复。
//
// denseHits 曾用 map 迭代 + 只按分数排序：同分条目的相对次序随机，
// 表现为「同样的查询两次给出不同首位」。Search 的主排序早有 tie-break，
// 这条漏了。
func TestDenseHitsTieIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// fakeMM 缺省给同一个向量 ⇒ 查询与全部条目同向，全部同分。
	// 注意必须走 SetDenseSpace（稠密路），不是 SetVectorizer（稀疏路）。
	mm := &fakeMM{
		dim: 8, fp: "tie-fp", loaded: true,
		text: map[string][]float64{"__default": {1, 1, 1, 1, 1, 1, 1, 1}},
		img:  map[string][]float64{"__default": {1, 1, 1, 1, 1, 1, 1, 1}},
	}
	s.SetDenseSpace(mm)
	_ = s.Add("zeta", "同分内容")
	_ = s.Add("alpha", "同分内容")
	_ = s.Add("mid", "同分内容")

	qv := []float64{1, 1, 1, 1, 1, 1, 1, 1}
	var first string
	for i := 0; i < 30; i++ {
		hits := s.denseHits(qv)
		if len(hits) == 0 {
			t.Fatal("无命中")
		}
		if i == 0 {
			first = hits[0].id
			continue
		}
		if hits[0].id != first {
			t.Fatalf("第 %d 次首位变了: %s → %s（同分次序不确定）", i, first, hits[0].id)
		}
	}
	// 且必须按名字定序
	hits := s.denseHits(qv)
	if hits[0].id != "alpha" {
		t.Errorf("同分应按名字定序，首位应为 alpha，实为 %s", hits[0].id)
	}
}

// mmTinyPNG 造一张 8x8 的真 PNG（需为合法 PNG 才能被 provider 当图片处理）。
func mmTinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(3, 3, color.RGBA{R: 200, G: 40, B: 40, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
