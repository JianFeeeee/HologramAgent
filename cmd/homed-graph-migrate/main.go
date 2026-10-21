// Command homed-graph-migrate 把存量 entities 迁移成块节点（entities 退场第 3 步）。
//
// 背景
// ----
// entities 是旧形态：只有 name/type/mention_count，没有向量。块节点
// (memory_blocks) 带 vector/fingerprint/modality，是图节点的正统形态。
// MigrateLegacyMediaEntities 已经把媒体类实体迁过一次（internal/memory/migrate.go），
// 本命令处理剩下的文本类实体。
//
// 安全设计（与 homed-kb-migrate 同款）
// --------------------------------------
//  1. **默认只报告**（-apply 才真迁移）。迁移本身是单事务、全成功或全回滚，
//     但「迁完召回是否变好」只有跑过才知道。
//  2. **不删旧表**：迁移只加块与边，entities 原样保留。验证通过后由人工
//     另跑清理（下一步）。这样「效果不如预期」时的回滚就是什么都不做。
//  3. 迁移前自动快照 graph.db 到 <db>.bak-<时间戳>。
//  4. **向量是可选的**：-embed-provider 缺省则不带向量迁移（结构正确、
//     不参与向量召回）。可后续用 -backfill 单独回填，不必重跑迁移。
//
// 用法：
//
//	homed-graph-migrate -db /data/homeagent/memory/graph.db              # 报告
//	homed-graph-migrate -db ... -apply                                 # 迁移（不带向量）
//	homed-graph-migrate -db ... -apply -embed chineseclip -model-dir ...  # 带向量迁移
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
	_ "github.com/JianFeeeee/HologramAgent/providers/chineseclip"
	_ "github.com/JianFeeeee/HologramAgent/providers/qwen3vl"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正迁移（缺省只报告）")
	provider := flag.String("embed-provider", "", "向量 provider 名（chineseclip / qwen3vl）；缺省则不带向量")
	modelDir := flag.String("model-dir", "", "provider 的模型目录")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -db <graph.db 路径>")
		os.Exit(2)
	}
	if _, err := os.Stat(*dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "错误：打不开 %s: %v\n", *dbPath, err)
		os.Exit(2)
	}

	db, err := memory.NewGraphDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开图库失败: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	// ── 报告现状 ──
	before, err := legacyStats(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取现状失败: %v\n", err)
		os.Exit(1)
	}
	stats, err := db.BlockVectorStats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取块向量状态失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("迁移前：%s\n", before)
	fmt.Printf("        块 %d（其中带向量 %d），块边 %d\n",
		stats.Total, stats.WithVector, before.BlockEdges)
	if before.Entities == 0 {
		fmt.Println("\n库里没有 entities，无需迁移。")
		return
	}

	// ── 准备 embed（可选）──
	var embed memory.EntityEmbedder
	// embedFP 记下 provider 指纹：迁移后重建中心要用（见文件末尾说明）。
	var embedFP string
	if *provider != "" {
		adapter, err := buildEmbedder(*provider, *modelDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：打开 provider %q 失败: %v\n", *provider, err)
			fmt.Fprintln(os.Stderr, "（不带向量也能迁移：结构正确，向量可后续回填）")
			os.Exit(1)
		}
		defer adapter.Close()
		embed = func(name string) ([]float64, string) {
			vec, err := adapter.VectorizeDense(name)
			if err != nil {
				return nil, ""
			}
			return vec, adapter.Fingerprint()
		}
		embedFP = adapter.Fingerprint()
		fmt.Printf("向量 provider：%s（指纹 %s，维度 %d）\n",
			*provider, embedFP, adapter.Dim())
	} else {
		fmt.Println("向量 provider：未指定 → 块不带向量迁移（可后续回填）")
	}

	if !*apply {
		fmt.Printf("\n这是报告模式（缺省）。加 -apply 真正迁移。\n")
		fmt.Printf("预计产出：块 %d，块边 %d（实体名同时作为句子，句子--contains-->块）\n",
			before.Entities, before.RelationsTotal)
		// ★ 口径说明：块边数是**按 relations 全表**估计的，与迁移同口径。
		// 孤儿关系（两端实体已不存在）会被跳过，实际产出可能略少。
		fmt.Println("  （块边按 relations 全表计，孤儿关系会被跳过，实际可能略少）")
		fmt.Printf("旧表 entities/relations/sentences 保持不动，验证通过后再单独清理。\n")
		return
	}

	// ── 快照 ──
	backup := fmt.Sprintf("%s.bak-%s", *dbPath, time.Now().Format("20060102-150405"))
	if err := copyFile(*dbPath, backup); err != nil {
		fmt.Fprintf(os.Stderr, "错误：快照失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n已快照：%s\n", backup)

	// ── 执行 ──
	t0 := time.Now()
	res, err := db.MigrateLegacyTextEntities(embed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n迁移失败（已整体回滚）: %v\n", err)
		fmt.Fprintf(os.Stderr, "快照可用于人工核对：%s\n", backup)
		os.Exit(1)
	}

	// ★★ scene_refs 迁移必须与块迁移**同一次**完成（2026-10-04）
	//
	// scene_refs 里的 kind='entity' / 'relation' 指向旧表的行。
	// 块迁移完成但 scene_refs 没迁 ⇒ 场景式记忆全部指向不存在的对象，
	// 而 RecallByScene 只 JOIN 块/边 ⇒ **静默召回空**。
	//
	// ★ 之前它是独立函数（只在测试里被调过），
	//   于是生产迁移留下 718 条旧 kind 引用 —— 验证脚本当场抓到。
	scN, err := db.MigrateSceneRefsToBlocks()
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nscene_refs 迁移失败（已整体回滚）: %v\n", err)
		fmt.Fprintf(os.Stderr, "快照可用于人工核对：%s\n", backup)
		os.Exit(1)
	}
	fmt.Printf("\n  scene_refs：迁移 %d 条（kind 分布迁移后应为 block/edge/document）\n", scN)

	after, err := legacyStats(db)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取迁移后状态失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n迁移完成（%.1fs）\n", time.Since(t0).Seconds())
	fmt.Printf("  句子 +%d，块 +%d，边 +%d\n", res.Sentences, res.Blocks, res.Edges)
	if res.SkippedOrphan > 0 {
		fmt.Printf("  跳过孤儿关系 %d 条（端点实体为空名；关系信息仍在句子里）\n", res.SkippedOrphan)
	}
	if res.SkippedNoVec > 0 {
		fmt.Printf("  无向量块 %d 个（可后续回填；本次迁移不影响它们按边查取）\n", res.SkippedNoVec)
	}
	fmt.Printf("\n迁移后：%s\n", after)

	// ★ 迁移后必须重建中心向量，否则召回没有区分度。
	//
	// 实测（生产快照 1294 实体 / 1391 块）：迁移后不建中心，
	// 召回分数挤成一团 —— 查询「本机 13010 端口」返回的 8 条
	// 全在 0.84~0.90，含精确串 13010 的目标块连 top2000 都进不去：
	//
	//	1. 0.9006  本机 443 按 SNI 透传到 192.168.2.106:3080
	//	2. 0.8712  ACP回环调用自身12001
	//	3. 0.8611  MAR/MDR与ALU不直通必须经CPU内部总线
	//
	// 而 RebuildCentroid 此前**只有测试在调用**，生产路径没有调用者
	// —— ha-c 的中心是手工测试时留下的，生产侧从来没有过。
	if embedFP != "" {
		if _, anomalous, err := db.RebuildCentroid(embedFP); err != nil {
			fmt.Fprintf(os.Stderr, "警告：重建中心失败（召回将无区分度）: %v\n", err)
		} else if anomalous {
			fmt.Println("已重建中心向量（检测到各向异性，已启用双边中心化）")
		} else {
			fmt.Println("已重建中心向量（各向异性不显著，仍保存以便后续块增多时复用）")
		}
	} else {
		fmt.Println("未指定向量 provider，跳过中心重建" +
			"（无向量时中心无意义；回填向量后请重跑本命令或手工 RebuildCentroid）")
	}

	fmt.Println("\n下一步：验证召回改善（memory_recall 的跨维度探针），确认后再清理旧表。")
}

