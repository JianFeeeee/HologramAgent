package update

// 差分 OTA 的判据。
//
// 这一层的收益是**实测的**：改一行逻辑，88MB 的二进制只差 80 字节。
// 但机制本身有真实风险 —— 错误应用会静默产出一份"看起来正常"的坏二进制。
// 所以判据的重点是「错了一定被咬住」，而不是「正常情况能跑」。

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// ① 往返：make → apply 得到逐字节相同的内容。
func TestDeltaRoundTrip(t *testing.T) {
	old := bytes.Repeat([]byte("ABCDEFGHIJKLMNOP"), 5000) // 80KB
	newData := append([]byte(nil), old...)
	// 中间插入 3 个字节（模拟改代码导致后续偏移全变）
	mid := len(newData) / 2
	newData = append(newData[:mid], append([]byte("XYZ"), newData[mid:]...)...)

	patch, hdr, err := MakeDelta(old, newData)
	if err != nil {
		t.Fatalf("生成补丁: %v", err)
	}
	if hdr.NewSize != int64(len(newData)) || hdr.OldSize != int64(len(old)) {
		t.Fatalf("头部大小不对: %+v", hdr)
	}

	got, err := ApplyDelta(old, patch)
	if err != nil {
		t.Fatalf("应用补丁: %v", err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatal("★ 重建内容与目标不一致")
	}
}

// ② 关键收益：小改动 ⇒ 小补丁。
//
// 这条判据钉住的是**压缩比**本身。若实现退化成"全字面量"，
// 往返仍能通过但 OTA 一点没省 —— 那是最容易漏掉的失败形态。
func TestDeltaSmallChangeProducesSmallPatch(t *testing.T) {
	old := make([]byte, 1<<20) // 1MB
	if _, err := rand.Read(old); err != nil {
		t.Fatal(err)
	}
	newData := append([]byte(nil), old...)
	// 只改 64 字节
	for i := 0; i < 64; i++ {
		newData[i*100] ^= 0xFF
	}

	patch, hdr, err := MakeDelta(old, newData)
	if err != nil {
		t.Fatal(err)
	}
	st := hdr.Stats(int64(len(patch)))
	t.Logf("patch=%d bytes, ratio=%.4f", len(patch), st.Ratio)

	// 1MB 随机内容，只有 64 字节变化 ⇒ 补丁应远小于原文件。
	// 这里用 5% 作宽松上限（块粒度 4KB，改动分散在 100 字节间隔上，
	// 会打断一些块，实际比例低于此）。
	if st.Ratio > 0.05 {
		t.Errorf("★ 补丁过大（%.2f%% of %d bytes）：差分没起作用", st.Ratio*100, hdr.NewSize)
	}
}

// ③ 基线不符必须拒绝（用错旧版本 ⇒ 产出垃圾）。
func TestDeltaRejectsWrongBaseline(t *testing.T) {
	old := []byte("the-original-content-here")
	newData := []byte("the-original-content-here-plus-more")
	patch, _, err := MakeDelta(old, newData)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDelta([]byte("a-completely-different-baseline"), patch); err == nil {
		t.Fatal("★ 基线不符时必须拒绝（否则静默产出垃圾）")
	}
}

// ④ 补丁被截断必须拒绝，不得"部分成功"。
func TestDeltaRejectsTruncatedPatch(t *testing.T) {
	old := bytes.Repeat([]byte("Z"), 8192)
	newData := append([]byte(nil), old...)
	newData = append(newData, []byte("TAIL")...)
	patch, _, err := MakeDelta(old, newData)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{len(deltaMagic), 20, len(patch) / 2, len(patch) - 1} {
		if _, err := ApplyDelta(old, patch[:cut]); err == nil {
			t.Errorf("★ 截断到 %d/%d 字节仍被接受", cut, len(patch))
		}
	}
}

// ⑤ 内容被篡改必须被 sha256 咬住。
//
// 这是最后一道防线：patch 结构合法、指令也不越界，但产出内容错。
func TestDeltaRejectsCorruptedOutput(t *testing.T) {
	old := bytes.Repeat([]byte("Q"), 4096)
	newData := append([]byte(nil), old...)
	newData[0] = 'X'
	patch, hdr, err := MakeDelta(old, newData)
	if err != nil {
		t.Fatal(err)
	}
	// 篡改头部里的 NewSHA256（模拟恶意/损坏的补丁）
	b := append([]byte(nil), patch...)
	off := len(deltaMagic) + 16 + 64
	b[off] ^= 0xFF
	if _, err := ApplyDelta(old, b); err == nil {
		t.Fatal("★ 内容哈希不符时必须拒绝")
	}
	_ = hdr
}

// ⑥ 空文件与超小文件不崩。
func TestDeltaEdgeCases(t *testing.T) {
	cases := [][2][]byte{
		{nil, nil},
		{[]byte("a"), []byte("b")},
		{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("a"), 100)},
		{[]byte{}, []byte("new")},
		{[]byte("old"), []byte{}},
	}
	for i, c := range cases {
		patch, _, err := MakeDelta(c[0], c[1])
		if err != nil {
			t.Fatalf("case %d 生成失败: %v", i, err)
		}
		got, err := ApplyDelta(c[0], patch)
		if err != nil {
			t.Fatalf("case %d 应用失败: %v", i, err)
		}
		if !bytes.Equal(got, c[1]) {
			t.Errorf("case %d 结果不符", i)
		}
	}
}

// ⑦ 文件级入口（CLI 用）。
func TestDeltaFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "homed.old")
	newPath := filepath.Join(dir, "homed.new")
	patchPath := filepath.Join(dir, "homed.patch")
	outPath := filepath.Join(dir, "homed.out")

	oldData := bytes.Repeat([]byte("0123456789abcdef"), 4096)
	newData := append([]byte(nil), oldData...)
	newData = append(newData[:100], append([]byte("INSERTED"), newData[100:]...)...)

	if err := os.WriteFile(oldPath, oldData, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, newData, 0755); err != nil {
		t.Fatal(err)
	}

	hdr, patchSize, err := MakeDeltaFile(oldPath, newPath, patchPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("patch=%d bytes (%.2f%% of new)", patchSize, hdr.Stats(patchSize).Ratio*100)

	if _, err := ApplyDeltaFile(oldPath, patchPath, outPath); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newData) {
		t.Fatal("★ 文件级往返结果不一致")
	}
}

// ⑧ 头部解析拒绝非本格式。
func TestParseDeltaHeaderRejectsGarbage(t *testing.T) {
	if _, err := ParseDeltaHeader([]byte("not a delta file at all")); err == nil {
		t.Fatal("应拒绝非补丁数据")
	}
}
