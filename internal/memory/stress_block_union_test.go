package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 块 union 化的压力测试：媒体字节进块之后，块生命周期里的并发热点。
//
// 为什么需要专门的压测（而不是只跑既有单测）：
// 块 union 化把「元数据」与「负载」合并进同一行（memory_blocks.data BLOB），
// 于是原本分属两个存储（media.db 元数据 + blobs 文件）的读写现在全部压到
// 同一张表、同一把 g.mu。这条改动是否引入锁争用/丢更新/字节错位，
// 只有并发实测能回答。
//
// 全部判据都是**不变量**，不是「函数被调过」：
//  1. 同内容并发写块：只留一份，且字节不丢（ON CONFLICT 语义）
//  2. 并发读字节与并发写向量：BlockData 永不返回半截/错位字节
//  3. 回填与召回并发：回填是单事务，读者要么看到全旧要么全新，不撕裂
//  4. 删除与读取并发：读到的字节必须要么是完整的原内容，要么是「不存在」

// ── 1. 同内容并发写块：去重且字节不丢 ────────────────────────────────

func TestStressBlockUnion_ConcurrentPutSameContent(t *testing.T) {
	g := newBlockTestGraph(t)

	const writers = 16
	payload := bytes.Repeat([]byte("SAME-CONTENT-PAYLOAD-"), 4096) // 88KB
	id := MediaBlockID("image/png", payload)

	var wg sync.WaitGroup
	var errs atomic.Int64
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			b := MemoryBlock{
				ID: id, Modality: BlockImage, Data: payload, MIME: "image/png",
				Size: int64(len(payload)), Source: fmt.Sprintf("writer-%d", n),
				Vector: vec(1, 0), Fingerprint: "fp",
			}
			if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
				errs.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if errs.Load() != 0 {
		t.Fatalf("%d 次并发写入报错", errs.Load())
	}

	// 不变量 1a：只留一份
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同内容并发写应只得 1 行，实际 %d", n)
	}
	// 不变量 1b：字节完整
	got, err := g.BlockData(id)
	if err != nil {
		t.Fatalf("BlockData: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("字节被破坏：len=%d 期望 %d", len(got), len(payload))
	}
}

// 关键回归：len 列表（blockColumns 不含 data）重写已回填块时，
// ON CONFLICT 不得把 data 覆盖成空 —— 那会让媒体字节凭空消失。
func TestStressBlockUnion_LeanRewriteMustNotBlankData(t *testing.T) {
	g := newBlockTestGraph(t)
	payload := []byte("ORIGINAL-MEDIA-BYTES")
	id := MediaBlockID("image/png", payload)
	mustPut(t, g, []MemoryBlock{{
		ID: id, Modality: BlockImage, Data: payload, MIME: "image/png",
		Size: int64(len(payload)),
	}})

	// 并发地用「不带 Data 的瘦块」重写（模拟只更新向量/指纹的路径）
	const writers = 16
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_ = g.PutMemoryBlocks([]MemoryBlock{{
				ID: id, Modality: BlockImage, MIME: "image/png",
				Vector: vec(float64(n), 1), Fingerprint: "fp2",
				// 刻意不带 Data
			}})
		}(i)
	}
	wg.Wait()

	got, err := g.BlockData(id)
	if err != nil {
		t.Fatalf("BlockData: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("瘦块重写把 data 覆盖没了：%q（期望 %q）", got, payload)
	}
}

// ── 2. 并发读字节 + 并发写向量：读到的必须始终是完整原字节 ──────────

func TestStressBlockUnion_ConcurrentReadWriteNoTear(t *testing.T) {
	g := newBlockTestGraph(t)

	const nBlocks = 24
	payloads := make([][]byte, nBlocks)
	ids := make([]string, nBlocks)
	for i := range payloads {
		// 每块内容与长度都不同：错位/串块立刻可见
		payloads[i] = bytes.Repeat([]byte{byte('A' + i)}, 1000+i*37)
		ids[i] = MediaBlockID("image/png", payloads[i])
	}
	blocks := make([]MemoryBlock, nBlocks)
	for i := range payloads {
		blocks[i] = MemoryBlock{
			ID: ids[i], Modality: BlockImage, Data: payloads[i], MIME: "image/png",
			Size: int64(len(payloads[i])), Vector: vec(1, 0), Fingerprint: "fp",
		}
	}
	mustPut(t, g, blocks)

	stop := make(chan struct{})
	var tear, notFound atomic.Int64
	var reads atomic.Int64

	// 读者：反复取字节，逐字节比对
	var wg sync.WaitGroup
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			i := seed
			for {
				select {
				case <-stop:
					return
				default:
				}
				i = (i + 7) % nBlocks
				data, err := g.BlockData(ids[i])
				if err != nil {
					notFound.Add(1)
					continue
				}
				reads.Add(1)
				if !bytes.Equal(data, payloads[i]) {
					tear.Add(1)
				}
			}
		}(r)
	}
	// 写者：反复改向量（不碰 data）
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			tick := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				i := tick % nBlocks
				// SetBlockVector 只改向量列（与字节无关），
				// 与 BlockData 并发执行不应互相影响。
				_ = g.SetBlockVector(ids[i], vec(float64(n), float64(tick)), "fp")
				tick++
			}
		}(w)
	}

	time.Sleep(1500 * time.Millisecond)
	close(stop)
	wg.Wait()

	if tear.Load() != 0 {
		t.Errorf("并发下读到撕裂/错位字节 %d 次", tear.Load())
	}
	if notFound.Load() != 0 {
		t.Errorf("并发下 BlockData 失败 %d 次（写向量不该影响负载可读性）", notFound.Load())
	}
	t.Logf("并发读 %d 次，0 撕裂、0 丢失", reads.Load())
}

