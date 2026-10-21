package knowledge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// fakeMM 是可控的多模态嵌入器：文本与图片各自查表，缺省给同一个兜底向量。
// Dim/Fingerprint 可变，用来验证维度与指纹守卫。
type fakeMM struct {
	dim  int
	fp   string
	text map[string][]float64
	img  map[string][]float64
	// imgErr 按 digest 注入错误（验证 ErrModalityUnsupported 的静默跳过）
	imgErr map[string]error
	loaded bool
}

func (f *fakeMM) VectorizeDense(t string) ([]float64, error) {
	if v, ok := f.text[t]; ok {
		return v, nil
	}
	return f.text["__default"], nil
}

func (f *fakeMM) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	key := string(img)
	if err, ok := f.imgErr[key]; ok {
		return nil, err
	}
	if v, ok := f.img[key]; ok {
		return v, nil
	}
	return f.img["__default"], nil
}

func (f *fakeMM) Fingerprint() string { return f.fp }
func (f *fakeMM) Dim() int            { return f.dim }
func (f *fakeMM) Loaded() bool        { return f.loaded }
func (f *fakeMM) Close()              {}

// fakeMedia 是仅按 digest 查表的 MediaGetter。
type fakeMedia struct{ byDigest map[string][]byte }

func (m fakeMedia) Get(digest string) ([]byte, error) {
	if b, ok := m.byDigest[digest]; ok {
		return b, nil
	}
	return nil, errors.New("media: not found")
}

