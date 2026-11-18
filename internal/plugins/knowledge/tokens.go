package knowledgeplugin

// tokens.go —— 知识库**自己的**分词/关键词实现（用户裁定 2026-11-18）。
//
// ## 为什么不能调 memory.*
//
// 用户原话：「knowledge 不应该作为一个内核模块，而是应该作为一个内部插件，
// 因为它并不必须依附内核内部接口实现，而是可以通过 sdk 完美实现功能」
// ＋「同时移出内核后相关计算要它自己实现」＋「kbtree 直接合入知识库插件」。
//
// 判据（§0 第 11 条）：**能不能只用 SDK 完整实现？能 ⇒ 内部插件，且它的计算
// 也归它自己**。分词与关键词提取是纯计算 —— 所以它必须住在这里，
// 不许再向 `internal/memory` 要（那个调用是"我还依赖内核"的实证）。
//
// ## 依赖了什么，没依赖什么
//
// 用了 jieba（`gojieba`）—— 那是**第三方库**，不是内核能力；插件用第三方库
// 完全正当（与用 net/http 一样）。**不用**的是 `memory.TokenizeWords` /
// `memory.ExtractKeywords` / `memory.MediaBlockID` 这些**内核内部的语义约定**。
//
// ## 与 memory 的差异（刻意的，不是遗漏）
//
//   - 停用词表、词性白名单是**本插件自己的**（知识条目与对话消息的语料分布
//     不同：知识库更偏术语，对话更偏口语）。
//   - `CleanText` 只做"去首尾空白 + 去开头的中文逗号"，不做 memory 的额外清洗
//     （那是给对话输入准备的）。

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yanyiwu/gojieba"
)

// contentPOS 是**知识库**保留的实义词性（名词/动词/形容词/专名等）。
//
// 与对话侧的区别：知识条目里专名（nr/ns/nt/nz）与符号名（eng）权重更高，
// 而去掉"语气词/助词"这类对话噪声。
var contentPOS = map[string]bool{
	"n": true, "nr": true, "ns": true, "nt": true, "nw": true, "nz": true,
	"v": true, "vd": true, "vn": true,
	"a": true, "ad": true, "an": true,
	"i": true, "l": true, "j": true, "s": true, "f": true, "b": true,
	"z": true, "t": true,
	"eng": true, "x": true, "zg": true, "un": true,
}

// stopWords 是知识库侧的停用词（中文虚词 + 英文功能词）。
//
// ★ 刻意比对话侧**更短**：知识库里 "文件"、"内容"、"消息" 这些词往往就是
// 条目主题（"文件系统"、"消息队列"），当停用词会把主题词删掉。
var stopWords = map[string]bool{
	"的": true, "了": true, "是": true, "在": true, "和": true, "与": true,
	"或": true, "有": true, "个": true, "这": true, "那": true, "我": true,
	"你": true, "他": true, "它": true, "就": true, "都": true, "也": true,
	"很": true, "对": true, "从": true, "到": true, "被": true, "把": true,
	"一个": true, "我们": true, "他们": true, "可以": true, "因为": true,
	"所以": true, "但是": true, "如果": true, "这个": true, "那个": true,
	"the": true, "a": true, "an": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "been": true, "of": true, "to": true, "in": true, "on": true, "at": true,
	"for": true, "with": true, "and": true, "or": true, "but": true, "it": true, "its": true,
	"this": true, "that": true, "these": true, "those": true,
}

var (
	kbJiebaOnce sync.Once
	kbJieba     *gojieba.Jieba
)

// kbJiebaInstance 返回知识库自己的 jieba 实例（懒初始化，失败即 nil）。
//
// ★ 为什么不复用 memory.GetJieba()：那会让"插件依赖内核单例"重新成立。
//
//	代价是可能多加载一份词典（实测约 40MB 常驻），但这是"计算归自己"的
//	直接代价 —— 而收益是它能在没有 memory 的环境里独立工作（例如插件被
//	单独装配、或将来真的走 SDK 独立进程）。
func kbJiebaInstance() *gojieba.Jieba {
	kbJiebaOnce.Do(func() {
		kbJieba = newJieba()
	})
	return kbJieba
}

// TokenizeWords 精确模式分词（去重，不过滤停用词）。
func TokenizeWords(text string) []string {
	text = CleanText(text)
	x := kbJiebaInstance()
	if x == nil {
		return nil
	}
	words := x.Cut(text, false)
	seen := make(map[string]bool, len(words))
	out := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// ExtractKeywords 词性过滤 + 停用词过滤后的关键词（用于标签与词法路）。
func ExtractKeywords(text string) []string {
	text = CleanText(text)
	x := kbJiebaInstance()
	if x == nil {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	for _, t := range x.Tag(text) {
		idx := strings.LastIndex(t, "/")
		if idx < 0 {
			continue
		}
		word, tag := strings.TrimSpace(t[:idx]), t[idx+1:]
		if word == "" || seen[word] || stopWords[word] {
			continue
		}
		if utf8.RuneCountInString(word) < 2 {
			continue
		}
		if !contentPOS[tag] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

// CleanText 做**最小清洗**：去首尾空白 + 去开头的中文逗号。
//
// 为什么不做更多：知识条目的正文是用户写下的文档，过度清洗会改变它的语义
// （例如把代码片段里的空行去掉）。对话侧的清洗规则（`memory.CleanText`）
// 是为聊天输入准备的，不适用于文档。
func CleanText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "，")
	return strings.TrimSpace(text)
}

// MediaBlockID 给媒体算内容身份（与内核媒体块 ID **同构但不共享实现**）。
//
// ★ 为什么自己算：`memory.MediaBlockID` 是内核的记忆块身份约定。知识库的
// 媒体身份只需要"同一份字节 ⇒ 同一个 id"，不需要与图记忆共享命名空间 ——
// 共享反而会让"库里没有这条块"与"图里没有这条块"看起来一样。
func MediaBlockID(mime string, data []byte) string {
	// 与内核同构：`blk_media_<sha1 前 16 hex>`，幂等且内容派生。
	const prefix = "blk_media_"
	h := sha1Hex(data)
	return prefix + h[:16]
}

// sha1Hex 返回字节的 sha1 十六进制串（MediaBlockID 用）。
func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}
