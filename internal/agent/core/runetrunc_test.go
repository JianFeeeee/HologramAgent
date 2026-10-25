package core

// 全仓守卫：按字节截断字符串是中文环境下的**必然 bug**。
//
// 中文占 3 字节，`s[:N]` 的切点常落在多字节字符中间 ⇒ 产生非法 UTF-8。
// 实测代价（不是理论风险）：
//
//	skillmgr: desc[:80] 把 25 个 skill 里 8 个（32%）的索引行切坏，
//	          模型看到 "…以后一次�"，且"什么时候该用"整段丢失
//	          ⇒ e2e 实测该用 seq 的场景，模型改去调别的工具
//	tooldefs: desc[:60] 同样问题
//	knowledge/distill: preview[:200] / raw[:400] 同样问题
//
// 正确写法：先转 []rune 再切。本判据扫描源码，发现新的字节截断即失败。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// byteSliceRe 匹配 `ident[:数字]` 形式（字节切片的典型写法）。
var byteSliceRe = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_.]*)\[:\s*(\d+)\]`)

// byteSliceAllow 是**已知安全**的例外：切的是 ASCII 生成物或切片重置。
var byteSliceAllow = map[string]bool{
	// 文件路径/哈希/hex 全是 ASCII
	"fp": true, "h": true, "id": true, "d": true, "digest": true,
	"sum": true, "shortFP": true, "head": true, "monthKey": true,
	// `x[:0]` 是"清空切片并复用底层数组"的惯用法，不涉及字符
	"msgs": true, "args": true, "queue": true, "pending": true,
	"Children": true, "changeSets": true, "keep": true, "out": true,
	"b": true, "buf": true, "data": true,
	// st.Names 是 []string（工具结果的名称**列表**），切它是切片截断，
	// 不涉及字符编码。误报清单要写明依据，否则以后没人敢再动这里。
	"show": true,
}

func TestNoByteLevelStringTruncation(t *testing.T) {
	root := ".."
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for i, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			// 跳过注释（我们的说明里会引用这个模式本身）
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			for _, m := range byteSliceRe.FindAllStringSubmatch(line, -1) {
				ident, n := m[1], m[2]
				// `[:0]` 是切片重置，不是字符截断
				if n == "0" {
					continue
				}
				if byteSliceAllow[ident] {
					continue
				}
				// 形如 `string(r[:60])` 且 r 来自 []rune 的写法：变量名含 rune 约定
				if ident == "r" {
					continue
				}
				offenders = append(offenders, filepath.Join(path)+":"+strconv.Itoa(i+1)+"  "+trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("发现 %d 处疑似**按字节**截断字符串（中文环境下会产生非法 UTF-8）：\n  %s\n\n"+
			"正确写法：先转 []rune 再切，例如 r := []rune(s); s = string(r[:N]) + \"...\"\n"+
			"若确实是 ASCII 生成物（hash/路径）或切片重置（x[:0]），加进 byteSliceAllow 并注明理由。",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
