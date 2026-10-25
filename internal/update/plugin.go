// Package pluginupdate 实现**插件**的更新闭环：查仓库最新版 → 拉源码 → 现场编译 → 交给调用方安装。
//
// ## 三个入口共享这一个实现（架构约束）
//
//   - agent 工具 plugin_update（pluginmgr 注册）
//   - WebAPI（/api/v1/plugins/update）
//   - CLI 无（插件更新由 agent/WebUI 驱动，没有独立 CLI 命令）
//
// 三处若各写一份「拉取/比较/编译」逻辑，必然漂移 —— 出现"agent 说没有更新、
// WebUI 却升了级"这类互相矛盾的状态。所以这里只提供**唯一**的两个动作，
// 入口层只做参数解析与结果呈现。
//
// ## 数据流
//
//	plugin.json 的 repository 字段（hmapdev 从 plg.json 的 repo 透传）
//	  → Check：git ls-remote / GitHub API 比较版本
//	  → Upgrade：git clone → hmapdev build → 产物 .hmap
//	  → 调用方用 plugin_install(path, overwrite=true) 安装
//
// ## 为什么编译产物不直接装
//
// 安装路径（配置保留、运行态停止、版本比对）是 pluginmgr 已有且经过
// 生产验证的逻辑。这里**只负责到 .hmap 产出为止**，安装由调用方走
// installFromPath —— 否则要在这里重新实现一遍升级语义，两份必然漂移。
package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// UpdateInfo 是一次「检查更新」的结果。
type UpdateInfo struct {
	// Name 是插件名（来自本地 plugin.json）。
	Name string `json:"name"`
	// Current 是本地版本（来自本地 plugin.json）。
	Current string `json:"current"`
	// Latest 是远端最新版本（来自远端 plg.json）。
	Latest string `json:"latest"`
	// Repo 是插件声明的源码仓库（本地 plugin.json 的 repository）。
	Repo string `json:"repo"`
	// HasUpdate 表示远端版本是否比本地新。
	HasUpdate bool `json:"has_update"`
	// Reason 是"为什么没有更新可用"的说明（未声明仓库/仓库不可达等）。
	// 有更新时为空。
	Reason string `json:"reason,omitempty"`
}

// RepoFromManifest 读本地 plugin.json 的 repository 字段。
//
// 返回空串表示该插件未声明仓库 —— 调用方应把它如实呈现
// （"该插件未声明仓库"），而不是当作"没有更新"混淆。
func RepoFromManifest(pluginDir string) string {
	data, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Repository string `json:"repository"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Repository)
}

// VersionFromManifest 读本地 plugin.json 的 version 字段。
func VersionFromManifest(pluginDir string) string {
	data, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Version)
}

// NameFromManifest 读本地 plugin.json 的 name 字段。
func NameFromManifest(pluginDir string) string {
	data, err := os.ReadFile(filepath.Join(pluginDir, "plugin.json"))
	if err != nil {
		return ""
	}
	var m struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	return strings.TrimSpace(m.Name)
}

var tagSemverRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+)$`)

// Check 从仓库查询最新版本并与本地比较。
//
// 版本来源按可靠性排序：
//  1. GitHub API：仓库最新 release 的 tag（最准确，发版即有）
//  2. git ls-remote 的 tag 列表（任何 git 托管都可用；无 release 时靠它）
//  3. 远端 plg.json 的 version 字段（兜底，覆盖没有打 tag 的仓库）
//
// 三种都失败时返回 Reason 说明（不返回 error —— "查不到"本身是可呈现的结果）。
func Check(pluginDir, repo string) *UpdateInfo {
	info := &UpdateInfo{
		Name:    NameFromManifest(pluginDir),
		Current: VersionFromManifest(pluginDir),
		Repo:    repo,
	}
	if info.Current == "" {
		info.Current = "unknown"
	}
	if repo == "" {
		info.Reason = "未声明仓库（plugin.json 无 repository 字段）：无法自动更新"
		return info
	}

	latest, how := fetchLatestVersion(repo)
	if latest == "" {
		info.Reason = "仓库不可达或没有版本信息：" + how
		return info
	}
	info.Latest = latest
	info.HasUpdate = Newer(latest, info.Current)
	return info
}

