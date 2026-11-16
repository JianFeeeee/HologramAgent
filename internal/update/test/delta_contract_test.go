package test

// internal/update 的**差分补丁**判据 —— 面向接口的黑盒。
//
// 入口：MakeDelta → ApplyDelta（内存）/ MakeDeltaFile → ApplyDeltaFile（文件）。
// 断言只看「读回来的字节是否等于新文件」与「坏输入是否被拒」。
//
// 原 `semver_contract_test.go` 全是纯函数（ParseSemver / CmpSemver / Newer…），
// **差分本体零覆盖** —— 而它是 delta OTA 的核心：这两个函数一旦语义漂移，
// 症状是「下载了几 MB 补丁、装上去是坏的二进制」，而版本号比较全绿。

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// TestDeltaRoundTrip：生成补丁再重放，还原结果必须等于新文件。
//
// 覆盖四种内容形态：完全相同（全 COPY）、小改动、完全不重叠（全 LITERAL）、
// 随机二进制。随机内容比文本更能暴露 COPY/LITERAL 的边界错误。
func TestDeltaRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		old  []byte
		new  []byte
	}{
		{"相同", bytes.Repeat([]byte("same-block-"), 8192), nil},
		{"小改动", nil, nil}, // 下面单独构造
		{"完全不重叠", nil, nil},
		{"随机二进制", nil, nil},
	}

	base := bytes.Repeat([]byte("base-content-"), 8192)
	cases[0].new = cases[0].old

	smallOld := bytes.Repeat([]byte("A"), 128*1024)
	smallNew := append([]byte(nil), smallOld...)
	copy(smallNew[1000:1020], []byte("BBBBBBBBBBBBBBBBBBBB"))
	cases[1].old, cases[1].new = smallOld, smallNew

	cases[2].old = bytes.Repeat([]byte("X"), 64*1024)
	cases[2].new = bytes.Repeat([]byte("Y"), 64*1024)

	rng := rand.New(rand.NewSource(42))
	randOld := make([]byte, 256*1024)
	rng.Read(randOld)
	randNew := append([]byte(nil), randOld...)
	for _, at := range []int{100, 100000, 200000} {
		rng.Read(randNew[at : at+64])
	}
	cases[3].old, cases[3].new = randOld, randNew

	_ = base

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			patch, hdr, err := update.MakeDelta(c.old, c.new)
			if err != nil {
				t.Fatalf("MakeDelta: %v", err)
			}
			if hdr.NewSize != int64(len(c.new)) {
				t.Fatalf("★ header.NewSize=%d，期望 %d", hdr.NewSize, len(c.new))
			}
			got, err := update.ApplyDelta(c.old, patch)
			if err != nil {
				t.Fatalf("ApplyDelta: %v", err)
			}
			if !bytes.Equal(got, c.new) {
				t.Fatalf("★ 往返不一致（%d vs %d 字节）", len(got), len(c.new))
			}
		})
	}
}

// TestApplyDeltaRejectsWrongBaseline：用别的版本打出来的补丁，在错误基线上
// 必须报错而不是产出二进制。
//
// 这是差分 OTA 最危险的一类错误：客户端版本不对、补丁也下错，拼出来的东西
// "能写盘、启动不了"，而版本号检查可能已经过了。
func TestApplyDeltaRejectsWrongBaseline(t *testing.T) {
	base := bytes.Repeat([]byte("base-1.4.0 "), 4096)
	other := bytes.Repeat([]byte("base-1.3.1 "), 4096) // 必须等长，否则大小检查先拦
	if len(base) != len(other) {
		t.Fatalf("判据自设前提：两条基线必须等长")
	}
	target := append([]byte(nil), base...)
	copy(target[100:120], []byte("patch-applied-here!!"))

	patch, _, err := update.MakeDelta(base, target)
	if err != nil {
		t.Fatalf("MakeDelta: %v", err)
	}
	if _, err := update.ApplyDelta(other, patch); err == nil {
		t.Fatal("★ 错误基线上重放成功了 —— 会拼出一个坏二进制")
	}
}

// TestApplyDeltaRejectsCorruptPatch：补丁是不可信输入（从网络下载），
// 解析必须要么完整成功、要么明确失败 —— 半成品二进制写进磁盘，
// 症状就是"升级后启动不了"。
//
// ★ 本条只钉「坏输入一定被拒」这个可观察后果，不钉「由哪一道闸拒绝」。
// 实测：单独去掉 OldSHA256 校验、或单独去掉结果哈希校验，本判据都**不红** ——
// 因为两道闸互为兜底（错基线拼出来的内容不对，会被结果哈希拦住；
// 结果哈希去掉后，结构/大小检查仍然拒绝坏补丁）。那是纵深防御的正常结果，
// 要为区分它们造一个"只破坏载荷字节"的用例，需要在判据里维护一个补丁
// 格式的迷你解析器 —— 那属于钉实现细节，不值得。
func TestApplyDeltaRejectsCorruptPatch(t *testing.T) {
	base := bytes.Repeat([]byte("payload "), 8192)
	target := append([]byte(nil), base...)
	copy(target[50:80], []byte("0123456789012345678901234567890"))

	patch, _, err := update.MakeDelta(base, target)
	if err != nil {
		t.Fatalf("MakeDelta: %v", err)
	}

	for _, c := range []struct {
		name  string
		patch []byte
	}{
		{"截断一半", patch[:len(patch)/2]},
		{"尾部翻转", flipLast(patch)},
		{"不是补丁", []byte("not a patch at all")},
		{"空", nil},
	} {
		if _, err := update.ApplyDelta(base, c.patch); err == nil {
			t.Fatalf("★ %s 的补丁被接受了", c.name)
		}
	}
}

// TestDeltaFileRoundTrip：文件接口（生产路径 `TryDeltaUpgrade` 走它）往返一致，
// 且报告的写入字节数等于新文件大小。
func TestDeltaFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.bin")
	newPath := filepath.Join(dir, "new.bin")
	patchPath := filepath.Join(dir, "patch.delta")
	outPath := filepath.Join(dir, "out.bin")

	old := bytes.Repeat([]byte("old-binary-content "), 4096)
	newData := append([]byte(nil), old...)
	copy(newData[5000:5100], bytes.Repeat([]byte("#"), 100))

	for _, w := range []struct {
		p string
		d []byte
	}{{oldPath, old}, {newPath, newData}} {
		if err := os.WriteFile(w.p, w.d, 0644); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := update.MakeDeltaFile(oldPath, newPath, patchPath); err != nil {
		t.Fatalf("MakeDeltaFile: %v", err)
	}
	n, err := update.ApplyDeltaFile(oldPath, patchPath, outPath)
	if err != nil {
		t.Fatalf("ApplyDeltaFile: %v", err)
	}
	if n != int64(len(newData)) {
		t.Fatalf("★ 报告写入 %d 字节，期望 %d", n, len(newData))
	}
	written, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, newData) {
		t.Fatal("★ 落盘内容与新文件不一致")
	}
}

func flipLast(p []byte) []byte {
	out := append([]byte(nil), p...)
	out[len(out)-1] ^= 0xFF
	return out
}
