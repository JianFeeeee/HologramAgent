package ctxmgr

// util.go —— 本包自带的小工具（不 import agent，避免成环）。
//
// ★ 为什么内联而不是共用基座的同名函数：`truncateStr` 与 `noMemoryOf`
//   住在 `agent` 包里，而本包**不能** import 它（agent import ctxmgr）。
//   两者都是几行的纯函数，复制一份并**用判据钉住一致**是可接受的唯一选择
//   （与 `sentenceBlockIDOf` 同一处置）。

import "unicode/utf8"

// truncateStr 按**字符**截断字符串（保留前 max 个 rune + "..."）。
func truncateStr(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	var truncated int
	for i := range s {
		if truncated >= max {
			return s[:i] + "..."
		}
		truncated++
	}
	return s
}

// NoMemoryFunc 是「某工具的输出是否不进记忆计算」的查询面。
//
// ★ 由调用方注入（基座的包级声明表 + StageHost 的插件声明）：
//
//	本包不认识工具表，只认识这个判定。
type NoMemoryFunc func(name string) bool

// noMemoryOf 是本包的包级默认（永远返回 false = 全部参与记忆）。
//
// 调用方（构造函数）用 `WithNoMemory` 覆盖它。零值语义与基座一致
// （"未登记 ⇒ 参与记忆"，保守）。
var noMemoryOf NoMemoryFunc = func(string) bool { return false }

// WithNoMemory 设置"是否不进记忆"的查询面（构造期一次）。
//
// 之所以是包级而不是字段：`textForVector` 是**包级函数**（它被
// `computeVector` 与若干判据直接调用），加字段要改所有签名。
func WithNoMemory(fn NoMemoryFunc) {
	if fn != nil {
		noMemoryOf = fn
	}
}
