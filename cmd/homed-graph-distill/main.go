// Command homed-graph-distill 把存量实体跑字段拆分、落成块节点。
//
// 背景
// ------
// MigrateLegacyTextEntities 把 entities 迁成了块（a3e4c1c/09f31e2），但迁移
// 刻意**不做内容改写** —— 迁出来的块仍是整句：
//
//	「下周起值班室分机号改为 4324，旧号 4379 停用，值班轮换到 老周」
//
// 实测这种形态在值覆盖维度上答错（真实 chineseclip，真库 188 块）：
//
//	查询「值班室分机号是多少」→ top1 = 旧号 4379（score 0.8127）
//	                          新号 4324 那条进不了 top8
//
// 归因实验（改写形态重算向量）证实病根是**长复合句**把新旧值混在一句里：
//
//	现状·旧号（短句）    cos=0.8127
//	现状·新号（长复合句）cos=0.7327   ← 正确但排第二
//	改写·新号（短句）    cos=0.9284   ← 形态一变跃升 0.20
//
// 本命令做那一步改写：LLM 拆字段 → 「<主语>|<维度>=<值>」块 → 向量。
// 拆完的块才带得到仲裁（arbitration.go 需要 主语|维度=值 才解析得出）。
//
// 安全设计（与 homed-graph-migrate / homed-kb-migrate 同款）
// --------------------------------------------------------
//  1. **默认只报告**（-apply 才真写）。
//  2. 写入前自动快照 graph.db。
//  3. **不删任何东西**：整句块保留（它们是原始素材，回退路径）。
//     拆分块是新增的独立块（ID 由内容派生，重复跑幂等）。
//  4. 拆不动就记「零字段」并跳过，不编造。
//
// 用法：
//
//	homed-graph-distill -db /data/homeagent/memory/graph.db          # 报告
//	homed-graph-distill -db ... -apply                              # 拆分落块
//	homed-graph-distill -db ... -apply -limit 20                     # 先试 20 条
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/distill"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/pkg/embedding"
	"github.com/JianFeeeee/HologramAgent/pkg/generation"
	_ "github.com/JianFeeeee/HologramAgent/providers/chineseclip"
	_ "github.com/JianFeeeee/HologramAgent/providers/ollama"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正拆分落块（缺省只报告）")
	limit := flag.Int("limit", 0, "最多处理多少条实体，0 = 不限")
	// minLen 过滤掉太短的实体（标题类如「order-gw 运维进展」「每天」
	// 本来就没有可拆字段，跑它们只会浪费模型时间并让报告全是「零字段」）。
	//
	// ★ 实测（真库）：前 8 条实体全是这类短标题，拆分结果 8/8 零字段 ——
	// 那是**正确**结果，不是故障。不加这个过滤会误以为拆分坏了。
	minLen := flag.Int("min-len", 18, "实体名最小字符数，短于此的跳过")
	embedProvider := flag.String("embed-provider", "chineseclip", "向量 provider 名")
	modelDir := flag.String("model-dir", "", "向量 provider 的模型目录")
	genModel := flag.String("gen-model", "qwen3:1.7b", "拆分用的生成模型")
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

	entities, err := loadEntities(db, *limit, *minLen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：读取实体失败: %v\n", err)
		os.Exit(1)
	}
	if len(entities) == 0 {
		fmt.Println("库里没有 entities，无需拆分。")
		return
	}

	stats, _ := db.BlockVectorStats()
	fmt.Printf("待处理实体 %d 条；现有块 %d（带向量 %d）\n", len(entities), stats.Total, stats.WithVector)

	// ★ 只对**有主语**的实体跑拆分。
	//
	// 实测（生产库 429 条实体）：
	//
	//	有主语 5 条（1%），无主语 424 条（99%）
	//	  路径/URL 38 条 全无主语
	//	  符号开头  2 条 全无主语
	//	  其他     387 条 98.7% 无主语
	//
	// 而 ha-c 的主力句式「第114批…」「值班室分机号 4324」在生产库
	// **一条都没有**（含「第N批」0 条、含「分机」0 条）。
	//
	// 无主语的实体跑 LLM 是纯浪费：Split 里
	//   if len(subjects) == 0 { continue }
	// 会把 LLM 已经拆对的字段**全部丢弃**（实测 60 条 / 837 秒 / 0 产出）。
	//
	// ⇒ 这里先过滤，并在报告里说明跳过了多少 —— 让「没产出」这件事
	//   变成可解释的数字，而不是又一次「零字段 N 条」的谜。
	withSubj := make([]legacyEntity, 0, len(entities))
	noSubj := make([]legacyEntity, 0, len(entities))
	for _, e := range entities {
		if len(distill.DeriveSubjects(e.name)) > 0 {
			withSubj = append(withSubj, e)
		} else {
			noSubj = append(noSubj, e)
		}
	}
	if len(noSubj) > 0 {
		fmt.Printf("\n★ 跳过无主语实体 %d 条（%.0f%%）：Split 的第五道闸门"+
			"「无主语不写悬空属性」会把 LLM 已拆对的字段全部丢弃。\n",
			len(noSubj), float64(len(noSubj))/float64(len(entities))*100)
		for i, e := range noSubj {
			if i >= 3 {
				fmt.Printf("    … 另 %d 条\n", len(noSubj)-3)
				break
			}
			fmt.Printf("    · %s\n", truncForLog(e.name, 56))
		}
	}
	entities = withSubj
	if len(entities) == 0 {
		fmt.Println("\n没有可拆分的实体（有主语的为 0）。" +
			"这些内容应保持整句块形态 —— 迁移已把它们写成 legacy-entity 块。")
		return
	}
	fmt.Printf("\n实际处理 %d 条（其余 %d 条无主语，保持整句块形态）\n",
		len(entities), len(noSubj))

	// 打开两个 provider
	gen, err := generation.Open("ollama", generation.Config{
		Options: map[string]string{
			"model": *genModel, "num_thread": "10",
			"think": "false", "keep_alive": "30m",
		}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开生成 provider 失败: %v\n", err)
		os.Exit(1)
	}
	defer gen.Close()

	adapter, err := buildEmbedder(*embedProvider, *modelDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开向量 provider 失败: %v\n", err)
		os.Exit(1)
	}
	defer adapter.Close()
	embed := func(text string) ([]float64, string) {
		vec, err := adapter.VectorizeDense(text)
		if err != nil {
			return nil, ""
		}
		return vec, adapter.Fingerprint()
	}

	ex := distill.NewExtractor(gen, nil)

	// ── 干跑：先只统计，不写 ──
	// ★ 拆一次，落库复用同一份结果。
	//
	// 初版在 -apply 分支里又调了一次 ex.Blocks —— 每条记录跑两遍模型。
	// 两个后果，都实测撞到了：
	//  1. 耗时翻倍：真库 188 条在 CPU 上第一轮就超了 2400s 超时上限。
	//  2. **结果可能不一致**：qwen3:1.7b 在 Temperature=0 下仍有波动，
	//     干跑报告的字段与落库的实际字段可能不是同一批 ——
	//     报告与数据对不上，整个命令的可信度就没了。
	//
	// 所以 payload 在这里算一次并保留，-apply 直接用它落库。
	type splitResult struct {
		payload *distill.BlockPayload
		err     error
	}
	results := make([]splitResult, len(entities))
	triples := 0
	zeroField := 0
	failed := 0
	t0 := time.Now()
	for i, e := range entities {
		payload, err := ex.Blocks(context.Background(), e.name)
		if err != nil {
			results[i] = splitResult{err: err}
			failed++
		} else {
			results[i] = splitResult{payload: payload}
			n := len(payload.Fields)
			if n == 0 {
				zeroField++
			} else {
				triples += n
			}
		}
		// ★ 进度必须实时可见：这活儿在 CPU 上要十几分钟，
		// 没有进度就只能等超时（第一版就是这么废掉的 —— 2400s 超时，
		// 跑完的 35 条结果全丢）。每 10 条或每 20 秒打一行。
		if (i+1)%10 == 0 || i+1 == len(entities) {
			el := time.Since(t0).Seconds()
			rate := float64(i+1) / el
			eta := 0.0
			if rate > 0 {
				eta = float64(len(entities)-i-1) / rate
			}
			fmt.Printf("  [%d/%d] %s 用时%.0fs 预计剩余%.0fs\n",
				i+1, len(entities), truncForLog(e.name, 26), el, eta)
		}
		// ★ 前 8 条无条件打印 —— 包括「报错」和「零字段」两种情况。
		//
		// 原版只在 `payload != nil` 时打印字段，于是：
		//   - 报错 → payload 为 nil → 什么都不打印
		//   - 零字段 → Fields 为空 → 什么都不打印
		// 而生产蒸馏实测「零字段 60 条」，屏幕上**一行内容都没有**
		// —— 判据能显示 0，是因为它只看得到 0。
		//
		// 实测故障根因：ollama schema 污染让模型返回
		//   {"fields":[{"name":"text","value":"commit f91b27a…"}]}
		// 这种「字段名=字段类型、值=整句」的垃圾，被三道闸门全拦掉 ⇒ 零字段。
		// 那个原始响应就藏在 err 里（split.go 现在会带上），必须打出来。
		if i < 8 {
			switch {
			case results[i].err != nil:
				fmt.Printf("        ✘ %s\n", truncForLog(results[i].err.Error(), 400))
			case results[i].payload != nil && len(results[i].payload.Fields) > 0:
				for _, f := range results[i].payload.Fields {
					fmt.Printf("        %s|%s=%s\n", f.Subject, f.Dimension, f.Value)
				}
			default:
				fmt.Printf("        · 零字段（LLM 返回了合法 JSON 但没有字段）\n")
			}
		}
	}

	// ★ 报错与零字段必须分开报 —— 它们的根因完全不同：
	//   报错  = 工具链故障（JSON 解析失败、模型返回垃圾）
	//   零字段 = 数据事实（确实没有可拆字段）
	//   实测 60 条零字段的根因是前者（ollama schema 污染），
	//   但旧统计把它们混在一个数字里 ⇒ 看不出根因。
	fmt.Printf("\n拆解结果：%d 条实体 → %d 个字段块；零字段 %d 条，报错 %d 条\n",
		len(entities), triples, zeroField, failed)
	if !*apply {
		fmt.Println("\n这是报告模式（缺省）。加 -apply 真正落块。")
		fmt.Println("注意：整句块保留不动；拆分块是新增的独立块（内容派生 ID，重复跑幂等）。")
		return
	}

	// ── 快照 ──
	backup := fmt.Sprintf("%s.bak-distill-%s", *dbPath, time.Now().Format("20060102-150405"))
	if err := copyFile(*dbPath, backup); err != nil {
		fmt.Fprintf(os.Stderr, "错误：快照失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n已快照：%s\n", backup)

	// ── 落块 ──
	writeStart := time.Now()
	written, zeroWritten, writeFailed := 0, 0, 0
	for i, e := range entities {
		r := results[i]
		if r.err != nil {
			writeFailed++
			continue
		}
		if len(r.payload.Fields) == 0 {
			zeroWritten++
			continue
		}
		// 复用干跑阶段算出的 payload（不再调模型）
		n, err := distill.WritePayload(context.Background(), db, r.payload, embed)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  写入失败 %.40s: %v\n", e.name, err)
			writeFailed++
			continue
		}
		written += n
		if (i+1)%10 == 0 || i+1 == len(entities) {
			fmt.Printf("  落库 [%d/%d] 已写 %d 块\n", i+1, len(entities), written)
		}
	}
	elapsed := time.Since(writeStart)

	after, _ := db.BlockVectorStats()
	fmt.Printf("\n落块完成（%.1fs）：写入字段块 %d，零字段跳过 %d，写入失败 %d\n",
		elapsed.Seconds(), written, zeroWritten, writeFailed)
	fmt.Printf("块总数 %d → %d（带向量 %d）\n", stats.Total, after.Total, after.WithVector)

	// ★ 蒸馏加了新块，中心向量必须跟着重建。
	//
	// 中心是「全库带向量块的均值」，块集变了它就过期。
	// 迁移后建的那份不覆盖新蒸馏的块 —— 而新块恰恰是**拆分块**
	// （格式 <主语>|<维度>=<值>），它们与整句块分布不同，
	// 混在一个中心里会把中心拉偏，反而削弱区分度。
	if *embedProvider != "" {
		if _, anomalous, err := db.RebuildCentroid(adapter.Fingerprint()); err != nil {
			fmt.Fprintf(os.Stderr, "警告：重建中心失败（召回将无区分度）: %v\n", err)
		} else if anomalous {
			fmt.Println("已重建中心向量（检测到各向异性，已启用双边中心化）")
		} else {
			fmt.Println("已重建中心向量")
		}
	}

	// 抽样展示落库后的块文本
	if b, err := db.MemoryBlocks(); err == nil && len(b) > 0 {
		fmt.Println("\n抽样块文本（应有 <主语>|<维度>=<值> 形态）：")
		shown := 0
		for _, x := range b {
			if x.Source == "distill" {
				fmt.Printf("  %s\n", x.Text)
				shown++
				if shown >= 5 {
					break
				}
			}
		}
	}
	fmt.Println("\n下一步：跑 memory_recall 端到端，对比仲裁前后的召回效果。")
}

type legacyEntity struct {
	id   int64
	name string
}

func loadEntities(db *memory.GraphDB, limit, minLen int) ([]legacyEntity, error) {
	out, err := db.LegacyEntities(limit, minLen)
	if err != nil {
		return nil, err
	}
	var res []legacyEntity
	for _, e := range out {
		res = append(res, legacyEntity{id: e.ID, name: e.Name})
	}
	return res, nil
}

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

func truncForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0644)
}