// buildEmbedder 按名字打开向量 provider 并适配成 VectorizeDense 形态。
//
// 走 pkg/embedding 注册表（内核无感边界）：换模型只改 -embed-provider，
// 本命令不含任何 provider 名分支。
func buildEmbedder(name, modelDir string) (*vector.ProviderAdapter, error) {
	opts := map[string]string{}
	if modelDir != "" {
		opts["model_dir"] = modelDir
	}
	p, err := embedding.Open(name, embedding.Config{Options: opts})
	if err != nil {
		return nil, err
	}
	adapted, err := vector.AdaptProvider(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	return adapted, nil
}

type legacyStat struct {
	Entities, Relations, Blocks, BlockEdges int
	// RelationsTotal 是 relations 全表数（不过滤 status）。
	//
	// 迁移转换全表（MigrateLegacyTextEntities 按 ORDER BY id 遍历），
	// 而 Introspect 的 relation_count 是**活跃数**（status='active'），
	// 两者语义不同：报告必须用这个，否则「预计产出」会少报
	// （生产库实测 966 vs 980）。
	RelationsTotal int
}

func (s legacyStat) String() string {
	return fmt.Sprintf("实体 %d，关系 %d，块 %d，块边 %d",
		s.Entities, s.Relations, s.Blocks, s.BlockEdges)
}

// legacyStats 汇总迁移前后的库规模。
//
// 复用既有的 Introspect（实体/关系统计）与 BlockVectorStats（块向量状态），
// 不新增统计 API：句数与块边数用只读查询补齐。
func legacyStats(db *memory.GraphDB) (legacyStat, error) {
	var s legacyStat
	// ★★ 全部直查旧表，不用 Introspect（2026-10-04）
	//
	// Introspect 的 entity_count / relation_count 在读侧切块之后
	// 数的是**块侧**（块数、活跃块边数）。而本命令的全部意义是
	// 「旧表还有多少没迁走」⇒ 口径必须是旧表。
	//
	// ★ 实测踩到的后果（生产快照）：
	//
	//     迁移前：实体 1294，关系 0，块 98，块边 97
	//
	//   「关系 0」是假的 —— 旧 relations 表里明明有 980 行
	//   （active 966 / deleted 14）。而报告照抄这个数，
	//   于是「预计产出：块 1294，块边 0」。
	//
	//   ★★ 那句话会让运维以为「旧关系早迁完了」，从而跳过迁移 ——
	//      而实际上 980 条关系一条都没迁。这是**诊断误导**，
	//      比报错危险：报错了会有人查，误导了没人会。
	// ★ 错误一律**返回**，不吞。
	//
	// 迁移报告的价值全在数字准确上；一个被静默吞掉的查询错误
	// 会变成「库里没有关系」，而迁移会照跑，把该迁的漏掉。
	for _, spec := range []struct {
		table string
		where []string
		dst   *int
	}{
		{"entities", nil, &s.Entities},
		{"relations", []string{"status = 'active'"}, &s.Relations},
		// 迁移报告用全表数（与 MigrateLegacyTextEntities 的遍历范围一致）
		{"relations", nil, &s.RelationsTotal},
	} {
		v, err := legacyCount(db, spec.table, spec.where...)
		if err != nil {
			return s, err
		}
		*spec.dst = v
	}
	edges, err := db.MemoryBlockEdges()
	if err != nil {
		return s, err
	}
	s.BlockEdges = len(edges)
	bs, err := db.BlockVectorStats()
	if err != nil {
		return s, err
	}
	s.Blocks = bs.Total
	return s, nil
}

// countSentences 通过块边反推句子数不可靠（迁移前无块），
// 而 Introspect 不含句数 —— 迁移报告里句数不是关键指标，
// 因此不新增统计 API，只用实体/关系/块三个数（够判断迁移效果）。

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}

