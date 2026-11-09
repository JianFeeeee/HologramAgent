package test

// internal/kernelboot 的黑盒判据：EncodeGraph → DecodeGraph / GraphDigest /
// CheckAgainstBinary 的输入 → 输出。不碰内部 payload 布局。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
)

var edges = []kernelboot.GraphEdge{
	{Name: "security", Provides: []string{"sec"}, Requires: nil, Layer: 1},
	{Name: "agent", Provides: []string{"agent"}, Requires: []string{"sec"}, Layer: 2},
}

// TestGraphEncodeDecodeRoundTrip：编码后能解回同样模块与层号。
func TestGraphEncodeDecodeRoundTrip(t *testing.T) {
	data := kernelboot.EncodeGraph(edges)
	gf, err := kernelboot.DecodeGraph(data)
	if err != nil {
		t.Fatalf("DecodeGraph: %v", err)
	}
	if gf == nil || len(gf.Modules) != len(edges) {
		t.Fatalf("★ 模块数不符：%+v", gf)
	}
	for i, m := range gf.Modules {
		if m.Name != edges[i].Name || m.Layer != edges[i].Layer {
			t.Fatalf("★ 第 %d 个模块不符：%+v", i, m)
		}
	}
}

// TestGraphDigestIsDeterministic：同一份声明 digest 稳定；换声明则变。
func TestGraphDigestIsDeterministic(t *testing.T) {
	a := kernelboot.GraphDigest(edges)
	b := kernelboot.GraphDigest(edges)
	if a == "" || a != b {
		t.Fatalf("★ digest 不稳定：%q vs %q", a, b)
	}
	other := []kernelboot.GraphEdge{{Name: "x", Provides: []string{"y"}}}
	if c := kernelboot.GraphDigest(other); c == a {
		t.Fatal("★ 不同声明得到同一 digest")
	}
}

// TestDecodeGraphRejectsCorruption：位翻转 / 截断必须报错。
func TestDecodeGraphRejectsCorruption(t *testing.T) {
	data := kernelboot.EncodeGraph(edges)
	// 只翻转**数据区末尾**的字节（不动 magic / 长度），
	// 这样唯一能抓到它的就是 hash 自校验。翻 magic 的话即使
	// 跳过 hash 也会因格式标识报错，判据会假绿。
	flipped := append([]byte(nil), data...)
	pos := len(flipped) - 32 - 1 // 尾部 hash 之前最后一个 payload 字节
	flipped[pos] ^= 0x01
	if _, err := kernelboot.DecodeGraph(flipped); err == nil {
		t.Fatal("★ 数据区位翻转的图被接受（hash 自校验失效）")
	}
	if _, err := kernelboot.DecodeGraph(data[:len(data)/2]); err == nil {
		t.Fatal("★ 截断的图被接受")
	}
}

// TestCheckAgainstBinary：空内嵌 hash 放行；不匹配报错；匹配通过。
func TestCheckAgainstBinary(t *testing.T) {
	gf, err := kernelboot.DecodeGraph(kernelboot.EncodeGraph(edges))
	if err != nil {
		t.Fatal(err)
	}
	if err := kernelboot.CheckAgainstBinary(gf, ""); err != nil {
		t.Fatalf("★ dev 环境（空 hash）被拒：%v", err)
	}
	if err := kernelboot.CheckAgainstBinary(gf, "deadbeef"); err == nil {
		t.Fatal("★ 不配套的 hash 被放行")
	}
	if err := kernelboot.CheckAgainstBinary(gf, gf.Digest); err != nil {
		t.Fatalf("★ 配套图被拒：%v", err)
	}
}

// TestFuncModuleExposesSpec：FuncModule 对外暴露 Name/Provides/Requires。
func TestFuncModuleExposesSpec(t *testing.T) {
	m := kernelboot.FuncModule(kernelboot.FuncSpec{
		Name: "contract", Provides: []string{"a"}, Requires: []string{"b"},
	})
	if m.Name() != "contract" {
		t.Fatalf("★ Name = %q", m.Name())
	}
	if len(m.Provides()) != 1 || m.Provides()[0] != "a" {
		t.Fatalf("★ Provides = %v", m.Provides())
	}
	if len(m.Requires()) != 1 || m.Requires()[0] != "b" {
		t.Fatalf("★ Requires = %v", m.Requires())
	}
}
