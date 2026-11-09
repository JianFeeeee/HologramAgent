package test

// internal/memory/distill 纯函数的黑盒判据：输入文本 → 输出。
// 只调导出函数，不构造 Extractor 内部状态。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/distill"
)

// TestBlockTextRoundTrip：ToMemoryBlock 产出的文本能被
// BlockSubject / Dimension / Value 完整还原。
func TestBlockTextRoundTrip(t *testing.T) {
	fb := distill.FieldBlock{Subject: "billing服务", Dimension: "端口", Value: "8499"}
	blk := fb.ToMemoryBlock("billing服务端口8499")
	if got := distill.BlockSubject(blk.Text); got != "billing服务" {
		t.Fatalf("★ BlockSubject = %q，期望 billing服务", got)
	}
	if got := distill.Dimension(blk.Text); got != "端口" {
		t.Fatalf("★ Dimension = %q，期望 端口", got)
	}
	if got := distill.Value(blk.Text); got != "8499" {
		t.Fatalf("★ Value = %q，期望 8499", got)
	}
}

// TestBlockIDDependsOnSubject：同句同维度同值、不同主语必须得到不同 ID。
//
// 真实缺陷：subject 不参与 ID 派生时，三个服务各自的「端口=8861」
// 会撞成同一个块 ⇒ 静默数据丢失（后者覆盖前者，无任何报错）。
func TestBlockIDDependsOnSubject(t *testing.T) {
	const sentence, dim, val = "各服务端口相同", "端口", "8861"
	a := distill.BlockID(sentence, "admin服务", dim, val)
	b := distill.BlockID(sentence, "billing服务", dim, val)
	if a == b {
		t.Fatal("★ 不同主语得到同一个 BlockID —— 记忆会互相覆盖")
	}
}

// TestBlockIDIsDeterministic：同输入同输出（缓存/去重的前提）。
func TestBlockIDIsDeterministic(t *testing.T) {
	x := distill.BlockID("s", "subj", "dim", "v")
	for i := 0; i < 5; i++ {
		if y := distill.BlockID("s", "subj", "dim", "v"); y != x {
			t.Fatalf("★ 第 %d 次得到 %q，期望 %q", i, y, x)
		}
	}
}

// TestDimensionValueEdgeCases：无 '=' / 空维度 / 多 '='。
func TestDimensionValueEdgeCases(t *testing.T) {
	cases := []struct{ in, dim, val string }{
		{"端口=8499", "端口", "8499"},
		{"没有等号", "", "没有等号"},
		{"=空维度", "", "空维度"},
		{"a=b=c", "a", "b=c"},
	}
	for _, c := range cases {
		if got := distill.Dimension(c.in); got != c.dim {
			t.Errorf("★ Dimension(%q)=%q，期望 %q", c.in, got, c.dim)
		}
		if got := distill.Value(c.in); got != c.val {
			t.Errorf("★ Value(%q)=%q，期望 %q", c.in, got, c.val)
		}
	}
}

// TestSentenceBlockIDIsStable：原句块 ID 由文本决定，跨调用稳定。
func TestSentenceBlockIDIsStable(t *testing.T) {
	const s = "用户偏好在晚上处理构建任务"
	a := distill.SentenceBlockID(s)
	b := distill.SentenceBlockID(s)
	if a == "" || a != b {
		t.Fatalf("★ SentenceBlockID 不稳定：%q vs %q", a, b)
	}
}

// TestNormalizeDimensionIsIdempotent：归一两次结果不变。
func TestNormalizeDimensionIsIdempotent(t *testing.T) {
	for _, in := range []string{"端口", " 端 口 ", "Port", "配置端口"} {
		once := distill.NormalizeDimension(in)
		if twice := distill.NormalizeDimension(once); twice != once {
			t.Fatalf("★ NormalizeDimension(%q) 非幂等：%q → %q", in, once, twice)
		}
	}
}
