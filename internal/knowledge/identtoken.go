package knowledge

import (
	"regexp"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// asciiRunRe 匹配一段连续的 ASCII 标识符/单词（允许 . _ - / 作分隔符）。
//
// 为什么用正则扫原文而不是复用 jieba 的结果：jieba 对英文标识符是**按字符切**的
// （实测 `PruneOldBackups` → `[P r u n e O l d B a c k p s]`，还顺带去重），
// 词法路因此对标识符完全失效 —— 任意两篇含英文字母的文档 token 重叠率都能到 50%。
var asciiRunRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_.\-/]*`)

// identSepRe 是标识符内的分隔符集合（不含空格：空格是词边界，不是标识符内部分隔）。
var identSepRe = regexp.MustCompile(`[_.\-/]+`)

// identifierTokenizer 是知识库**词法路专用**的分词器。
//
// # 它解决的问题（2026-10-31 实测）
//
// 2026-10-08 把检索改成「稠密路唯一」，理由是词法路对英文标识符失效、而
// 逐路归一化又会把弱信号抬到与稠密路等权。前一半是**分词器的缺陷**，不是
// 词法路本身的属性。实测（真实 196 条库）：
//
//	query=PruneOldBackups  → 目标条目从未进前五，返回的是 cangjie 文档
//	query=minSceneEvidence → 同样从未命中
//
// 因为查询被切成单字符，而 `hologramagent_kernel_assembly_upgrade` 里
// 那个唯一的 `PruneOldBackups` 在单字符空间里与其他文档没有区别。
//
// # 做法
//
// 在 jieba 词之外，**额外**补两类 token：
//
//	· 标识符整词：`PruneOldBackups` → `pruneoldbackups`、`strip_path` → `strip_path`
//	· 分隔符部件：`strip_path` → `strip`、`path`（仅当下划线/点/斜杠存在时）
//
// 刻意**不拆 camelCase**：拆了会把 `PruneOldBackups` 拆出 `old`/`backups`，
// 而 `old` 在 `cangjie/tools/cmd-tools/cjcompat_manual` 里是 CLI 参数名 `--old`，
// 出现 31 次 —— 拆 camelCase 反而让无关文档拿到高分（实测该文档 0.54 > 目标 0.20）。
//
// 同时丢掉**单个 ASCII 字母**的 token：那是 jieba 字符切分的残渣，只制造噪声。
//
// 中文部分完全沿用 jieba，行为不变。
func identifierTokenizer(text string) []string {
	base := memory.TokenizeWords(text)

	out := make([]string, 0, len(base)+8)
	seen := make(map[string]bool, len(base)+8)
	add := func(tok string) {
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		out = append(out, tok)
	}

	for _, t := range base {
		if isSingleASCIILetter(t) {
			continue // jieba 把标识符切碎的残渣
		}
		add(t)
	}

	for _, run := range asciiRunRe.FindAllString(text, -1) {
		lower := strings.ToLower(run)
		// 整词：长度 >= 3 才收（`go`/`io` 这类太泛，收进来只增噪声）。
		if len(lower) >= 3 {
			add(lower)
		}
		// 部件：只有真的带分隔符才拆；`PruneOldBackups` 这种无分隔符的整词
		// 已经由上一行覆盖，拆 camelCase 反而有害（见函数注释）。
		parts := identSepRe.Split(lower, -1)
		if len(parts) < 2 {
			continue
		}
		for _, p := range parts {
			if len(p) >= 3 {
				add(p)
			}
		}
	}
	return out
}

// isSingleASCIILetter 报告 token 是不是「单个 ASCII 字母」。
func isSingleASCIILetter(t string) bool {
	if len(t) != 1 {
		return false
	}
	c := t[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