// fetchLatestVersion 返回（最新版本, 来源说明）。
func fetchLatestVersion(repo string) (string, string) {
	if owner, name, ok := parseGitHub(repo); ok {
		if v := ghLatestRelease(owner, name); v != "" {
			return v, "GitHub release"
		}
		if v := ghLatestTag(owner, name); v != "" {
			return v, "GitHub tag"
		}
	}
	// 非 GitHub 或 GitHub 都空：git ls-remote 拿 tag 列表取最大
	if v := gitRemoteMaxTag(repo); v != "" {
		return v, "git tag"
	}
	return "", "无可用的版本来源"
}

// parseGitHub 判断是否 GitHub 仓库，返回 (owner, repo)。
func parseGitHub(repo string) (string, string, bool) {
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/", "git@github.com:", "github.com/",
	} {
		if strings.HasPrefix(repo, prefix) {
			rest := strings.TrimPrefix(repo, prefix)
			rest = strings.TrimSuffix(rest, ".git")
			rest = strings.TrimSuffix(rest, "/")
			parts := strings.Split(rest, "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return parts[0], parts[1], true
			}
		}
	}
	return "", "", false
}

var ghHTTPClient = &http.Client{Timeout: 15 * time.Second}

// ghLatestRelease 查 GitHub 最新 release 的 tag。
func ghLatestRelease(owner, name string) string {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, name)
	var body struct {
		TagName string `json:"tag_name"`
	}
	if getJSON(url, &body) != nil {
		return ""
	}
	return TrimTag(body.TagName)
}

// ghLatestTag 查 GitHub 全部 tag，取最大的 semver。
func ghLatestTag(owner, name string) string {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/tags?per_page=100", owner, name)
	var body []struct {
		Name string `json:"name"`
	}
	if getJSON(url, &body) != nil {
		return ""
	}
	max := ""
	for _, t := range body {
		if v := TrimTag(t.Name); v != "" {
			if Newer(v, max) {
				max = v
			}
		}
	}
	return max
}

// gitRemoteMaxTag 用 git ls-remote 拿 tag 列表（非 GitHub 也可用）。
func gitRemoteMaxTag(repo string) string {
	cmd := exec.Command("git", "ls-remote", "--tags", repo)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	max := ""
	for _, line := range strings.Split(string(out), "\n") {
		// 形如 "refs/tags/v1.2.3"（跳过 ^{} 解引用行）
		i := strings.LastIndex(line, "refs/tags/")
		if i < 0 {
			continue
		}
		tag := strings.TrimSuffix(line[i+len("refs/tags/"):], "^{}")
		if v := TrimTag(tag); v != "" && Newer(v, max) {
			max = v
		}
	}
	return max
}

