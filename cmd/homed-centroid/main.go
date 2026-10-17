// homed-centroid 重建中心向量。
//
// ★ 为什么需要独立命令
// ---------------------
// RebuildCentroid 此前**只有测试在调用**，生产路径没有调用者：
// homed-graph-migrate 与 homed-graph-distill 都已接上自动重建，
// 但仍需要独立入口处理这几种情况：
//
//   - 向量回填之后（回填脚本不重建中心）
//   - 换了 provider（指纹变了，旧中心自动失效但没人建新的）
//   - 库增长后中心过期（中心是块集的均值，块集变了它就偏）
//
// 用法：
//
//	homed-centroid -db <graph.db>                      # 自动取库内指纹
//	homed-centroid -db <graph.db> -fingerprint <hex>   # 显式指定
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
)

func main() {
	dbPath := flag.String("db", "", "graph.db 路径")
	explicitFP := flag.String("fingerprint", "", "provider 指纹（缺省取库内最多的）")
	flag.Parse()

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "错误：需要 -db")
		os.Exit(1)
	}
	db, err := memory.NewGraphDB(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：打开 %s 失败: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	fp := *explicitFP
	if fp == "" {
		fp, err = db.DominantBlockFingerprint()
		if err != nil {
			fmt.Fprintf(os.Stderr, "错误：读取库内指纹失败: %v\n", err)
			os.Exit(1)
		}
		if fp == "" {
			fmt.Fprintln(os.Stderr,
				"错误：库内没有带指纹的块 —— 先跑 homed-graph-migrate -apply 写入向量")
			os.Exit(1)
		}
		fmt.Printf("用库内指纹：%s\n", fp)
	}

	c, anomalous, err := db.RebuildCentroid(fp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：重建中心失败: %v\n", err)
		os.Exit(1)
	}
	st := c.Stats()
	fmt.Printf("已重建中心：均值向量长度 %.4f，平均两两余弦 %.4f\n",
		st.MeanLength, st.AvgPairCosine)
	fmt.Printf("  启用双边中心化: %v\n", anomalous)
	if !anomalous {
		fmt.Println("  （各向异性不显著，召回不会做中心化；中心已存备用）")
	}
}
