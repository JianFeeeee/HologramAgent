package update

// 版本号工具：**子系统内唯一一份**语义版本实现。
//
// ## 为什么必须收敛到一份（合并前的教训）
//
// 合并前有 6 个函数散在两个包里做同一件事：
//
//	internal/kernelupgrade: ParseSemver / CmpSemver / versionNewer / trimTagV
//	internal/pluginupdate:  trimV / isNewer / semverNums
//
// 三份实现的差异是真实的 bug 温床：
//   · kernel 侧只挡 len<2 ⇒ "1.4" 被当成 1.4.0（已修）
//   · plugin 侧用 map+正则另写一套，行为与 kernel 侧不一致
//   · trimV / trimTagV 名字不同、逻辑相同
//
// 版本比较错一次的后果很具体：把"已是最新"说成"有新版"（白编一场），
// 或把"有新版本"说成"无需升级"（用户永远拿不到修复）。
//
// ⇒ 现在只导出一组：ParseSemver / CmpSemver / Newer / TrimTag。

import (
	"fmt"
	"regexp"
)

// Semver 是解析后的三段式版本号。
type Semver struct{ Major, Minor, Patch int }

var pureSemverRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// ParseSemver 解析纯 X.Y.Z（可带 v 前缀）。
//
// 返回 nil 表示"不是可比的三段式版本"——调用方应据此降级
// （如退回字符串比较、或明确告知无法比较），而不是猜一个值。
//
// ★ 必须恰好三段。早期实现只挡 len<2，于是 "1.4" 漏过去、
// 缺失的 patch 被零值填成 1.4.0 ⇒ "1.4 → 1.4.0" 被判成完全相同，
// 进而说"无需升级"，而两者内容未必相同。
func ParseSemver(s string) *Semver {
	m := pureSemverRe.FindStringSubmatch(s)
	if m == nil {
		return nil
	}
	var v Semver
	if _, err := fmt.Sscanf(m[1]+"."+m[2]+"."+m[3], "%d.%d.%d", &v.Major, &v.Minor, &v.Patch); err != nil {
		return nil
	}
	return &v
}

// CmpSemver 比较两个已解析版本：<0 / 0 / >0。
//
// 任一侧为 nil 时返回 0（不可比）——调用方必须先判 nil，
// 因为"无法比较"与"相等"是两回事，混同会让升级判定失效。
func CmpSemver(a, b *Semver) int {
	if a == nil || b == nil {
		return 0
	}
	switch {
	case a.Major != b.Major:
		return a.Major - b.Major
	case a.Minor != b.Minor:
		return a.Minor - b.Minor
	default:
		return a.Patch - b.Patch
	}
}

// Newer 判断版本串 a 是否严格新于 b。
//
// 语义要点：
//   - 只认纯三段式；任一侧不可解析 ⇒ false（不猜）
//   - "1.4.0" 与 "1.4.0dev" 视为**相同**（TrimTag 去 dev 后比）
//     —— 开发分支通常领先于同名正式版，不误报"有新版"
//   - 空串 b 表示"本地无版本信息"，此时 a 非空即算新
func Newer(a, b string) bool {
	if b == "" {
		return a != ""
	}
	pa, pb := ParseSemver(TrimTag(a)), ParseSemver(TrimTag(b))
	if pa == nil || pb == nil {
		return false
	}
	return CmpSemver(pa, pb) > 0
}

// TrimTag 把版本串归一成可比形态：去 v 前缀、去 dev/预发布后缀。
//
// 处理 "v1.4.4" → "1.4.4"、"1.4.0dev" → "1.4.0"。
// 非纯三段式（如 "1.4.4-rc1"、"release1"）原样返回，
// 由 ParseSemver 判为不可比 —— 这里不做有损猜测。
func TrimTag(s string) string {
	out := s
	if len(out) > 0 && (out[0] == 'v' || out[0] == 'V') {
		out = out[1:]
	}
	// 去 dev 及其后（1.4.0dev / 1.4.0dev.1）
	if i := indexOf(out, "dev"); i >= 0 {
		out = out[:i]
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
