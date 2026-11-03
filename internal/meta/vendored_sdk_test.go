package meta_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ===== 主仓不再 vendoring SDK 的示例代码 =====
//
// 决策（对应 .gitignore 里的 sdk_repo_only）：外部插件与工具链只在 SDK 仓
// 维护，主仓经 go.mod 的 replace 引用，**只保留编译所需的那几个包**。
//
// 曾经的状况：主仓跟踪了 29 个 SDK 示例文件（example/*/plg.json、plugin.go、
// qq/plugin_test.go，加 remotedevice/ 的 8 个 C 文件）。它们不参与主仓构建
// （唯一 import 的 SDK 包是 sdk/ 与 meta/），却要在两个仓里同步同一份代码。
// 本轮修 example/bili 时的重复劳动就是这么来的。
//
// 本测试钉住这个决策：有人再把示例代码提交进主仓时立刻变红。
// ★ 必须用 git ls-files 判断「**版本控制里有没有**」而不是看目录是否存在 ——
//   工作树里可能因为 vendoring 或本地试验而临时存在，那不算回归。

// vendoredSDKRoot 是 vendored SDK 在主仓里的位置。
const vendoredSDKRoot = "third_party/hologramagentsdk"

// repoRoot 返回主仓根目录（本测试包在 internal/meta/，往上是两层）。
func repoRoot(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("解析仓库根: %v", err)
	}
	return abs
}

// gitTracked 列出 git 版本控制里该路径下的文件（相对仓库根）。
//
// ★ 必须用 `git -C <仓库根>` 而不是就地执行：git ls-files 的路径参数
//
//	是**相对当前目录**解析的，而这个测试运行在 internal/meta/ 下，
//	就地执行会返回空 —— 表现为"必需包全都不在"的假红（我第一版就这样）。
func gitTracked(t *testing.T, pathPrefix string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", repoRoot(t), "ls-files", pathPrefix).Output()
	if err != nil {
		// 不是 git 仓库 / git 不可用：跳过而不是误报。
		if _, statErr := os.Stat(filepath.Join(repoRoot(t), ".git")); statErr != nil {
			t.Skip("非 git 仓库，跳过结构守卫")
		}
		t.Fatalf("git ls-files %s: %v", pathPrefix, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// example/ 与 remotedevice/ 不该再被主仓跟踪。
func TestVendoredSDKHasNoExampleOrRemotedevice(t *testing.T) {
	for _, sub := range []string{"example", "remotedevice"} {
		files := gitTracked(t, vendoredSDKRoot+"/"+sub)
		if len(files) > 0 {
			t.Errorf("主仓仍在跟踪 %s/ 下的 %d 个文件（决策：示例代码只在 SDK 仓维护）:\n  %s\n"+
				"     这些文件不参与主仓构建（唯一 import 的 SDK 包是 sdk/ 与 meta/），\n"+
				"     却要在两个仓同步同一份代码 —— 本轮修 example/bili 时已因此重复劳动。\n"+
				"     修复：git rm -r --cached %s/%s",
				sub, len(files), strings.Join(files, "\n  "), vendoredSDKRoot, sub)
		}
	}
}

// 反向保护：编译真正需要的包必须在，否则删过头会编译失败。
//
// 这条不能省：上一条只防"多了"，若有人为了省事把 sdk/ 或 meta/ 也删了，
// 上一条照样绿，而 go build ./... 会在 CI 里才炸。
func TestVendoredSDKKeepsRequiredPackages(t *testing.T) {
	required := []string{
		"sdk",           // 公开契约：所有插件的入口
		"sdk/plugin.go", // 具体文件：确保不是空目录
		"meta",          // 版本号：主仓与 SDK 版本一致性检查
		"meta/meta.go",
		// ★ 不列 tools/hmapdev/yaegi：.gitignore 第 33 行忽略了
		//   third_party/hologramagentsdk/tools/，它从来没被跟踪过。
		//   而 grep 到的 yaegi 引用是 **SDK 仓自己的文件**在互相 import
		//   （tools/hmapdev/yaegi/interp.go → .../mocksdk），不是主仓依赖。
		//   我第一版把它当主仓依赖写进来，判据直接假红。
	}
	tracked := map[string]bool{}
	for _, f := range gitTracked(t, vendoredSDKRoot) {
		tracked[f] = true
	}
	for _, req := range required {
		// 支持两种形态：目录（下面有文件）或直接是文件
		found := tracked[vendoredSDKRoot+"/"+req]
		if !found {
			// 目录形态：检查它下面是否有被跟踪的文件
			for f := range tracked {
				if strings.HasPrefix(f, vendoredSDKRoot+"/"+req+"/") {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("必需包 %s 不在主仓 vendored 范围内（删过头了）：%s/%s",
				req, vendoredSDKRoot, req)
		}
	}
}

// go.mod 的 replace 必须仍指向 vendored 路径 —— 决策 A 依赖它。
func TestGoModStillReplacesSDKToVendoredPath(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "go.mod"))
	if err != nil {
		t.Fatalf("读 go.mod: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, vendoredSDKRoot) {
		t.Errorf("go.mod 里没有 replace 到 %s：\n%s", vendoredSDKRoot, content)
	}
}