// 跨模态召回：一条知识的“图”与查询向量相近，就该被召回——即便它的正文
// 与查询毫无词面重叠。这是稠密路存在的全部理由。
func TestDenseCrossModalRecall(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// 两个正交方向：eax 表示“猫的图”，eby 表示“别的”。
	//
	// ★ 桩的 key 必须与 denseFor 实际传入的文本**逐字一致**：
	//   denseFor 算的是 `k.Name + " " + k.Content`，不是纯 Content。
	//   写成纯 Content 会静默回落到 __default，两条条目就拿到同一向量。
	//
	// ★ ecy（第三个正交方向）给「别的条目」的正文：若 other 与 cat-photo
	//   的正文同向量，纯稠密路下“猫”查询会让 other（余弦 1.0）排在
	//   cat-photo（文本⊕图融合后 0.707）之前——那不是产品缺陷，是
	//   **桩把所有未知文本映射到同一向量**造成的假象。
	eax := []float64{1, 0, 0, 0}
	eby := []float64{0, 1, 0, 0}
	ecy := []float64{0, 0, 1, 0}
	mm := &fakeMM{
		dim: 4, fp: "fake-v1", loaded: true,
		text: map[string][]float64{
			"__default":         eby,
			"cat-photo 猫 图片 说明": eby,
			"other 完全无关的正文":     ecy,
		},
		img: map[string][]float64{"__default": eby, "d-cat": eax},
	}
	s.SetDenseSpace(mm)

	if err := s.AddWithMedia("cat-photo", "猫 图片 说明", []KnowledgeMediaRef{
		// ★ 带 Data：Write 路径会 writeMediaFiles 落盘到条目目录，随后 denseFor
		//   读回它算图向量。字节 "d-cat" 对应 fakeMM.img 里的 eax 方向。
		{File: "cat.png", MIME: "image/png", Kind: "image", Data: []byte("d-cat")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("other", "完全无关的正文"); err != nil {
		t.Fatal(err)
	}

	// 查询向量直接命中“猫图”方向（模拟以图搜知识）
	qv := eax
	hits := s.denseHits(qv)
	if len(hits) == 0 {
		t.Fatal("稠密路无命中")
	}
	if hits[0].id != "cat-photo" {
		t.Fatalf("稠密路首位应为 cat-photo，实为 %v", hits)
	}
	// 分数应显著高于正交基线（0.707 是文本⊕猫图两个正交方向融合的必然值，
	// 不是噪声——单模态文档在同查询下只会有 ~0）
	if !(hits[0].score > 0.5) {
		t.Errorf("cat-photo 与查询向量应显著同向，实为 %f", hits[0].score)
	}
	// 正文里的“猫”字查询也应召回它。
	// ★ 纯稠密路下这里的分数是 1.0（查询向量与 cat-photo 正文向量同向），
	//   而 other 是正交的 0 —— 前提是桩给 other 一个**真正无关**的方向
	//   （见上面 ecy 的说明）。
	if got := s.Search("猫", 3); len(got) == 0 || got[0].Name != "cat-photo" {
		t.Errorf("按正文查询应召回 cat-photo，实为 %v", namesOf(got))
	}
}

// 维度守卫：同指纹但维度不同的向量必须被跳过，否则会算出错维度余弦。
func TestDenseRejectsWrongDim(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	mm := &fakeMM{dim: 4, fp: "fp", loaded: true,
		text: map[string][]float64{"__default": {1, 0, 0, 0}},
		img:  map[string][]float64{"__default": {1, 0, 0, 0}},
	}
	s.SetDenseSpace(mm)
	if err := s.Add("k", "正文"); err != nil {
		t.Fatal(err)
	}
	// 手动塞一个维度不符的向量（模拟换模型后的残留）
	s.mu.Lock()
	s.items["k"].Dense = []float64{1, 0, 0, 0, 0, 0, 0, 0}
	s.mu.Unlock()

	if hits := s.denseHits([]float64{1, 0, 0, 0}); len(hits) != 0 {
		t.Errorf("维度不符的条目必须被跳过，实为 %v", hits)
	}
}

// 指纹守卫：模型换过后，旧向量在重算前不得参与召回。
func TestDenseRejectsStaleFingerprint(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	mm := &fakeMM{dim: 4, fp: "fp-new", loaded: true,
		text: map[string][]float64{"__default": {1, 0, 0, 0}},
		img:  map[string][]float64{"__default": {1, 0, 0, 0}},
	}
	s.SetDenseSpace(mm)
	if err := s.Add("k", "正文"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.items["k"].Dense = []float64{1, 0, 0, 0}
	s.items["k"].DenseFP = "fp-old"
	s.mu.Unlock()

	if hits := s.denseHits([]float64{1, 0, 0, 0}); len(hits) != 0 {
		t.Errorf("指纹过期的条目在重算前不得参与召回，实为 %v", hits)
	}
	// ReindexDense 应把它修好
	if built, _ := s.ReindexDense(); built != 1 {
		t.Errorf("ReindexDense 应重算 1 条，实为 %d", built)
	}
	if hits := s.denseHits([]float64{1, 0, 0, 0}); len(hits) != 1 {
		t.Errorf("重算后应可召回，实为 %v", hits)
	}
}

// ReindexDense 幂等：第二次不该再做任何事。
func TestReindexDenseIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	mm := &fakeMM{dim: 3, fp: "fp", loaded: true,
		text: map[string][]float64{"__default": {1, 1, 0}},
		img:  map[string][]float64{"__default": {0, 1, 1}},
	}
	s.SetDenseSpace(mm)
	for _, n := range []string{"a", "b", "c"} {
		if err := s.Add(n, "正文"+n); err != nil {
			t.Fatal(err)
		}
	}
	// Add 已当场算过，故首次 Reindex 应为 0 新建
	if built, _ := s.ReindexDense(); built != 0 {
		t.Errorf("Add 已算过稠密向量，Reindex 应为 0，实为 %d", built)
	}
	if built, _ := s.ReindexDense(); built != 0 {
		t.Errorf("重复 Reindex 应幂等，实为 %d", built)
	}
}

// 媒体模态不受支持时必须**静默跳过该媒体**，但整条知识的文本向量仍要成立。
func TestDenseUnsupportedModalityStillTextEmbedded(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	etext := []float64{1, 0}
	mm := &fakeMM{
		dim: 2, fp: "fp", loaded: true,
		text:   map[string][]float64{"__default": etext},
		img:    map[string][]float64{"__default": {0, 1}},
		imgErr: map[string]error{"d-audio": vector.ErrModalityUnsupported},
	}
	s.SetDenseSpace(mm)

	if err := s.AddWithMedia("song", "一首歌", []KnowledgeMediaRef{{File: "a.mp3", MIME: "audio/mpeg"}}); err != nil {
		t.Fatal(err)
	}
	k := s.items["song"]
	if k == nil {
		t.Fatal("条目未写入")
	}
	if len(k.Dense) != 2 {
		t.Fatalf("音频不支持时应退化为纯文本向量，实为 %v", k.Dense)
	}
	// 纯文本向量应与文本方向同向（没被音频的错向量污染）
	if k.Dense[0] <= 0 {
		t.Errorf("文本向量方向被污染：%v", k.Dense)
	}
}

// 未注入稠密空间时，行为必须与加入稠密路之前逐字一致。
func TestNoDenseSpaceKeepsLegacyBehavior(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if s.denseEnabled() {
		t.Fatal("未注入时稠密路应为禁用")
	}
	for _, n := range []string{"coffee", "sleep", "arch"} {
		if err := s.Add(n, "正文 "+n); err != nil {
			t.Fatal(err)
		}
	}
	// 稀疏两路仍要工作
	if got := s.Search("coffee", 3); len(got) == 0 || got[0].Name != "coffee" {
		t.Errorf("无稠密路时稀疏两路应正常召回，实为 %v", namesOf(got))
	}
	// Add 不应试图算稠密向量
	for _, k := range s.items {
		if len(k.Dense) != 0 {
			t.Errorf("无稠密路时不应产生稠密向量：%s", k.Name)
		}
	}
	if st := s.DenseStats(); st["enabled"] != false {
		t.Errorf("DenseStats 应报告未启用，实为 %v", st)
	}
}

// AttachMedia 挂上媒体后必须当场重算稠密向量（否则要等下次 Reindex）。
func TestAttachMediaRecomputesDense(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	eax := []float64{1, 0}
	mm := &fakeMM{dim: 2, fp: "fp", loaded: true,
		text: map[string][]float64{"__default": {0, 1}},
		img:  map[string][]float64{"__default": {0, 1}, "d-x": eax},
	}
	s.SetDenseSpace(mm)

	if err := s.Add("k", "正文"); err != nil {
		t.Fatal(err)
	}
	// AttachMedia 是「挂已在盘上的媒体」：字节必须先落到条目目录，
	// 否则 denseFor 读不到。"d-x" 对应 fakeMM.img 里的 eax 方向。
	if err := os.WriteFile(filepath.Join(dir, "k", "x.png"), []byte("d-x"), 0644); err != nil {
		t.Fatal(err)
	}
	before := append([]float64{}, s.items["k"].Dense...)
	if err := s.AttachMedia("k", KnowledgeMediaRef{File: "x.png", MIME: "image/png"}); err != nil {
		t.Fatal(err)
	}
	after := s.items["k"].Dense
	if len(after) != 2 {
		t.Fatalf("AttachMedia 后应有 2 维稠密向量，实为 %v", after)
	}
	// 融合了 eax 后方向应偏向第一维，与 before 不同
	if before[0] == after[0] && before[1] == after[1] {
		t.Errorf("AttachMedia 未重算稠密向量：%v", after)
	}
}

// DenseStats 的计数应与实际状态一致。
func TestDenseStatsCounts(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	mm := &fakeMM{dim: 2, fp: "fp", loaded: true,
		text: map[string][]float64{"__default": {1, 0}},
		img:  map[string][]float64{"__default": {1, 0}},
	}
	s.SetDenseSpace(mm)
	if err := s.Add("a", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("b", "B"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.items["b"].DenseFP = "stale"
	s.mu.Unlock()

	st := s.DenseStats()
	if st["ready"] != 1 || st["stale"] != 1 {
		t.Errorf("DenseStats 应为 ready=1 stale=1，实为 %v", st)
	}
	if st["dim"] != 2 || st["fingerprint"] != "fp" {
		t.Errorf("DenseStats 维度/指纹不符：%v", st)
	}
}

func namesOf(ks []*Knowledge) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.Name
	}
	return out
}
