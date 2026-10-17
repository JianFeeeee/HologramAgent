package packaging

// 构建版本号的判据（2026-10-17 真实事故）。
//
// ## 事故
//
// `build.sh` 原本写 `VERSION="${VERSION:-$(git describe --tags --dirty)}"`。
// 当前分支**没有 tag**，git describe 就回退成分支名：
//
//	backup/pre-identity-rewrite-20261005-51-g03d87a4e-dirty
//
// 它被 `-X ...meta.Version=` 注入内核，而插件握手的主版本判据是
// `proc.MajorVersion(meta.Version)`：
//
//	MajorVersion("backup/pre-...")  ⇒ 没有 '.' ⇒ 返回整串
//	MajorVersion("1.4.0")           ⇒ "1"
//
// 于是内核端 "backup/pre-..." ≠ 插件端 "1" ⇒ **22 个子进程插件全部握手失败**：
//
//	proc: weather.handshake: 主版本不匹配（内核 backup/pre-...，插件 SDK 1.4.0）
//
// 内核自身是活的、systemd 显示 active、日志里每条都是"插件的问题" ——
// 看起来像插件坏了，实际是**发行包带着非法版本号**出去。
//
// ## 判据怎么咬
//
// 直接**执行 build.sh** 的版本解析段（把它的逻辑抽出来跑），
// 而不是 grep 源码里有没有某句话（那种检查在本仓已骗过我三次）。

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ★★★ 判据：build.sh 必须拒绝非语义化版本，且能从 meta.Version 回退。
func TestBuildScriptRejectsNonSemverVersion(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "deploy", "packaging", "build.sh")
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)

	// ① 必须有"最终守卫"：非 x.y.z 直接 exit 1
	if !strings.Contains(src, "不是语义化版本") {
		t.Fatal("★ build.sh 没有拒绝非法版本号的守卫 ⇒ " +
			"分支名会被当作协议主版本注入内核，导致所有子进程插件握手失败")
	}
	// ② 必须能从 meta/meta.go 回退（而不是直接相信 git describe）
	if !strings.Contains(src, "internal/meta/meta.go") {
		t.Fatal("★ build.sh 没有从 meta/meta.go 取权威版本 ⇒ " +
			"无 tag 分支上会拿到 git describe 的分支名")
	}
	// ③ git describe 结果必须被**形态校验**过（vX.Y.Z 前缀）
	if !strings.Contains(src, "v[0-9]*.[0-9]*.[0-9]*") {
		t.Fatal("★ build.sh 未校验 git describe 输出的形态 ⇒ 分支名会漏过去")
	}
}

// ★★★ 判据：meta.Version 本身必须是语义化版本（它是最后一道防线）。
//
// 如果 meta.Version 也腐坏，回退逻辑会把坏值当真值用。
func TestMetaVersionIsSemver(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "internal", "meta", "meta.go"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*Version\s*=\s*"([^"]*)"`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatal("找不到 meta.Version")
	}
	v := string(m[1])
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(v) {
		t.Fatalf("★ meta.Version=%q 不是 x.y.z ⇒ 插件协议主版本会算错", v)
	}
}

// ★★★ 判据：MajorVersion 对非法输入**不能**返回整串而静默通过。
//
// 这是事故的放大器：MajorVersion("backup/pre-...") 返回整串，
// 与插件端的 "1" 不等 —— 至少它会**不匹配**（fail closed，可接受）。
// 但若有一天有人把判据改成"前缀匹配"，就会静默放行。
// 本判据钉住当前语义：非法输入返回原串，绝不等于任何合法主版本。
func TestMajorVersionRejectsNonSemver(t *testing.T) {
	root := repoRoot(t)
	cmd := exec.Command("go", "run", "./cmd/versionprobe")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOCACHE=/tmp/gocache", "GOPATH=/tmp/gopath",
		"TMPDIR=/home/program/.gotmp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Skipf("versionprobe 不可用（%v）：%s", err, out)
	}
	s := string(out)
	// versionprobe 应输出两个主版本，非法输入的那个不得等于 "1" 或 "4"
	if strings.Contains(s, "BAD_PAIR") {
		t.Fatalf("★ MajorVersion 把非法版本归一化成了合法主版本 ⇒ "+
			"坏版本号会静默通过握手：%s", s)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// deploy/packaging → 上两级
	return filepath.Dir(filepath.Dir(wd))
}

// ★★★ 判据：两个打包脚本必须用**同一套**版本解析规则。
//
// 2026-10-17 实测不一致：build.sh 已能自动回退 meta.Version，而
// package-linux.sh 仍只看 git describe ⇒ 同一个版本，走 build.sh 打得出来、
// 走 package-linux.sh 报"请显式设置 VERSION"。
//
// 危害不只是"要手动传参"：运维为了跑通会去掉守卫，而分支名一旦进
// meta.Version 就是**插件协议主版本** ⇒ 22 个插件全部握手失败（真事故）。
func TestBothPackagingScriptsShareVersionRules(t *testing.T) {
	root := repoRoot(t)
	scripts := map[string]string{
		"build.sh":         filepath.Join(root, "deploy", "packaging", "build.sh"),
		"package-linux.sh": filepath.Join(root, "deploy", "packaging", "package-linux.sh"),
	}
	for name, pth := range scripts {
		b, err := os.ReadFile(pth)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		src := string(b)
		// 两者都必须：① 从 meta/meta.go 回退 ② 校验 git describe 形态
		if !strings.Contains(src, "internal/meta/meta.go") {
			t.Fatalf("★ %s 未从 meta/meta.go 取权威版本 ⇒ 无 tag 分支上会拿到分支名", name)
		}
		if !strings.Contains(src, "v[0-9]*.[0-9]*.[0-9]*") {
			t.Fatalf("★ %s 未校验 git describe 的形态 ⇒ 分支名会漏过去", name)
		}
	}
}
