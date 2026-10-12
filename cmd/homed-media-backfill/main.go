// Command homed-media-backfill 把存量媒体字节从旧 CAS 回填进块自身的 data 列。
//
// 背景
// ----
// media.Store/CAS 淘汰后，媒体字节成为块自己的负载（memory_blocks.data）。
// 但存量库里 data 为空、payload_digest 仍指向 CAS 的 blob —— 必须一次性
// 把字节搬进块，否则 CAS 目录一删，BlockData 就取不到字节（图永久丢失）。
//
// 存量事实（生产 /home/newqqagent/memory）：
//   - graph.db 有 112 个媒体块，引 106 个唯一 digest
//   - media.db 156 条，blobs 84M，106 digest 抽样 100% 命中
//
// 安全设计（与 homed-graph-migrate 同款）
// --------------------------------------
//  1. 默认只报告（-apply 才真回填）。
//  2. 单事务、全成功或全回滚。幂等：只处理 data 为空且 payload_digest 非空的块。
//  3. 迁移前自动快照 graph.db 到 <db>.bak-<时间戳>。
//  4. CAS 里取不到字节的块计为 Missing 并跳过（既有数据损失，不阻断其余回填）。
//
// 用法：
//
//	homed-media-backfill -db /data/homeagent/memory/graph.db -media /data/homeagent/memory/media     # 报告
//	homed-media-backfill -db ... -media ... -apply                                                  # 回填
//	homed-media-backfill -db ... -apply -no-snapshot                                                 # 跳过快照（已自行备份）
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/media"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径（必填）")
	mediaDir := flag.String("media", "", "旧 CAS 目录（含 media.db + blobs/，必填）")
	apply := flag.Bool("apply", false, "真正回填（缺省只报告）")
	noSnapshot := flag.Bool("no-snapshot", false, "跳过 graph.db 快照（已自行备份时用）")
	flag.Parse()

	if *dbPath == "" || *mediaDir == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -db <graph.db> 和 -media <cas 目录>")
		os.Exit(2)
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "错误：打不开 %s: %v\n", *dbPath, err)
		os.Exit(2)
	}

	g, err := memory.NewGraphDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开图库失败: %v\n", err)
		os.Exit(1)
	}
	defer g.Close()

	// 报告：有多少块待回填
	cands, err := g.CountBlockDataBackfillCandidates()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：统计候选失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("待回填块：%d（data 为空且 payload_digest 非空）\n", cands)
	if cands == 0 {
		fmt.Println("\n无需回填：所有媒体块已带字节，或无 payload_digest 残留。")
		return
	}

	// CAS 逐个取字节（顺带校验：media.Store.Get 内部 sha256 校验）
	var ms *media.Store
	ms, err = media.New(*mediaDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开 CAS 失败: %v\n", err)
		os.Exit(1)
	}
	defer ms.Close()

	resolve := func(digest string) ([]byte, bool) {
		data, err := ms.Get(digest)
		if err != nil {
			return nil, false
		}
		return data, true
	}

	// 报告：CAS 能取到多少字节（dry-run 探针，不写库）
	probe, err := g.ProbeBackfillAvailability(resolve)
	if err != nil {
		fmt.Fprintf(os.Stderr, "警告：CAS 探针失败: %v\n", err)
		probe = -1
	}
	if !*apply {
		fmt.Printf("\n这是报告模式（缺省）。加 -apply 真正回填。\n")
		if probe >= 0 {
			fmt.Printf("CAS 字节命中率探针：%d/%d 可取回\n", probe, cands)
		}
		return
	}

	// 快照
	if !*noSnapshot {
		backup := fmt.Sprintf("%s.bak-%s", *dbPath, time.Now().Format("20060102-150405"))
		if err := copyFile(*dbPath, backup); err != nil {
			fmt.Fprintf(os.Stderr, "错误：快照失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已快照：%s\n", backup)
	}

	// 执行
	t0 := time.Now()
	st, err := g.BackfillBlockData(resolve)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n回填失败（已整体回滚）: %v\n", err)
		fmt.Fprintf(os.Stderr, "快照可用于人工核对\n")
		os.Exit(1)
	}
	fmt.Printf("\n回填完成（%.1fs）\n", time.Since(t0).Seconds())
	fmt.Printf("  %s\n", st.String())
	fmt.Printf("  累计回填 %.2f MB\n", float64(st.SizeFilled)/1024/1024)
	if st.Missing > 0 {
		fmt.Printf("\n⚠️ %d 个块的内容在 CAS 里已不存在（既有数据损失，无法补救）\n", st.Missing)
	}

	// 验证：回填后立刻 BlockData 取一次样
	fmt.Println("\n抽样验证（BlockData 取回前 3 块）：")
	for i, id := range sampleFilled(g, 3) {
		data, err := g.BlockData(id)
		if err != nil {
			fmt.Printf("  %d. %s ⚠️ BlockData 失败: %v\n", i+1, id, err)
			continue
		}
		fmt.Printf("  %d. %s → %d 字节\n", i+1, id, len(data))
	}
}

// sampleFilled 返回前 n 个已回填字节的块 id（供验证 BlockData）。
func sampleFilled(g *memory.GraphDB, n int) []string {
	ids, _ := g.SampleFilledBlockIDs(n)
	return ids
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
