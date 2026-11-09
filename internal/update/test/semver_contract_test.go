package test

// internal/update 的黑盒判据：纯函数的输入 → 输出。
// 不读源码、不跑外部命令、不碰内部正则表。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// TestParseSemver：合法三段式（可带 v）解析；非纯版本返回 nil。
func TestParseSemver(t *testing.T) {
	cases := []struct {
		in            string
		wantNil       bool
		maj, min, pat int
	}{
		{"1.4.0", false, 1, 4, 0},
		{"v2.10.3", false, 2, 10, 3},
		{"1.4.0dev", true, 0, 0, 0},
		{"1.4", true, 0, 0, 0},
		{"abc", true, 0, 0, 0},
		{"", true, 0, 0, 0},
	}
	for _, c := range cases {
		got := update.ParseSemver(c.in)
		if c.wantNil {
			if got != nil {
				t.Errorf("★ ParseSemver(%q) = %+v，期望 nil", c.in, got)
			}
			continue
		}
		if got == nil || got.Major != c.maj || got.Minor != c.min || got.Patch != c.pat {
			t.Errorf("★ ParseSemver(%q) = %+v", c.in, got)
		}
	}
}

// TestCmpSemver：大小与相等；nil 返回 0（不可比）。
func TestCmpSemver(t *testing.T) {
	a := update.ParseSemver("1.4.0")
	b := update.ParseSemver("1.4.1")
	c := update.ParseSemver("1.4.0")
	if update.CmpSemver(a, b) >= 0 || update.CmpSemver(b, a) <= 0 {
		t.Fatal("★ 版本大小比较错误")
	}
	if update.CmpSemver(a, c) != 0 {
		t.Fatal("★ 同版本未判等")
	}
	if update.CmpSemver(nil, a) != 0 || update.CmpSemver(a, nil) != 0 {
		t.Fatal("★ nil 未返回 0（不可比）")
	}
}

// TestNewer：只认纯三段式；dev 后缀不误报。
func TestNewer(t *testing.T) {
	if !update.Newer("1.5.0", "1.4.0") {
		t.Fatal("★ 新版本未判新")
	}
	if update.Newer("1.4.0", "1.4.0") {
		t.Fatal("★ 相同版本判新")
	}
	if update.Newer("1.4.0dev", "1.4.0") {
		t.Fatal("★ dev 后缀被误判为新版本")
	}
}

// TestCompareVersions：升级 / 降级 / 完全相同 / 同版本不同 commit。
func TestCompareVersions(t *testing.T) {
	cur := &update.BinVersion{Version: "1.4.0", Commit: "aaaaaaaa"}
	cases := []struct {
		nv       *update.BinVersion
		contains string
	}{
		{&update.BinVersion{Version: "1.5.0", Commit: "bbbbbbbb"}, "可升级"},
		{&update.BinVersion{Version: "1.3.0", Commit: "bbbbbbbb"}, "降级"},
		{&update.BinVersion{Version: "1.4.0", Commit: "aaaaaaaa"}, "完全相同"},
		{&update.BinVersion{Version: "1.4.0", Commit: "bbbbbbbb"}, "commit 不同"},
	}
	for _, c := range cases {
		got := update.CompareVersions(cur, c.nv)
		if !strings.Contains(got, c.contains) {
			t.Errorf("★ CompareVersions = %q，期望含 %q", got, c.contains)
		}
	}
	if got := update.CompareVersions(nil, cur); !strings.Contains(got, "无法读取") {
		t.Fatalf("★ nil 当前版本的输出不可诊断：%q", got)
	}
}

// TestDeltaAssetName：产物名内联版本/平台/来源 commit。
func TestDeltaAssetName(t *testing.T) {
	got := update.DeltaAssetName("1.4.0", "linux", "amd64", "abcdef123456")
	for _, want := range []string{"1.4.0", "linux", "amd64", "abcdef12", ".delta"} {
		if !strings.Contains(got, want) {
			t.Errorf("★ DeltaAssetName = %q，缺 %q", got, want)
		}
	}
}

// TestShortCommit：长 commit 截 8 位；空/unknown 原样。
func TestShortCommit(t *testing.T) {
	if got := update.ShortCommit("abcdef123456"); got != "abcdef12" {
		t.Fatalf("★ ShortCommit = %q", got)
	}
	if got := update.ShortCommit("unknown"); got != "unknown" {
		t.Fatalf("★ unknown 未保留：%q", got)
	}
}

// TestBrokenDirPath：输出落在 dataDir 下且含时间戳。
func TestBrokenDirPath(t *testing.T) {
	got := update.BrokenDirPath("/data/home", "20261109-120000")
	if !strings.HasPrefix(got, "/data/home") || !strings.Contains(got, "20261109-120000") {
		t.Fatalf("★ BrokenDirPath = %q", got)
	}
}
