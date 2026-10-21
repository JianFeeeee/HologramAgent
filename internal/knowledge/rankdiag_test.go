package knowledge

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 知识库检索质量判据（**退化路径**：稀疏两路，无多模态空间）。
//
//	KB_DIAG=1                              → 跑并打印指标
//	KB_DIAG=1 KB_DIAG_ASSERT=1             → 额外断言门槛（CI/回归用）
//	KB_DIAG_ROOT / KB_DIAG_MODELS          → 覆盖数据与词向量路径
//
// # 这条判据现在测的是什么（2026-10-08 改）
//
// 主路径已改为**纯稠密向量检索**（见 knowledge.go 的 Search 注释），
// 质量判据在 dense_recall_quality_test.go。本诊断不注入多模态空间，
// 因此它走的是**退化路径**——即把 core.memory.multimodal_space.provider
// 留空的部署所用的稀疏两路。
//
// 保留它的理由：退化路径仍要能检索（否则那些部署直接变成知识库不可用），
// 而这条路的质量历史上就没好过——实测（生产 KB 37 条）自检索 top-1 仅 24%。
// 把它量出来，是为了不把「主路修好了」误当成「两路都好了」。
//
// # 门槛的来源
//
// 注释里的历史实测（仅稠密路 top-1 15% / MRR 0.271；三路融合 21% / 0.376）
// 做在**33 条 KB + 稀疏 fastText** 上，而那次「融合更优」的结论已被
// 纯稠密路推翻（换成 chineseclip 后稠密路全库排名 1/192）。
// 下面两个门槛沿用至今，只用于挡退化路径自己变差，**不代表主路径的水平**。
func TestRankingQualityOnRealKB(t *testing.T) {
	if os.Getenv("KB_DIAG") == "" {
		t.Skip("需要 KB_DIAG=1（真实 KB + 词向量文件）")
	}
	srcRoot := envOr("KB_DIAG_ROOT", envOr("KB_DIAG_ROOT", "/home/newqqagent/knowledge"))
	models := envOr("KB_DIAG_MODELS", "/data/cc.zh.top200k.vec,/data/cc.en.top200k.vec")
	emb := memory.NewStaticEmbedder(strings.Split(models, ",")...)

	// 拷贝到临时目录跑：Start() 会重写 .index.json，不能动线上数据
	tmp := t.TempDir()
	entries, err := os.ReadDir(srcRoot)
	if err != nil {
		t.Fatalf("读取 %s: %v", srcRoot, err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		src := filepath.Join(srcRoot, e.Name(), "content.md")
		in, err := os.Open(src)
		if err != nil {
			continue
		}
		dst := filepath.Join(tmp, e.Name(), "content.md")
		os.MkdirAll(filepath.Dir(dst), 0755)
		out, _ := os.Create(dst)
		io.Copy(out, in)
		out.Close()
		in.Close()
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		t.Fatal("没有可用的知识条目")
	}

	st := NewStore(tmp)
	st.SetVectorizer(emb)
	if err := st.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	top1, mrr, missed := 0, 0.0, []string{}
	for _, name := range names {
		// 取全量排名：MRR 的定义用到真实名次，只取 top-2 会把 rank>2 的全都记 0
		// （我第一版就是这么写的，把 0.376 误报成 0.197）
		hits := st.Search(name, len(names))
		if len(hits) == 0 {
			missed = append(missed, name+"(无结果)")
			continue
		}
		if hits[0].Name == name {
			top1++
		} else {
			missed = append(missed, fmt.Sprintf("%s→%s", name, hits[0].Name))
		}
		for i, h := range hits {
			if h.Name == name {
				mrr += 1.0 / float64(i+1)
				break
			}
		}
	}
	n := float64(len(names))
	rate := 100 * float64(top1) / n
	fmt.Printf("\n  === 知识库检索质量（%d 条，自检索判据）===\n", len(names))
	fmt.Printf("  top-1 %d/%d = %.0f%%   MRR %.3f\n", top1, len(names), rate, mrr/n)
	if len(missed) > 0 {
		fmt.Printf("  未命中 top-1（前 10）：%v\n", firstN(missed, 10))
	}
	for _, q := range []string{"最近更新", "首启人格门禁", "插件怎么开发和部署", "统一多模态向量空间 ONNX", "隐私政策"} {
		hits := st.Search(q, 2)
		got := []string{}
		for _, h := range hits {
			got = append(got, h.Name)
		}
		fmt.Printf("  查询「%s」→ %v\n", q, got)
	}

	if os.Getenv("KB_DIAG_SWEEP") != "" {
		fmt.Printf("\n  === 稀疏融合权重扫描（1.0 = 只用语义路，0.0 = 只用词法路）===\n")
		fmt.Printf("  （无多模态稠密路接入时，本扫描直接对应历史 densePathWeight 的语义）\n")
		saved := sparseSemWeight
		for _, w := range []float64{1.0, 0.8, 0.7, 0.5, 0.3, 0.0} {
			sparseSemWeight = w
			t1, m := 0, 0.0
			for _, name := range names {
				hits := st.Search(name, len(names))
				for i, h := range hits {
					if h.Name == name {
						if i == 0 {
							t1++
						}
						m += 1.0 / float64(i+1)
						break
					}
				}
			}
			fmt.Printf("  权重 %.1f：top-1 %2d/%d = %3.0f%%   MRR %.3f\n",
				w, t1, len(names), 100*float64(t1)/float64(len(names)), m/float64(len(names)))
		}
		sparseSemWeight = saved
	}

	if os.Getenv("KB_DIAG_ASSERT") != "" {
		// 门槛只针对**退化路径**：实测生产 KB 37 条上 MRR 0.358 / top-1 24%。
		// 这两个数都远低于主路径（纯稠密路全库排名 1/192），别拿它们
		// 当作知识库的检索水平。
		if mrr/n < 0.34 {
			t.Fatalf("退化路径检索质量又变差了：MRR %.3f < 0.34（实测 0.358）", mrr/n)
		}
		if rate < 18 {
			t.Fatalf("退化路径检索质量又变差了：top-1 %.0f%% < 18%%（实测 24%%）", rate)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