// ── 3. 回填与召回并发：不撕裂（读者看到全旧或全新）───────────────

func TestStressBlockUnion_BackfillConcurrentWithRecall(t *testing.T) {
	g := newBlockTestGraph(t)

	const n = 30
	// 造「旧库形态」：块有 payload_digest、data 空
	if err := g.ensurePayloadDigestColumn(); err != nil {
		t.Fatal(err)
	}
	cas := map[string][]byte{}
	for i := 0; i < n; i++ {
		d := fmt.Sprintf("digest-%02d", i)
		cas[d] = bytes.Repeat([]byte{byte('a' + i%26)}, 512+i)
		if _, err := g.db.Exec(`INSERT INTO memory_blocks
			(id, modality, payload_digest, mime, vector, fingerprint, created_at, updated_at)
			VALUES (?, 'image', ?, 'image/png', ?, 'fp', datetime('now'), datetime('now'))`,
			fmt.Sprintf("blk_%02d", i), d, `[1,0]`); err != nil {
			t.Fatal(err)
		}
	}

	resolve := func(d string) ([]byte, bool) { b, ok := cas[d]; return b, ok }

	stop := make(chan struct{})
	var bad atomic.Int64
	var reads atomic.Int64
	var wg sync.WaitGroup

	// 读者：任何时刻取回的字节要么为空（未回填），要么是完整正确内容
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < n; i++ {
					data, err := g.BlockData(fmt.Sprintf("blk_%02d", i))
					if err != nil || len(data) == 0 {
						continue // 未回填：合法中间态
					}
					reads.Add(1)
					want := cas[fmt.Sprintf("digest-%02d", i)]
					if !bytes.Equal(data, want) {
						bad.Add(1)
					}
				}
			}
		}()
	}
	// 写者：回填（单事务）
	wg.Add(1)
	go func() {
		defer wg.Done()
		st, err := g.BackfillBlockData(resolve)
		if err != nil {
			t.Errorf("BackfillBlockData: %v", err)
			return
		}
		if st.Filled != n {
			t.Errorf("回填 %d/%d", st.Filled, n)
		}
	}()

	time.Sleep(1200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if bad.Load() != 0 {
		t.Errorf("回填与读取并发时读到错误字节 %d 次", bad.Load())
	}
	// 终态：全部回填
	left, _ := g.CountBlockDataBackfillCandidates()
	if left != 0 {
		t.Errorf("回填结束后仍有 %d 个候选", left)
	}
	t.Logf("回填并发读 %d 次，0 错字节", reads.Load())
}

