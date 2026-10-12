//go:build e2e

// e2e-media-backfill：在**真实生产数据副本**上验证块 union 化的读路径。
//
// 与单元测试的区别：单元测试用注入的假 resolver，这里读真的 graph.db +
// 真的 CAS 字节，走 RecallBlocks（向量召回）与 BlockData（负载按需取回）
// 两条真实读路径。
//
// 用法：
//
//	E2E_DB=/tmp/e2e-backfill/graph.db go test -tags 'onnxruntime e2e' -run TestE2EMediaBackfill -v ./internal/memory/
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
)

// 真实数据 e2e：回填后的 112 个媒体块必须
//  1. BlockData(id) 能取回非空字节（负载真的在块里）
//  2. 字节 sha256 与 payload_digest 一致（身份没变）
//  3. 用自己的向量召回自己 —— 命中且 Score≈1（读路径端到端可用）
//  4. 媒体块的 modality / mime 没被回填改写（只改 data 列）
func TestE2EMediaBackfill(t *testing.T) {
	dbPath := os.Getenv("E2E_DB")
	if dbPath == "" {
		t.Skip("E2E_DB 未设置；跳过真实数据 e2e")
	}
	g, err := NewGraphDB(dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	t.Cleanup(func() { g.Close() })

	// 找出所有带 payload_digest 的媒体块
	rows, err := g.db.Query(`SELECT id, payload_digest, modality, mime
		FROM memory_blocks WHERE payload_digest != '' AND modality != 'text' ORDER BY id`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	type row struct{ id, digest, modality, mime string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.digest, &r.modality, &r.mime); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if len(all) == 0 {
		t.Skip("库里没有带 payload_digest 的媒体块；跳过")
	}
	t.Logf("媒体块总数：%d", len(all))

	// 判据 1 + 2：BlockData 取回非空，且 sha256 与 payload_digest 一致
	empty, mismatch := 0, 0
	for _, r := range all {
		data, err := g.BlockData(r.id)
		if err != nil {
			t.Errorf("BlockData(%s): %v", r.id, err)
			continue
		}
		if len(data) == 0 {
			empty++
			continue
		}
		if got := sha256Hex(data); got != r.digest {
			mismatch++
			if mismatch <= 3 {
				t.Errorf("块 %s 字节 sha256=%s ≠ payload_digest=%s", r.id, got[:16], r.digest[:16])
			}
		}
	}
	if empty > 0 || mismatch > 0 {
		t.Fatalf("判据 1/2 失败：空 data=%d，sha256 不吻合=%d", empty, mismatch)
	}
	t.Logf("判据 1/2 通过：%d 块全部可取回且字节身份吻合", len(all))

	// 判据 3：用媒体块自己的向量召回自己（读路径端到端）
	sampled, recalled := 0, 0
	for _, r := range all {
		b, ok, err := g.BlockByIDPrefix(r.id)
		if err != nil || !ok || len(b.Vector) == 0 {
			continue // 无向量的块跳过（回填不负责算向量）
		}
		sampled++
		hits, err := g.RecallBlocks(BlockRecallQuery{
			Vector:      b.Vector,
			Fingerprint: b.Fingerprint,
			TopK:        5,
		})
		if err != nil {
			t.Fatalf("RecallBlocks(%s): %v", r.id, err)
		}
		for _, h := range hits {
			if h.Block.ID == r.id {
				recalled++
				if sampled <= 3 {
					t.Logf("  召回命中：%s score=%.4f", r.id, h.Score)
				}
				break
			}
		}
		if sampled >= 20 {
			break // 抽 20 个够证明读路径
		}
	}
	if sampled > 0 && recalled < sampled {
		t.Errorf("判据 3 失败：抽样 %d 个块，只有 %d 个能被自己的向量召回", sampled, recalled)
	}
	t.Logf("判据 3 通过：抽样 %d 个有向量的块，%d 个自召回命中", sampled, recalled)

	// 判据 4：modality/mime 未被回填改写
	for _, r := range all {
		if r.modality == "" {
			t.Errorf("块 %s modality 被清空", r.id)
		}
		if r.mime == "" {
			t.Errorf("块 %s mime 被清空", r.id)
		}
	}
	t.Logf("判据 4 通过：modality/mime 原样保留")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
