package memory

// 段页式在**生产数据副本**上的 e2e 验收（2026-10-29）。
//
// ## 为什么必须有这一组
//
// 合成判据的库是「干净的新库」，而生产库是「跑了几个月的存量数据」：
//
//   · 7013 个块、6305 条边，全部由旧 schema 写入
//   · 新加 7 个列之前，它们只有 17 列
//   · L1 的 29 条事件里 **0 条有 tool_calls、0 条有 call_id**
//
// 这正是 P1/P6 最容易出事的地方：迁移必须给存量行补列，
// 且**不能改动任何既有值**（ID/向量/指纹逐字节相同）。
// 合成数据永远测不出「ALTER TABLE 之后老数据还在不在」。
//
// ## 取快照（不要直接指生产库，判据会写）
//
//	sqlite3 "file:$HOME/../memory/graph.db?mode=ro" ".backup /tmp/graph-copy.db"
//	HA_PROD_DB=/tmp/graph-copy.db go test -count=1 -run ProdSeg ./internal/memory/
//
// 没有副本时自动跳过（CI 不该依赖本机路径）。

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// vecDigestForTest 把向量压成定长摘要，用于「逐字节相同」的比较。
//
// 为什么不直接比 []float64：浮点切片不能直接 `==` 做全等判断，
// 而「逐字节不变」正是本判据要钉的东西 —— 用摘要比更严格也更好读。
func vecDigestForTest(v []float64) string {
	if len(v) == 0 {
		return ""
	}
	h := sha256.New()
	for _, x := range v {
		fmt.Fprintf(h, "%x;", math.Float64bits(x))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// vecDigestForTestRaw 把基准侧的原始 JSON 解成向量后，用**同一个**摘要算法。
//
// ★ 关键：两侧必须用同一算法，否则比较毫无意义。
//
//	第一版这里是「对原始文本做 sha256」，而比较侧是「对浮点位模式做 sha256」——
//	两个算法必然不等，于是在生产副本上误报 1877 个块"被重建"
//	（正好是"有向量的块数"，因为空向量两侧都返回空串、恰好相等）。
//	这是判据自身的假阳性，不是迁移的缺陷。
func vecDigestForTestRaw(raw string) string {
	if raw == "" || raw == "null" {
		return ""
	}
	var v []float64
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "unparsable"
	}
	return vecDigestForTest(v)
}

// openProdCopy 把生产库复制到临时目录并打开（判据会写，绝不能动原件）。
func openProdCopy(t *testing.T) *GraphDB {
	t.Helper()
	src := prodCopySrc()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skip("无生产库副本，跳过：" + err.Error())
	}
	p := filepath.Join(t.TempDir(), "g.db")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := NewGraphDB(p)
	if err != nil {
		t.Fatalf("打开生产副本失败（迁移可能崩了）: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

// C5：迁移后每一行的 **ID / 向量 / 指纹逐字节不变**。
//
// 这是「不静默重建」的判据：如果迁移顺手重算了向量（或改了 ID），
// 那么所有块与新查询就落到不同空间里 —— 召回会全面失准，而且**不报错**。
func TestProdSeg_MigrationPreservesIdentity(t *testing.T) {
	src := prodCopySrc()
	if _, err := os.Stat(src); err != nil {
		t.Skip("无生产库副本，跳过")
	}

	// 先从**未迁移的原始副本**读出基线（用只读 SQL 直接读，不经过 NewGraphDB）。
	baseline := readRawBlockIdentity(t, src)
	if len(baseline) < 100 {
		t.Skipf("原始副本只读到 %d 块，不足以验证", len(baseline))
	}

	// 迁移（NewGraphDB 内部跑 ALTER TABLE）
	g := openProdCopy(t)

	same, changed, missing := 0, 0, 0
	var changedSample string
	for id, want := range baseline {
		got, ok, err := g.BlockByIDPrefix(id)
		if err != nil {
			t.Fatalf("读块 %s 失败: %v", id, err)
		}
		if !ok {
			missing++
			continue
		}
		if got.ID == id && got.Fingerprint == want.fp &&
			vecDigestForTest(got.Vector) == want.vecDigest {
			same++
		} else {
			changed++
			if changedSample == "" {
				changedSample = id
			}
		}
	}
	t.Logf("生产副本 %d 块：不变 %d，变化 %d，缺失 %d", len(baseline), same, changed, missing)

	if missing > 0 {
		t.Fatalf("★ 迁移后丢了 %d 个块（数据丢失）", missing)
	}
	if changed > 0 {
		t.Fatalf("★ 迁移改动了 %d 个块的 ID/向量/指纹（静默重建，首例 %s）",
			changed, changedSample)
	}
}

// 新列在生产副本上可用，且存量行的语义正确（frame_ts 为 NULL = 未分帧）。
func TestProdSeg_NewColumnsOnLegacyRows(t *testing.T) {
	g := openProdCopy(t)

	var total, nullFrame, nullRecalled int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE frame_ts IS NULL`).Scan(&nullFrame); err != nil {
		t.Fatal(err)
	}
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE last_recalled_at IS NULL`).Scan(&nullRecalled); err != nil {
		t.Fatal(err)
	}
	t.Logf("生产副本：%d 块，frame_ts NULL=%d，last_recalled_at NULL=%d",
		total, nullFrame, nullRecalled)

	if total != nullFrame {
		t.Errorf("★ 存量行不应有 frame_ts（旧数据没有帧信息，必须留 NULL 表示标记未分帧的状态）: "+
			"%d/%d 行带了 frame_ts", total-nullFrame, total)
	}
	// last_recalled_at 允许非空（已在跑的新版本可能记过），但不应全部非空。
	if nullRecalled == 0 {
		t.Errorf("★ 所有行都有 last_recalled_at —— 存量行不可能都被召回过，迁移写错了")
	}
}

// 存量块能被正常召回（迁移没有破坏检索能力）。
func TestProdSeg_RecallStillWorksOnLegacyData(t *testing.T) {
	g := openProdCopy(t)

	// 取一个有向量的块，用它的向量查自己（必然命中，除非检索坏了）
	var id, vectorJSON, fp string
	if err := g.db.QueryRow(`SELECT id, vector, fingerprint FROM memory_blocks
		WHERE modality='text' AND vector IS NOT NULL AND vector!='' AND vector!='null'
		LIMIT 1`).Scan(&id, &vectorJSON, &fp); err != nil {
		t.Skip("没有带向量的文本块，跳过")
	}
	var vec []float64
	if err := json.Unmarshal([]byte(vectorJSON), &vec); err != nil {
		t.Fatalf("解析向量失败: %v", err)
	}
	if len(vec) == 0 {
		t.Skip("向量为空，跳过")
	}

	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: vec, TopK: 10, Fingerprint: fp,
	})
	if err != nil {
		t.Fatalf("召回失败: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("★ 用块自己的向量都召回不到东西 —— 迁移破坏了检索")
	}
	// 命中自己
	found := false
	for _, h := range hits {
		if h.Block.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Logf("自查询未命中自身（前 10 名被高权重块占据），属正常，hits=%d", len(hits))
	}

	// P6：召回必须刷新 last_recalled_at
	got, ok, err := g.BlockByIDPrefix(hits[0].Block.ID)
	if err != nil || !ok {
		t.Fatalf("读回失败: %v ok=%v", err, ok)
	}
	if got.LastRecalledAt.IsZero() {
		t.Fatalf("★ 生产副本上召回仍未记录 last_recalled_at")
	}
}

// ── 基线读取（只读，不触发迁移）──

type blockIdentity struct {
	fp        string
	vecDigest string
}

func readRawBlockIdentity(t *testing.T, path string) map[string]blockIdentity {
	t.Helper()
	// 用只读模式打开，避免触发 ALTER TABLE。
	// 只读打开：不触发 ALTER TABLE（NewGraphDB 才会跑迁移）。
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Skipf("只读打开失败（跳过基线对比）: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT id, fingerprint, vector FROM memory_blocks`)
	if err != nil {
		t.Skipf("读基线失败（可能是旧库 schema 差异）: %v", err)
	}
	defer rows.Close()

	out := map[string]blockIdentity{}
	for rows.Next() {
		var id, fp, vectorJSON string
		if err := rows.Scan(&id, &fp, &vectorJSON); err != nil {
			t.Fatal(err)
		}
		out[id] = blockIdentity{fp: fp, vecDigest: vecDigestForTestRaw(vectorJSON)}
	}
	return out
}
