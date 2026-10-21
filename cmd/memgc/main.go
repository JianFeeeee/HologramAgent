// memgc 清理图记忆里已存在的「噪音实体」「孤立实体」及其关系。
//
// 为什么需要这个命令：噪音闸门（internal/memory.IsNoiseEntity）只能拦住
// **新写入**的噪音。旧库里那批（常用词 / 归档内部标记 / 模板摘要回声）是
// 闸门上线前攒下的存量，没人清就一直在——热实体被它们占着，召回预算被
// 同构垃圾边挤满。清理是一次性动作，但需要可重复执行、可先看不做。
//
// 两件事分开开关：-orphans 处理的是「零关系的空节点」（清理噪音后另一端
// 留下的壳），它们的名字本身可能没问题，但已经不在图里了。
//
// 用法（默认 dry-run，只列不删）：
//
//	memgc -db ${HA_DATA}/memory/graph.db
//	memgc -db ${HA_DATA}/memory/graph.db -orphans -apply
//
// 清理生产库前请先备份：sqlite3 graph.db ".backup 'graph.db.bak-<ts>'"
// 不要用 cp —— WAL 模式下会复制出主库与 -wal 不一致的快照。
package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func main() {
	path := flag.String("db", "", "graph.db 路径（必填）")
	apply := flag.Bool("apply", false, "真正删除；不加则只 dry-run 打印")
	orphans := flag.Bool("orphans", false, "同时处理「零关系孤立实体」（先被清理的噪音在另一端留下的空节点）")
	tagScene := flag.String("tag-scene", "", "存量引导：把实体名匹配 -entity-glob 的活跃关系标进该场景键（如 chan:qq）")
	entityGlob := flag.String("entity-glob", "", "配合 -tag-scene 的 GLOB 模式（如 *QQ*）。GLOB 区分大小写，避免把 /data/homeagent 这类路径卷进场景")
	sceneStats := flag.Bool("scene-stats", false, "只打印场景规模摘要")
	flag.Parse()

	if *path == "" {
		flag.Usage()
		log.Fatal("memgc: 必须指定 -db")
	}

	g, err := memory.NewGraphDB(*path)
	if err != nil {
		log.Fatalf("memgc: open %s: %v", *path, err)
	}
	defer g.Close()

	if *sceneStats {
		stats, err := g.SceneStats()
		if err != nil {
			log.Fatalf("memgc: scene stats: %v", err)
		}
		fmt.Printf("场景 %d 个：\n", len(stats))
		for _, st := range stats {
			fmt.Printf("  [%-9s] %-40s refs=%-5d rel=%-5d ent=%-4d strength=%-4d features=%-3d updated=%s\n",
				st.Origin, st.Key, st.Refs, st.Relations, st.Entities, st.Strength, st.Features,
				st.UpdatedAt.Format("2006-01-02 15:04"))
		}
		return
	}

	// 存量引导：场景是后引入的维度，老库里的规则（那批 QQ 规则就是典型）
	// 没有任何场景引用，不补挂就永远吃不到场景召回。
	if *tagScene != "" {
		if *entityGlob == "" {
			log.Fatal("memgc: -tag-scene 需要配套 -entity-glob（如 '*QQ*'）；不做自动猜测")
		}
		n, err := g.TagSceneByEntityGlob(*tagScene, *entityGlob, !*apply)
		if err != nil {
			log.Fatalf("memgc: tag scene: %v", err)
		}
		if *apply {
			fmt.Printf("[APPLIED] 已把 %d 条关系标进场景 %q\n", n, *tagScene)
		} else {
			fmt.Printf("[DRY-RUN] 将把 %d 条关系标进场景 %q（未写库）\n", n, *tagScene)
		}
		return
	}

	junk, err := g.NoiseEntities()
	if err != nil {
		log.Fatalf("memgc: scan: %v", err)
	}

	fmt.Printf("噪音实体 %d 个：\n", len(junk))
	for _, e := range junk {
		fmt.Printf("  %-64s type=%-8s mentions=%d\n", e.Name, e.Type, e.MentionCount)
	}

	de, dr, err := g.PurgeNoise(!*apply)
	if err != nil {
		log.Fatalf("memgc: purge: %v", err)
	}
	if *apply {
		fmt.Printf("[APPLIED] 噪音：已删除 实体=%d 关系=%d\n", de, dr)
	} else {
		fmt.Printf("[DRY-RUN] 噪音：将删除 实体=%d 关系=%d（未写库，加 -apply 才落地）\n", de, dr)
	}

	if *orphans {
		list, err := g.OrphanEntities()
		if err != nil {
			log.Fatalf("memgc: orphans: %v", err)
		}
		fmt.Printf("孤立实体（零关系）%d 个：\n", len(list))
		for _, e := range list {
			fmt.Printf("  %-64s type=%-8s mentions=%d\n", e.Name, e.Type, e.MentionCount)
		}
		n, err := g.PurgeOrphans(!*apply)
		if err != nil {
			log.Fatalf("memgc: purge orphans: %v", err)
		}
		if *apply {
			fmt.Printf("[APPLIED] 孤立实体：已删除 %d 个\n", n)
		} else {
			fmt.Printf("[DRY-RUN] 孤立实体：将删除 %d 个\n", n)
		}
	}

	if *apply {
		fmt.Println("提示：运行中的进程会在下一个 archive 心跳（Indexer.Sync）重建实体名向量索引，无需重启。")
	}
}