// legacyCount 直查旧表的行数。
//
// ★ 为什么不用 Introspect：它数的是块侧（读侧切块后），
//
//	而迁移报告要的是「旧表还剩多少」。
//
// where 为空表示不加过滤。
func legacyCount(db *memory.GraphDB, table string, where ...string) (int, error) {
	// 表名是内部常量调用方给的，不是外部输入；
	// 仍用白名单校验 —— 迁移命令会拿用户给的 -db 路径，
	// 而 SQL 拼接不该留任何口子。
	switch table {
	case "entities", "relations", "sentences":
	default:
		return 0, fmt.Errorf("legacyCount: 不支持的表 %q", table)
	}
	q := "SELECT COUNT(*) FROM " + table
	for i, w := range where {
		// ★ 第一个条件要 WHERE，后续才 AND。
		//
		// 我写成统一的 `q += " AND " + w`，于是第一条条件产生
		// `FROM relations AND status='active'` —— SQL 语法错误。
		//
		// ★★ 而错误被调用方的 `if err == nil` 吞掉，
		//   于是报告打出「关系 0」：一个**语法错误**伪装成
		//   「库里没有关系」。
		//   诊断报告里的假 0 比报错危险 —— 报错会有人查，0 不会。
		if i == 0 {
			q += " WHERE " + w
		} else {
			q += " AND " + w
		}
	}
	n, err := db.LegacyRowCount(q)
	if err != nil {
		return 0, fmt.Errorf("legacyCount(%s): %w", table, err)
	}
	return n, nil
}