// ── 4. 删除与读取并发：只能看到「完整内容」或「不存在」────────────
//
// ★ 为何不用 media 块：生产**没有**按块 ID 删媒体块的路径。
//   删除发生在实体级（DeleteEntity 按 text_content 匹配）与
//   孤块清理（PurgeOrphans 只扫 text_content != ''）。
//   所以这里用 DeleteEntity 这个**真实路径**，分两步验证：
//     (a) 被删 текст块的字节在删除过程中不会被读到半截
//     (b) 删完后块与负载一起消失（块 union 的核心承诺：字节随块同生共灭）
func TestStressBlockUnion_DeleteConcurrentWithRead(t *testing.T) {
	g := newBlockTestGraph(t)

	const n = 16
	payloads := make([][]byte, n)
	texts := make([]string, n)
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		payloads[i] = bytes.Repeat([]byte{byte('0' + i)}, 800+i*11)
		texts[i] = fmt.Sprintf("待删文本块-%02d", i)
		// 同时带正文与负载的块（union 的两半）：删除必须两半一起走
		ids[i] = fmt.Sprintf("blk_test_del_%02d", i)
	}
	blocks := make([]MemoryBlock, n)
	for i := 0; i < n; i++ {
		blocks[i] = MemoryBlock{ID: ids[i], Modality: BlockText, Text: texts[i],
			Data: payloads[i], MIME: "application/octet-stream",
			Size: int64(len(payloads[i]))}
	}
	mustPut(t, g, blocks)

	stop := make(chan struct{})
	var bad, gone atomic.Int64
	var wg sync.WaitGroup
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for i := 0; i < n; i++ {
					data, err := g.BlockData(ids[i])
					if err != nil || len(data) == 0 {
						gone.Add(1)
						continue
					}
					if !bytes.Equal(data, payloads[i]) {
						bad.Add(1)
					}
				}
			}
		}()
	}
	// 写者：用真实删除路径 DeleteEntity 逐个删（它按 text_content 匹配）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if _, err := g.DeleteEntity(texts[i]); err != nil {
				t.Errorf("DeleteEntity(%s): %v", texts[i], err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	time.Sleep(1200 * time.Millisecond)
	close(stop)
	wg.Wait()

	if bad.Load() != 0 {
		t.Errorf("删除并发时读到非完整内容 %d 次（应为『完整』或『不存在』）", bad.Load())
	}
	// 终态：块与负载一起消失
	for i := 0; i < n; i++ {
		if data, err := g.BlockData(ids[i]); err == nil && len(data) > 0 {
			t.Errorf("块 %s 已删但负载还在（%d 字节）—— union 两半未同生共灭", ids[i], len(data))
			break
		}
	}
	t.Logf("删除并发：读到「不存在」%d 次、0 次非完整内容", gone.Load())
}

// ── 5. 向量写回幂等 + 并发（reembedStaleMedia 的落库动作）────────────

func TestStressBlockUnion_ConcurrentSetVector(t *testing.T) {
	g := newBlockTestGraph(t)
	id := MediaBlockID("image/png", []byte("x"))
	mustPut(t, g, []MemoryBlock{{ID: id, Modality: BlockImage, Data: []byte("x"), MIME: "image/png"}})

	const writers = 12
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				if err := g.SetBlockVector(id, vec(float64(n), float64(k)), "fpX"); err != nil {
					t.Errorf("SetBlockVector: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	// 不变量：向量列是合法 JSON 且维度为 2，字节没被动过
	b, ok, err := g.BlockByIDPrefix(id)
	if err != nil || !ok {
		t.Fatalf("取块失败: ok=%v err=%v", ok, err)
	}
	if len(b.Vector) != 2 {
		t.Fatalf("向量维度应为 2，实际 %d", len(b.Vector))
	}
	if _, err := json.Marshal(b.Vector); err != nil {
		t.Fatalf("向量不是合法 JSON: %v", err)
	}
	if data, _ := g.BlockData(id); string(data) != "x" {
		t.Errorf("并发写向量破坏了字节: %q", data)
	}
}

// ── 6. 压力：大量块 + 大负载，反复读写（内存/磁盘不炸）────────────

func TestStressBlockUnion_ManyBlocksLargePayloads(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过")
	}
	dir := t.TempDir()
	g, err := NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	const n = 120
	const payloadSize = 256 * 1024 // 每块 256KB → 合计约 30MB
	blocks := make([]MemoryBlock, 0, n)
	payloads := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		p := bytes.Repeat([]byte{byte(i)}, payloadSize)
		payloads = append(payloads, p)
		blocks = append(blocks, MemoryBlock{
			ID: MediaBlockID("image/png", p), Modality: BlockImage, Data: p,
			MIME: "image/png", Size: int64(len(p)),
			Vector: vec(float64(i%7), 1), Fingerprint: "fp",
		})
	}
	if err := g.PutMemoryBlocks(blocks); err != nil {
		t.Fatalf("批量写入 %d 块: %v", n, err)
	}

	// 反复取回并逐字节校验
	for round := 0; round < 3; round++ {
		for i, b := range blocks {
			data, err := g.BlockData(b.ID)
			if err != nil {
				t.Fatalf("round %d BlockData(%s): %v", round, b.ID, err)
			}
			if !bytes.Equal(data, payloads[i]) {
				t.Fatalf("round %d 块 %s 字节不符（len=%d vs %d）", round, b.ID, len(data), len(payloads[i]))
			}
		}
	}
	t.Logf("%d 块 × %dKB = %.1fMB 反复读写校验通过",
		n, payloadSize/1024, float64(n*payloadSize)/1024/1024)
}