func getJSON(url string, v interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := ghHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// 版本比较统一走 semver.go 的 Newer / TrimTag。
//
// 合并前这里是另一套私有实现（trimV / isNewer / semverNums + 自己的正则），
// 与 kernel 侧的行为不一致 —— 例如 kernel 侧会把 "1.4" 判不可比，
// 这套却因正则只要求两段而漏过去。三份实现的差异就是 bug 温床。

// UpgradeResult 是一次「升级」的结果。
type UpgradeResult struct {
	// HmapPath 是编出来的 .hmap 产物路径（调用方交给 plugin_install）。
	HmapPath string `json:"hmap_path"`
	// Repo / From / To 记录升级轨迹。
	Repo string `json:"repo"`
	From string `json:"from"`
	To   string `json:"to"`
	// BuildLog 是 hmapdev build 的输出尾部（失败时诊断用）。
	BuildLog string `json:"build_log,omitempty"`
}

// Upgrade 拉最新源码并现场编译，返回 .hmap 产物路径。
//
// 流程：clone（浅）→ 校验远端 plg.json 的版本确实更新 → hmapdev build。
//
// workDir 是工作目录（由调用方给定，插件源码副本落在其下，调试友好且可复用）；
// hmapdevBin 是 hmapdev 可执行文件路径。
func Upgrade(repo, workDir, hmapdevBin string) (*UpgradeResult, error) {
	if repo == "" {
		return nil, fmt.Errorf("未声明仓库，无法升级")
	}
	if hmapdevBin == "" {
		hmapdevBin = "hmapdev"
	}
	if _, err := exec.LookPath(hmapdevBin); err != nil {
		return nil, fmt.Errorf("hmapdev 不可用（%s）：现场编译插件需要它", hmapdevBin)
	}

	name := repoName(repo)
	dst := filepath.Join(workDir, name)
	from := VersionFromManifest(dst) // 已有副本时读它的版本

	// ① 拉取/更新源码（浅克隆足够 —— 我们不要历史）
	if err := fetchSource(repo, dst); err != nil {
		return nil, fmt.Errorf("拉取源码: %w", err)
	}

	// ② 远端版本校验：明确告知"已经是最新"而不是白编一场
	to := VersionFromManifest(dst)
	if to == "" {
		return nil, fmt.Errorf("远端仓库的 plg.json 缺 version 字段，无法判定版本")
	}
	if !Newer(to, from) && from != "" {
		return nil, fmt.Errorf("已是最新（本地 %s = 远端 %s）", from, to)
	}

	// ③ 现场编译
	buildOut, err := hmapdevBuild(dst, hmapdevBin)
	if err != nil {
		return nil, fmt.Errorf("编译失败: %w\n%s", err, tailStr(buildOut, 800))
	}

	// ④ 找产物（hmapdev 输出到 dist/）
	hmap := findHmap(filepath.Join(dst, "dist"))
	if hmap == "" {
		return nil, fmt.Errorf("编译完成但 dist/ 下没有 .hmap 产物\n%s", tailStr(buildOut, 400))
	}
	return &UpgradeResult{HmapPath: hmap, Repo: repo, From: from, To: to, BuildLog: tailStr(buildOut, 400)}, nil
}

// fetchSource clone（或更新已存在的副本）到 dst。
func fetchSource(repo, dst string) error {
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		// 已有副本：fetch + reset，比删了重 clone 快且省流量
		if out, err := exec.Command("git", "-C", dst, "fetch", "origin").CombinedOutput(); err != nil {
			return fmt.Errorf("git fetch: %w\n%s", err, tailStr(string(out), 300))
		}
		if out, err := exec.Command("git", "-C", dst, "reset", "--hard", "origin/HEAD").CombinedOutput(); err != nil {
			return fmt.Errorf("git reset: %w\n%s", err, tailStr(string(out), 300))
		}
		return nil
	}
	// 新副本：浅 clone。--depth 1 对几十 MB 的插件仓省 90% 时间。
	cmd := exec.Command("git", "clone", "--depth", "1", repo, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		// 清掉半成品，避免下次把半成品误当有效副本
		os.RemoveAll(dst)
		return fmt.Errorf("git clone: %w\n%s", err, tailStr(string(out), 300))
	}
	return nil
}

// hmapdevBuild 在 srcDir 里跑 hmapdev build，返回完整输出。
func hmapdevBuild(srcDir, hmapdevBin string) (string, error) {
	cmd := exec.Command(hmapdevBin, "build")
	cmd.Dir = srcDir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// findHmap 在 dist 目录里找 .hmap 产物（优先 bundle）。
func findHmap(distDir string) string {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return ""
	}
	// bundle 优先：它是全平台合集，plugin_install 直接吃
	bundle := ""
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), "_bundle.hmap") {
			bundle = filepath.Join(distDir, e.Name())
			break
		}
	}
	if bundle != "" {
		return bundle
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".hmap") {
			return filepath.Join(distDir, e.Name())
		}
	}
	return ""
}

// repoName 从仓库 URL 提取名字（用于本地目录名）。
func repoName(repo string) string {
	s := strings.TrimSuffix(repo, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		return s[i+1:]
	}
	return "plugin"
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…（前略）\n" + s[len(s)-n:]
}
