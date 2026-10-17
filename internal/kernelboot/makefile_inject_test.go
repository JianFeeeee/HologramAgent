package kernelboot

// Makefile 的**编译期 hash 注入**判据（2026-10-17）。
//
// ## 守什么
//
// `make build` 必须把**真实的**依赖图 digest 注入 homed 的
// `kernelboot.EmbeddedGraphHash`。若注入失败（值为空、或变成字面量
// `$GRAPH_HASH`），内核启动时会做配套校验：
//
//     磁盘图的 hash: 44823b02…
//     二进制内嵌的:  $GRAPH_HASH
//     ⇒ ★ 依赖图与本二进制**不配套** ⇒ **拒绝启动**
//
// ## ★ 为什么必须有这条（实测缺陷的形态）
//
// 原 Makefile 把整串 ldflags 包在**单引号**里：
//
//     -ldflags '... -X ...EmbeddedGraphHash=$$GRAPH_HASH'
//
// shell 里**单引号不做变量展开** ⇒ 注入的是**字面量** `$GRAPH_HASH`。
//
// 这个缺陷最难的地方是「**所有表面信号都是绿的**」：
//   · `make build` 退出码 0
//   · 日志打印 `已注入启动依赖图 hash: 44823b02…`（那是 echo，不是注入结果）
//   · 依赖图文件正常生成、自校验通过
// 只有**真跑内核**才炸，而且报错指向"改了 Requires 没重出图"——**方向是错的**。
//
// ⇒ 判据必须查两件事：
//   ① Makefile 里注入 hash 用的引号**允许展开**（不能是单引号包 $$VAR）
//   ② 构建产物里**没有**字面量 `$GRAPH_HASH` 残留

import (
	"os"
	"strings"
	"testing"
)

// ★★★ 判据：Makefile 注入 hash 时不得用单引号包 `$$GRAPH_HASH`。
//
// ★ 为什么是源码判据而不是"跑一次 make build 再看二进制"：
//
//	跑构建很贵（分钟级 + 需要 cgo/onnx），不适合放进单测；
//	而这里要断言的是**一个引号选择**，属于"写法模式存在性"，
//	源码断言**精确且便宜**。真正的端到端验证由发版流程做
//	（见 README 的「构建后必须真跑一次内核」）。
func TestMakefileGraphHashIsShellExpandable(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Skipf("读不到 Makefile: %v", err)
	}
	src := string(b)

	// 定位注入行
	idx := strings.Index(src, "EmbeddedGraphHash=$$GRAPH_HASH")
	if idx < 0 {
		t.Fatal("Makefile 里找不到 EmbeddedGraphHash=$$GRAPH_HASH 的注入点 —— " +
			"若改了注入方式，请同步更新本判据（不要删掉它）")
	}
	// 取该行
	lineStart := strings.LastIndex(src[:idx], "\n") + 1
	lineEnd := strings.Index(src[idx:], "\n")
	if lineEnd < 0 {
		lineEnd = len(src) - idx
	}
	line := src[lineStart : idx+lineEnd]

	// ★ 核心断言：该行里 `$$GRAPH_HASH` 前面不能是**单引号开启的引用段**。
	// 判定方式：检查 `-ldflags` 到 `$$GRAPH_HASH` 之间的引号是否平衡，
	// 若在到达 $$GRAPH_HASH 之前遇到**奇数个**单引号 ⇒ 它处在单引号里 ⇒ 不展开。
	ld := strings.Index(line, "-ldflags")
	if ld < 0 {
		t.Fatalf("注入行里没有 -ldflags: %s", line)
	}
	seg := line[ld:strings.Index(line, "$$GRAPH_HASH")]
	sq := strings.Count(seg, "'")
	dq := strings.Count(seg, `"`)
	if sq%2 == 1 {
		t.Fatalf("★ -ldflags 段用**单引号**包住了 $$GRAPH_HASH ⇒ shell 不做变量展开\n"+
			"   ⇒ 注入进二进制的是**字面量** `$GRAPH_HASH` ⇒ 内核启动时\n"+
			"   「磁盘图 digest」vs「内嵌 $GRAPH_HASH」不等 ⇒ **拒绝启动**。\n"+
			"   （而 make build 日志全绿，只有真跑内核才暴露）\n"+
			"   修法：改用双引号包 ldflags。\n   实际行: %s", line)
	}
	// 正向：应当有双引号（允许展开）
	if dq%2 == 0 {
		t.Logf("提示：本行未用双引号包 ldflags（若用无引号形式且无空格也可接受）: %s", line)
	}
}

// ★★★ 判据：构建产物里**没有**字面量 `$GRAPH_HASH` 残留。
//
// 需要 build/homed 存在（跑过 make build）。不存在则跳过 ——
// 这条是**补充**判据，主判据是上面那条源码检查。
func TestBuiltHomedHasNoLiteralGraphHash(t *testing.T) {
	b, err := os.ReadFile("../../build/homed")
	if err != nil {
		t.Skipf("未构建 build/homed（跳过）: %v", err)
	}
	if strings.Contains(string(b), "$GRAPH_HASH") {
		t.Fatal("★ build/homed 里含**字面量** `$GRAPH_HASH` —— " +
			"说明 -ldflags 注入没展开（单引号问题）⇒ 内核会拒绝启动")
	}
	// 正向：必须含有某个 64 位 hex（注入成功）
	hasHex := false
	for i := 0; i+64 <= len(b); i++ {
		ok := true
		for j := i; j < i+64; j++ {
			c := b[j]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				ok = false
				break
			}
		}
		if ok {
			hasHex = true
			break
		}
	}
	if !hasHex {
		t.Error("build/homed 里找不到 64 位 hex（依赖图 digest 未注入？）")
	}
}
