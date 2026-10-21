// homed-depgraph 产出内核模块的**启动依赖图制品**（设计 §2.5）。
//
// ## 用户的设计（原话）
//
// > 启动顺序是编译期间按照声明的依赖顺序生成的启动依赖图排列的
//
// 本工具就是那个"编译期生成器"：
//
//	① import 模块登记表（internal/kernelmod/all ⇒ 各模块 init 自注册）
//	② 用各模块**自己的** Requires()/Provides() 算分层（BFS）
//	③ 把【模块 + 边 + 层号】编成 depsgraph.bin，并输出 digest
//
// 运行期内核**读**这份图即可（不再自己算顺序）。
//
// ## 用法
//
//	homed-depgraph -out <homed 目录>/depgraph.bin   # 产出制品
//	homed-depgraph -print                           # 只打印计划（评审用）
//	homed-depgraph -digest                          # 只输出 digest（供 -ldflags 注入）
//
// ## ★ 拒绝产出空图 / 无解图
//
// 空图会让内核认为"没有依赖" ⇒ 按一个**错误的计划**启动 —— 比报错危险得多。
// 依赖图无解（缺依赖/环/重名）时也拒绝产出：那是结构性错误，
// 应在编译期就报出来，而不是留给运行期。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"

	// ★ 把全部内核模块**引入编译** —— 这就是"编译期收集各模块声明"的落点。
	// 各模块在自己的 init() 里 kernelmod.Register(...)。
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/all"
)

func main() {
	out := flag.String("out", "", "输出路径（默认 <二进制目录>/depgraph.bin）")
	printPlan := flag.Bool("print", false, "打印启动计划（不落盘）")
	digestOnly := flag.Bool("digest", false, "只输出 digest（供 -ldflags 注入）")
	flag.Parse()

	if d := kernelmod.Duplicates(); len(d) > 0 {
		fmt.Fprintf(os.Stderr, "homed-depgraph: 模块名重复注册: %v\n", d)
		os.Exit(1)
	}
	mods := kernelmod.Registered()
	if len(mods) == 0 {
		fmt.Fprintln(os.Stderr,
			"homed-depgraph: 没有任何模块被登记 —— 拒绝产出空图。\n"+
				"空图会让内核认为'没有依赖'，从而按一个**错误的计划**启动。\n"+
				"检查 internal/kernelmod/all 是否 import 了各模块包。")
		os.Exit(2)
	}

	// ★ 编译期就算出分层 —— 这是"顺序由编译期决定"的落点。
	plan, err := kernelboot.Build(mods)
	if err != nil {
		fmt.Fprintf(os.Stderr, "homed-depgraph: ★ 依赖图无解，拒绝产出制品:\n%v\n", err)
		os.Exit(1)
	}

	edges := planToEdges(plan)
	if *printPlan {
		fmt.Print(renderPlan(plan))
	}
	if *digestOnly {
		fmt.Println(kernelboot.GraphDigest(edges))
		return
	}

	data := kernelboot.EncodeGraph(edges)
	path := *out
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintf(os.Stderr, "homed-depgraph: 无法确定输出目录: %v\n", err)
			os.Exit(1)
		}
		path = filepath.Join(filepath.Dir(exe), kernelboot.GraphFileName)
	}
	// 写临时文件再 rename：避免半截文件被读到
	//（自校验也能拦住截断，但原子替换能避免"那一刻读到坏图"的噪音）。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "homed-depgraph: 写 %s: %v\n", tmp, err)
		os.Exit(1)
	}
	if err := os.Rename(tmp, path); err != nil {
		fmt.Fprintf(os.Stderr, "homed-depgraph: 替换 %s: %v\n", path, err)
		os.Exit(1)
	}
	fmt.Printf("已产出启动依赖图: %s（%d 个模块，%d 层）\n", path, len(edges), len(plan.Layers))
	fmt.Printf("digest（注入 -ldflags -X ...EmbeddedGraphHash=%s）\n",
		kernelboot.GraphDigest(edges))
}

// planToEdges 把分层计划转成图记录（含**编译期算好的层号**）。
func planToEdges(plan *kernelboot.Plan) []kernelboot.GraphEdge {
	var out []kernelboot.GraphEdge
	for layer, mods := range plan.Layers {
		for _, m := range mods {
			out = append(out, kernelboot.GraphEdge{
				Name:     m.Name(),
				Provides: m.Provides(),
				Requires: m.Requires(),
				Layer:    layer, // ★ 编译期定序
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Layer != out[j].Layer {
			return out[i].Layer < out[j].Layer
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// renderPlan 把分层计划渲染成**给人看**的文本（供 -print）。
//
// 格式刻意稳定：它会进 PR / 运维记录，因此同一份声明必须渲染出逐字相同的结果。
func renderPlan(plan *kernelboot.Plan) string {
	var b strings.Builder
	b.WriteString("内核模块启动计划（编译期由依赖声明算出，BFS 分层，同层并行）:\n")
	for i, layer := range plan.Layers {
		names := make([]string, 0, len(layer))
		for _, m := range layer {
			names = append(names, m.Name())
		}
		sort.Strings(names)
		fmt.Fprintf(&b, "  第 %d 层（%d 个，可并行）: %s\n", i, len(names), strings.Join(names, ", "))
	}
	return b.String()
}
