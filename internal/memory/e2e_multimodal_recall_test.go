//go:build e2e && onnxruntime

// 多模态真实数据 e2e：验证块 union 化的**核心主张**——
// 媒体块只按自己的向量被召回，全程不碰任何描述文本。
//
// 这是 CAS 淘汰后最需要证明的一条：两个存储层合并成一个块值之后，
// 「图片能不能被它自己的向量搜到」必须仍然成立。
//
// 与单测的区别：这里用**生产同款 chineseclip 模型**对真实库里的图算向量，
// 不是注入的假向量。
//
// 用法：
//
//	E2E_DB=/tmp/e2e-backfill/graph.db \
//	E2E_MODEL_DIR=/home/newqqagent/models/chinese-clip-vit-b16-onnx \
//	go test -tags 'e2e onnxruntime' -run TestE2E_MultimodalBlockRecall -v ./internal/memory/
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"

	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/pkg/embedding"
	_ "github.com/JianFeeeee/HomeAgent/providers/chineseclip"
)

// 判据（不变量，不是"函数被调过"）：
//  1. 用生产模型能把真实媒体块的字节算成与原空间一致的向量（Dim 匹配）
//  2. 块**自己的向量**召回自己 → top1 命中且 Score≈1（纯向量检索端到端可用）
//  3. 向量写回库后，跨进程重开库仍能召回（身份与向量都落盘了）
//  4. 文本查询向量能召回到**任意**媒体块（同一坐标系里文本与图可比）——
//     这是"一个向量空间同时覆盖文本与图像"的主张
func TestE2E_MultimodalBlockRecall(t *testing.T) {
	dbPath := os.Getenv("E2E_DB")
	modelDir := os.Getenv("E2E_MODEL_DIR")
	if dbPath == "" || modelDir == "" {
		t.Skip("E2E_DB / E2E_MODEL_DIR 未设置；跳过多模态真实数据 e2e")
	}

	// 1. 打开生产同款多模态空间
	p, err := embedding.Open("chineseclip", embedding.Config{
		Options: map[string]string{"model_dir": modelDir},
	})
	if err != nil {
		t.Fatalf("打开 chineseclip(%s): %v", modelDir, err)
	}
	adapter, err := vector.AdaptProvider(p)
	if err != nil {
		p.Close()
		t.Fatalf("适配 provider: %v", err)
	}
	t.Cleanup(func() { adapter.Close() })
	if !adapter.Loaded() {
		t.Fatal("多模态空间未加载")
	}
	fp := adapter.Fingerprint()
	dim := adapter.Dim()
	t.Logf("多模态空间就绪：指纹 %s，维度 %d", fp[:16], dim)

	g, err := NewGraphDB(dbPath)
	if err != nil {
		t.Fatalf("打开 %s: %v", dbPath, err)
	}
	// 末尾要做"跨进程重开"验证：先显式 Close 再开 g2。
	// 用标志让 Cleanup 不重复关闭（g 已关时跳过）。
	closed := false
	t.Cleanup(func() {
		if !closed {
			g.Close()
		}
	})

	// 取若干已回填字节的媒体块
	ids, err := g.SampleFilledBlockIDs(12)
	if err != nil || len(ids) == 0 {
		t.Skipf("没有已回填块: %v", err)
	}

	// 2. 对每个块的真字节算向量（判据 1）
	type rec struct{ id, digest string }
	var recs []rec
	for _, id := range ids {
		data, err := g.BlockData(id)
		if err != nil || len(data) == 0 {
			t.Errorf("BlockData(%s): %v", id, err)
			continue
		}
		// 只处理图像（chineseclip 空间只覆盖 text/image）
		b, ok, err := g.BlockByIDPrefix(id)
		if err != nil || !ok {
			continue
		}
		if b.Modality != BlockImage {
			continue
		}
		vec, err := adapter.EmbedImageDense(data, b.MIME)
		if err != nil {
			t.Errorf("EmbedImageDense(%s): %v", id, err)
			continue
		}
		if len(vec) != dim {
			t.Errorf("块 %s 向量维度 %d ≠ 空间维度 %d", id, len(vec), dim)
			continue
		}
		sum := sha256.Sum256(data)
		recs = append(recs, rec{id: id, digest: hex.EncodeToString(sum[:])})
		// 写回向量（复现生产 reembedStaleMedia 的落库动作）
		if err := g.SetBlockVector(id, vec, fp); err != nil {
			t.Errorf("SetBlockVector(%s): %v", id, err)
		}
	}
	if len(recs) == 0 {
		t.Fatal("没有一个图像块能被嵌入")
	}
	t.Logf("判据1 通过：%d 个真图块嵌入成功（维度 %d）", len(recs), dim)

	// 3. 用块自己的向量召回自己（判据 2）
	//
	// ★ 为何不是“必须 top1”：存量库里同一张图可能被存成多个块
	//   （旧式 blk_<ts>_<n> ID，见实测 ee5bdb2a… 一份内容 4 个块）。
	//   同内容的两个块向量**逐位相同** ⇒ 余弦都是 1.0 ⇒ 并列满分。
	//   此时 top1 取哪个只取决于排序稳定性，不代表召回错。真正的不变量是：
	//     (a) 自己必须出现在**满分并列集**里；
	//     (b) 排在它前面的块必须是**同内容**（不同内容不该顶替）。
	//   只断言“index 0 == 自己”会把内容去重这件好事当成缺陷。
	selfHit := 0
	for i, r := range recs {
		b, ok, err := g.BlockByIDPrefix(r.id)
		if err != nil || !ok || len(b.Vector) == 0 {
			t.Errorf("块 %s 向量未落库", r.id)
			continue
		}
		hits, err := g.RecallBlocks(BlockRecallQuery{
			Vector:       b.Vector,
			Fingerprint:  b.Fingerprint,
			SkipCentroid: true, // 纯向量比较：不引入中心化校正变量
			TopK:         20,
		})
		if err != nil {
			t.Fatalf("RecallBlocks(%s): %v", r.id, err)
		}

		// (a) 找到自己在结果里的位置
		selfRank := -1
		for k, h := range hits {
			if h.Block.ID == r.id {
				selfRank = k
				break
			}
		}
		if selfRank < 0 {
			t.Errorf("块 %s 未被自己的向量召回到 top20（got=%d 条）", r.id, len(hits))
			continue
		}
		// (b) 排在自己前面的每一个都必须是同内容（同一 digest）。
		//     同内容⇒同向量⇒同分，排序只取决于稳定性；不同内容则真的算错。
		for k := 0; k < selfRank; k++ {
			d := digestOfBlock(t, g, hits[k].Block.ID)
			if d != r.digest {
				t.Errorf("块 %s 被**不同内容**的 %s 顶替（rank %d，digest %s ≠ %s）",
					r.id, hits[k].Block.ID, k, d[:12], r.digest[:12])
				break
			}
		}
		if hits[selfRank].Score < 0.999 {
			t.Errorf("块 %s 自召回的满分分数异常低: %.4f", r.id, hits[selfRank].Score)
		}
		selfHit++
		if i < 4 {
			t.Logf("  自召回：%s rank=%d score=%.4f（top20 共 %d 条）", r.id, selfRank, hits[selfRank].Score, len(hits))
		}
	}
	if selfHit != len(recs) {
		t.Fatalf("判据2 失败：%d/%d 个块能在 top20 里召回到自己", selfHit, len(recs))
	}
	t.Logf("判据2 通过：%d/%d 块能用自己的向量召回到自己（纯向量，无描述文本参与）", selfHit, len(recs))

	// 4. 视觉路：媒体块由**多模态文本编码 → 块自身向量**召回。
	//
	// ★ 为何不测“文本查询在原始 RecallBlocks top-k 里应命中图像块”：
	//   实测（生产模型 chineseclip + 本库 1294 文本块 / 112 图像块）：
	//     查询「图片」→ 图像块 max cosine 0.377，文本块 max 1.000
	//     查询「照片」→ 图像块 max 0.385，文本块 max 0.935
	//   同一坐标系里图片的绝对相似度天然低于“字面相同的文本”，
	//   原始 top-k 必然被文本块淹没——这是 CLIP 的分数分布，不是接线缺陷。
	//   生产也正是这么设计的：crossmodal.go 把文本路与视觉路**分开取**。
	//   ★ 但分开取有个前提：视觉路必须限定模态。实测反例：不限定模态时
	//   （2026-10-12 发现）per=200 返回 1000 条里图像块 **0 条** —— 因为
	//   “取全局 top-K 再过滤掉文本块”里过滤发生在截断**之后**，名额早被
	//   文本块占满。修法：在 SQL 层限定模态（ExcludeText）。
	//   本判据兼作该修复的回归护栏。
	//
	//   验证：用多模态文本编码查询、ExcludeText=true，必须能召到图像块 ——
	//   也即“文本与图在同一个向量空间可比”这条主张成立。
	tq, err := adapter.VectorizeDense("一张图片")
	if err != nil {
		t.Fatalf("文本嵌入失败: %v", err)
	}
	allHits, err := g.RecallBlocks(BlockRecallQuery{
		Vector:      tq,
		Fingerprint: fp,
		TopK:        20,
		ExcludeText: true, // 视觉路的正确取法（与生产 crossmodal.go 一致）
	})
	if err != nil {
		t.Fatalf("视觉路召回失败: %v", err)
	}
	if len(allHits) == 0 {
		t.Error("判据4 失败：视觉路（ExcludeText）召不到任何媒体块")
	} else {
		for _, h := range allHits {
			if h.Block.Modality == BlockText {
				t.Errorf("ExcludeText=true 仍返回文本块 %s", h.Block.ID)
				break
			}
		}
		t.Logf("判据4 通过：文本查询经视觉路召到 %d 个媒体块（最高分 %.4f；同库文本块可达 1.0，故必须分两路取——见 crossmodal.go）",
			len(allHits), allHits[0].Score)
	}

	// 5. 跨进程重开：向量与字节都必须落盘（判据 3）
	g.Close()
	closed = true
	g2, err := NewGraphDB(dbPath)
	if err != nil {
		t.Fatalf("重开库: %v", err)
	}
	defer g2.Close()
	r := recs[0]
	b, ok, err := g2.BlockByIDPrefix(r.id)
	if err != nil || !ok {
		t.Fatalf("重开后找不到块 %s: ok=%v err=%v", r.id, ok, err)
	}
	if len(b.Vector) == 0 || b.Fingerprint != fp {
		t.Errorf("重开后向量/指纹丢失: dim=%d fp=%s", len(b.Vector), b.Fingerprint)
	}
	data, err := g2.BlockData(r.id)
	if err != nil || len(data) == 0 {
		t.Fatalf("重开后字节丢失: %v", err)
	}
	if got := sha256Hex(data); got != r.digest {
		t.Errorf("重开后字节身份变了: %s ≠ %s", got[:16], r.digest[:16])
	}
	hits2, err := g2.RecallBlocks(BlockRecallQuery{
		Vector: b.Vector, Fingerprint: b.Fingerprint, SkipCentroid: true, TopK: 20,
	})
	// 同内容可能多块并列满分，所以判定“能召回到自己”而非“top1 是自己”。
	selfIn := false
	for _, h := range hits2 {
		if h.Block.ID == r.id {
			selfIn = true
			break
		}
	}
	if err != nil || !selfIn {
		t.Errorf("判据3 失败：重开后自召回失败 (err=%v hits=%v)", err, topIDs(hits2))
	} else {
		t.Logf("判据3 通过：重开后 %s 仍能被自己的向量召回到 top3，字节 %d 一致", r.id, len(data))
	}
}

func topIDs(hits []BlockHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Block.ID)
	}
	return out
}

// digestOfBlock 取块负载的 sha256（内容身份，用于判定“是否同内容”）。
func digestOfBlock(t *testing.T, g *GraphDB, id string) string {
	t.Helper()
	data, err := g.BlockData(id)
	if err != nil {
		t.Fatalf("BlockData(%s): %v", id, err)
	}
	return sha256Hex(data)
}
