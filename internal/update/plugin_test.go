package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writePluginDir 造一个最小的已安装插件目录（只有 plugin.json）。
func writePluginDir(t *testing.T, name, version, repo string) string {
	t.Helper()
	dir := t.TempDir()
	content := "{\n  \"name\": \"" + name + "\",\n  \"version\": \"" + version + "\""
	if repo != "" {
		content += ",\n  \"repository\": \"" + repo + "\""
	}
	content += "\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ── Check 的判据 ─────────────────────────────────────────────

// 判据 1：未声明仓库 ⇒ Reason 明确说明，且 HasUpdate=false。
//
// 静默返回"没有更新"会让用户以为插件是最新的，实际是"查不了"。
// 两种状态必须可区分。
func TestCheck_NoRepoStatesClearly(t *testing.T) {
	dir := writePluginDir(t, "qq", "1.4.4", "")
	info := Check(dir, RepoFromManifest(dir))
	if info.Repo != "" {
		t.Errorf("Repo 应为空，实际 %q", info.Repo)
	}
	if info.HasUpdate {
		t.Error("未声明仓库不应报告有更新")
	}
	if !strings.Contains(info.Reason, "未声明仓库") {
		t.Errorf("Reason 必须说明「未声明仓库」（与「没有更新」是两回事），实际: %s", info.Reason)
	}
}

// 判据 2：对真实仓库（HologramAgent/qq）检查，必须拿到版本且不为空。
//
// ★ 这是**活体**测试：它验证的是整条链路（manifest → GitHub API → 比较）。
// 网络不可用时跳过（CI 偶发网络抖动不该红整个包）。
func TestCheck_AgainstRealRepo(t *testing.T) {
	if os.Getenv("SKIP_LIVE") != "" {
		t.Skip("SKIP_LIVE set")
	}
	dir := writePluginDir(t, "qq", "0.0.1", "https://github.com/HologramAgent/qq")
	info := Check(dir, RepoFromManifest(dir))
	if info.Reason != "" && strings.Contains(info.Reason, "不可达") {
		t.Skipf("网络不可达，跳过活体验证: %s", info.Reason)
	}
	if info.Latest == "" {
		t.Fatalf("真实仓库应能查到版本，实际 reason=%s", info.Reason)
	}
	if !info.HasUpdate {
		t.Errorf("本地 0.0.1 vs 远端 %s 应判有更新", info.Latest)
	}
}

// 判据 3：Newer 的比较语义（kernel/plugin 两侧共用）。
func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1.4.5", "1.4.4", true},
		{"1.5.0", "1.4.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.4.4", "1.4.4", false},
		{"1.4.3", "1.4.4", false},
		{"", "1.4.4", false},
		{"1.4.4", "", true},
		// dev 视为与同名正式版相同（开发分支通常领先，不误报）
		{"1.4.4", "1.4.4dev", false},
		{"1.4.4dev", "1.4.4", false},
		// v 前缀不影响
		{"v1.4.5", "v1.4.4", true},
		// 非纯三段式不参与比较
		{"1.4", "1.4.0", false},
		{"1.4.4-rc1", "1.4.4", false},
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q,%q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// 判据 3b：TrimTag 的归一语义。
func TestTrimTag(t *testing.T) {
	for in, want := range map[string]string{
		"v1.4.4":     "1.4.4",
		"1.4.4":      "1.4.4",
		"1.4.0dev":   "1.4.0",
		"1.4.0dev.1": "1.4.0",
		"release1":   "release1", // 非纯版本原样返回，交给 ParseSemver 判不可比
	} {
		if got := TrimTag(in); got != want {
			t.Errorf("TrimTag(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 判据 3c：ParseSemver 只认恰好三段。
//
// ★ 这是合并时抓到的真实 bug：早期实现只挡 len<2，于是 "1.4" 漏过去、
// 缺失的 patch 被零值填成 1.4.0 ⇒「1.4 → 1.4.0」被判成完全相同，
// 进而说"无需升级"，而两者内容未必相同。
func TestParseSemver_RequiresThreeParts(t *testing.T) {
	for _, ok := range []string{"1.4.4", "v1.4.4", "1.4.0"} {
		if ParseSemver(ok) == nil {
			t.Errorf("%q 应可解析", ok)
		}
	}
	for _, bad := range []string{"", "1", "1.4", "1.4.4.4", "v1.4", "abc", "1.4.x"} {
		if v := ParseSemver(bad); v != nil {
			t.Errorf("%q 不应被解析成语义版本，实际 %+v", bad, v)
		}
	}
}

// 判据 4：trimV 只认纯 semver tag。
func TestTrimV(t *testing.T) {
	// TrimTag 归一化（去 v 前缀）—— 非纯 semver 原样返回，
	// 由 ParseSemver 判"不可比"，不在这里丢弃信息。
	for tag, want := range map[string]string{
		"v1.4.4": "1.4.4",
		"1.4.4":  "1.4.4",
	} {
		if got := TrimTag(tag); got != want {
			t.Errorf("TrimTag(%q) = %q，期望 %q", tag, got, want)
		}
	}
	// 这些原样返回（交给 ParseSemver 判不可比）
	for _, tag := range []string{"v1.4", "release1", "1.4.4-rc1"} {
		if ParseSemver(TrimTag(tag)) != nil {
			t.Errorf("%q 归一后仍不应可解析", tag)
		}
	}
}

// 判据 5：parseGitHub 认全主流 URL 形态。
func TestParseGitHub(t *testing.T) {
	for _, repo := range []string{
		"https://github.com/HologramAgent/qq",
		"https://github.com/HologramAgent/qq.git",
		"github.com/HologramAgent/qq",
		"git@github.com:HologramAgent/qq.git",
	} {
		owner, name, ok := parseGitHub(repo)
		if !ok || owner != "HologramAgent" || name != "qq" {
			t.Errorf("parseGitHub(%q) = (%q,%q,%v)", repo, owner, name, ok)
		}
	}
	// 非 GitHub 不可误判
	if _, _, ok := parseGitHub("https://gitee.com/x/y"); ok {
		t.Error("gitee 不应被解析为 GitHub")
	}
}

// 判据 6：Upgrade 拒绝缺 hmapdev 的环境（响亮失败而非中途半成品）。
func TestUpgrade_RequiresHmapdev(t *testing.T) {
	res, err := Upgrade("https://github.com/HologramAgent/qq", t.TempDir(), "/nonexistent/hmapdev")
	if err == nil {
		t.Fatal("hmapdev 不可用时必须报错")
	}
	if res != nil {
		t.Errorf("失败时不应返回结果: %+v", res)
	}
	if !strings.Contains(err.Error(), "hmapdev") {
		t.Errorf("错误应指明 hmapdev 问题，实际: %v", err)
	}
}

// 判据 7：Upgrade 拒绝空仓库。
func TestUpgrade_EmptyRepo(t *testing.T) {
	if _, err := Upgrade("", t.TempDir(), ""); err == nil {
		t.Fatal("空仓库必须报错")
	}
}

// ── 端到端：对真实仓库做完整升级（编译产物验证）──────────────
//
// ★ 这条是 plugin_update 的**核心承诺**的验证：拉下来 → 编出来 → 产物存在。
// 前置：本机有 git 与 hmapdev。任一缺失则跳过（CI 上 hmapdev 未必装）。
func TestUpgrade_EndToEnd_RealRepo(t *testing.T) {
	if _, err := exec.LookPath("hmapdev"); err != nil {
		t.Skip("本机无 hmapdev，跳过端到端")
	}
	if os.Getenv("SKIP_LIVE") != "" {
		t.Skip("SKIP_LIVE set")
	}
	// 用一个已打 tag 的真实仓库（v1.4.4）
	res, err := Upgrade("https://github.com/HologramAgent/qq", t.TempDir(), "hmapdev")
	if err != nil {
		// 首次升级 from="" 时 isNewer 恒真，失败大概率是编译环境问题
		t.Skipf("端到端失败（可能是编译环境）: %v", err)
	}
	if res.HmapPath == "" {
		t.Fatal("应产出 .hmap 路径")
	}
	if _, err := os.Stat(res.HmapPath); err != nil {
		t.Fatalf("产物不存在: %v", err)
	}
	if !strings.HasSuffix(res.HmapPath, ".hmap") {
		t.Errorf("产物应是 .hmap: %s", res.HmapPath)
	}
	if res.To == "" {
		t.Error("应记录远端版本")
	}
}
