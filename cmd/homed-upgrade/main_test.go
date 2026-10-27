package main

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// ── 版本比较的判据 ────────────────────────────────────────────
//
// 这些是 OTA 敢不敢按 -apply 的依据。判错一次就可能把新版换成旧版，
// 或让"内容其实没变"被当成升级成功。

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		name    string
		cur     *update.BinVersion
		nv      *update.BinVersion
		wantSub string // 结论文案必须包含的片段
	}{
		{
			name:    "正常升级",
			cur:     &update.BinVersion{Version: "1.3.0", Commit: "aaa"},
			nv:      &update.BinVersion{Version: "1.4.0", Commit: "bbb"},
			wantSub: "可升级",
		},
		{
			name:    "降级必须显式警告",
			cur:     &update.BinVersion{Version: "1.4.0", Commit: "aaa"},
			nv:      &update.BinVersion{Version: "1.3.0", Commit: "bbb"},
			wantSub: "降级",
		},
		{
			name:    "同版本同 commit = 无需升级",
			cur:     &update.BinVersion{Version: "1.4.0", Commit: "aaa"},
			nv:      &update.BinVersion{Version: "1.4.0", Commit: "aaa"},
			wantSub: "完全相同",
		},
		{
			// ★ 这条最容易被忽略：版本号没变但内容变了（同版本重发）。
			//   不报出来会让用户以为"升级了其实没升"或反之。
			name:    "同版本不同 commit 必须点明",
			cur:     &update.BinVersion{Version: "1.4.0", Commit: "aaa"},
			nv:      &update.BinVersion{Version: "1.4.0", Commit: "bbb"},
			wantSub: "同版本重发",
		},
		{
			// 非标准形态（1.4.0dev）：走字符串回退，不能说"降级"
			name:    "非标准版本号走字符串比较",
			cur:     &update.BinVersion{Version: "1.4.0", Commit: "aaa"},
			nv:      &update.BinVersion{Version: "1.4.0dev", Commit: "bbb"},
			wantSub: "commit 不同",
		},
	}
	for _, c := range cases {
		got := update.CompareVersions(c.cur, c.nv)
		if !strings.Contains(got, c.wantSub) {
			t.Errorf("%s: 结论应含 %q，实际 %q", c.name, c.wantSub, got)
		}
	}
}

// 判据：读不到当前版本时**不能**阻断（第一次升级必然撞到）。
//
// 这是实测踩到的：线上内核是加 --version 之前的版本，第一次探测必然失败。
// 若此时直接退出，所有用户第一次升级都要人工介入。
func TestCompareVersions_UnknownCurrentDoesNotBlock(t *testing.T) {
	got := update.CompareVersions(nil, &update.BinVersion{Version: "1.4.0", Commit: "bbb"})
	if strings.Contains(got, "❌") {
		t.Errorf("当前版本未知不应判失败，实际: %s", got)
	}
	if !strings.Contains(got, "跳过比较") {
		t.Errorf("应说明跳过比较，实际: %s", got)
	}
}

// ── 版本解析 ─────────────────────────────────────────────────

func TestParseSemver(t *testing.T) {
	// 只测**可观察行为**（返回 nil 与否），不探私有字段 ——
	// 跨包断言内部字段会把重构锁死，而"解析对不对"最终要由
	// CompareVersions 的行为体现（上面那组用例已覆盖）。
	if update.ParseSemver("1.4.0") == nil {
		t.Error("1.4.0 应能解析")
	}
	// dev 后缀不算正式语义版本（否则 1.4.0dev 会被当成 1.4.0 比大小）
	if v := update.ParseSemver("1.4.0dev"); v != nil {
		t.Errorf("1.4.0dev 应回退字符串比较，实际解析成 %+v", v)
	}
	// ★ "v1.4.0" 现在**算**合法：GitHub 的 release tag 就是 v 前缀形态
	//（hologramagent 的 tag 全部形如 v1.4.0）。早期实现不认 v 前缀，
	// 于是 CLI 对"当前 v1.4.0 vs 包内 v1.4.0"判不可比、只能退回字符串比较。
	//
	// 不完整串同样不算
	for _, bad := range []string{"", "1", "1.4", "v1.4", "1.4.x", "1.4.4.4"} {
		if v := update.ParseSemver(bad); v != nil {
			t.Errorf("%q 不应被解析成语义版本，实际 %+v", bad, v)
		}
	}
}

// ── waiter 兜底解析 ──────────────────────────────────────────

func TestFirstSemverLike(t *testing.T) {
	// waiter 的真实输出形态
	line := "waiter 1.4.0 (commit 98850e1, built 2026-10-17T09:20:25Z)"
	if got := update.FirstSemverLike(line); got != "1.4.0" {
		t.Errorf("应从 waiter 输出取到 1.4.0，实际 %q", got)
	}
	// 不能把日期当版本号
	if got := update.FirstSemverLike("built 2026-10-17"); got != "" {
		t.Errorf("日期不应被识别为版本号，实际 %q", got)
	}
}
