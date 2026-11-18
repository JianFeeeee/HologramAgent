// Package scene 是**场景键的构造规则**（叶子包，零内部依赖）。
//
// ## 为什么它必须是独立叶子包
//
// 这些规则（`qq` → `chan:qq`、大小写归一、层内非法字符归一）有**两个消费方**：
//
//	internal/memory          存储侧（写/查挂场景）
//	internal/agent           归档/蒸馏侧（`coldDocToTriples` 要按来源钉场景）
//
// 而基座（`internal/agent/agent`）**不能** import `internal/memory` ——
// 那是一个带 SQLite 与 ONNX 的实现包（判据 §8 第 3 条）。
//
// 也不能放进 `agenttypes`：那边 import 了 `internal/sdk`，而 sdk 又 import
// memory ⇒ 环（实测撞过）。所以它必须是**零内部依赖**的叶子。
//
// 规则与 `internal/memory/scene.go` 的实现在本包**逐字一致**（含长度上限
// 与归一字符集），memory 侧改为转发本包 ⇒ 同一规则只有一份。

package scene

import (
	"strings"
	"unicode"
)

// MaxKeyLen 是场景键的长度上限。场景键要进索引、要参与前缀比较，
// 过长只说明有人把正文塞进了键里（那就该用句子/实体，而不是场景）。
const MaxKeyLen = 96

// Normalize 规范化场景键：按 `/` 分层、每层小写、层内空白与非法字符
// 归一成 `_`（连续多个只留一个）。
//
// 为什么要归一：场景键是**索引键**，`chan:QQ` 与 `chan:qq` 必须是同一个场景，
// 否则同一条规则会因为写入时大小写不同而分裂成两个召不齐的场景。
//
// 归一结果为空（全是非法字符）时返回空串，调用方应视为「没有场景」。
func Normalize(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	key = strings.ToLower(key)

	parts := strings.Split(key, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		seg := normalizeSegment(p)
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	out := strings.Join(segs, "/")
	if len(out) > MaxKeyLen {
		out = strings.TrimRight(out[:MaxKeyLen], "/")
	}
	return out
}

// normalizeSegment 归一单层场景：小写、空白与非法字符 → `_`（压缩连续）。
func normalizeSegment(seg string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= '\u4e00' && r <= '\u9fff',
			r == ':' || r == '-' || r == '.':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '_' || unicode.IsSpace(r):
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		default:
			// 其它字符（标点、表情）归一成 `_` 而不是静默丢弃：
			// 「A/B」与「A_B」是两个不同的来源，不能塌成一个场景。
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// Channel 由输入/输出通道名构造场景键（`qq` → `chan:qq`）。
func Channel(source string) string {
	s := Normalize(source)
	if s == "" {
		return ""
	}
	return "chan:" + s
}

// Tool 由工具名构造场景键（`qq_get_message` → `tool:qq_get_message`）。
func Tool(tool string) string {
	s := Normalize(tool)
	if s == "" {
		return ""
	}
	return "tool:" + s
}
