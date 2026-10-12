package knowledge

import (
	"os"
	"path/filepath"
	"testing"
)

// 稠密向量缓存：第二次启动不得重算。
func TestDenseCacheAvoidsRecompute(t *testing.T) {
	dir := t.TempDir()
	mm := &fakeMM{dim: 3, fp: "fp-x", loaded: true,
		text: map[string][]float64{"__default": {1, 1, 0}},
		img:  map[string][]float64{"__default": {0, 1, 1}},
	}

	s := NewStore(dir)
	_ = s.Start()
	s.SetDenseSpace(mm)
	for _, n := range []string{"a", "b", "c"} {
		_ = s.Add(n, "正文"+n)
	}
	if built, _ := s.ReindexDense(); built != 0 {
		t.Fatalf("Add 已算过，首次 Reindex 应为 0，实为 %d", built)
	}
	fi, err := os.Stat(filepath.Join(dir, ".dense.json"))
	if err != nil {
		t.Fatalf("稠密缓存未落盘: %v", err)
	}
	t.Logf(".dense.json = %d 字节", fi.Size())
	s.Stop()

	// 重启：条目 + 缓存都在，Reindex 不该新建任何向量
	s2 := NewStore(dir)
	_ = s2.Start()
	s2.SetDenseSpace(mm)
	for _, id := range s2.List() {
		if len(s2.items[id].Dense) == 0 {
			t.Fatalf("重启后 %s 未从缓存恢复稠密向量", id)
		}
	}
	if built, _ := s2.ReindexDense(); built != 0 {
		t.Errorf("重启后应命中缓存（built 应为 0），实为 %d", built)
	}
	s2.Stop()
}

// 换模型/维度后缓存必须整体作废并重算。
func TestDenseCacheInvalidatedOnModelChange(t *testing.T) {
	dir := t.TempDir()
	old := &fakeMM{dim: 3, fp: "fp-old", loaded: true,
		text: map[string][]float64{"__default": {1, 1, 0}},
		img:  map[string][]float64{"__default": {0, 1, 1}},
	}
	s := NewStore(dir)
	_ = s.Start()
	s.SetDenseSpace(old)
	_ = s.Add("a", "A")
	s.Stop()

	// 新空间：不同 fp 与维度
	newMM := &fakeMM{dim: 5, fp: "fp-new", loaded: true,
		text: map[string][]float64{"__default": {1, 1, 1, 0, 0}},
		img:  map[string][]float64{"__default": {0, 1, 1, 0, 0}},
	}
	s2 := NewStore(dir)
	_ = s2.Start()
	s2.SetDenseSpace(newMM)
	defer s2.Stop()
	if len(s2.items["a"].Dense) != 0 {
		t.Error("旧空间的缓存不得被新空间采用")
	}
	if built, _ := s2.ReindexDense(); built != 1 {
		t.Errorf("换空间后应重算 1 条，实为 %d", built)
	}
	if len(s2.items["a"].Dense) != 5 {
		t.Errorf("重算后应为 5 维，实为 %d", len(s2.items["a"].Dense))
	}
}

// 媒体引用必须跨重启存活（此前只在内存，重启即静默丢失）。
func TestMediaRefsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Start()
	if err := s.AddWithMedia("cat", "猫的图", []KnowledgeMediaRef{
		{Digest: "d1", MIME: "image/png", Kind: "image"},
	}); err != nil {
		t.Fatal(err)
	}
	s.Stop()

	s2 := NewStore(dir)
	_ = s2.Start()
	defer s2.Stop()
	k := s2.items["cat"]
	if k == nil {
		t.Fatalf("条目未载入，实为 %v", s2.List())
	}
	if len(k.Media) != 1 || k.Media[0].Digest != "d1" {
		t.Fatalf("媒体引用未跨重启存活：%+v", k.Media)
	}
}

// AttachMedia 新挂的媒体也必须落盘。
func TestAttachMediaPersists(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Start()
	_ = s.Add("k", "正文")
	if err := s.AttachMedia("k", KnowledgeMediaRef{Digest: "d9", MIME: "image/jpeg"}); err != nil {
		t.Fatal(err)
	}
	s.Stop()

	s2 := NewStore(dir)
	_ = s2.Start()
	defer s2.Stop()
	if got := s2.items["k"].Media; len(got) != 1 || got[0].Digest != "d9" {
		t.Fatalf("AttachMedia 未落盘：%+v", got)
	}
}

// 无媒体的条目不应产生 .media.json 残留。
func TestNoMediaSidecarForPlainEntry(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	_ = s.Start()
	defer s.Stop()
	_ = s.Add("plain", "正文")
	if _, err := os.Stat(filepath.Join(dir, "plain", mediaSidecarName)); err == nil {
		t.Errorf("纯文本条目不该有 %s", mediaSidecarName)
	}
}
