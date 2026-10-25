package update

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ── kernel_update 的「检查」半边 ──────────────────────────────
//
// 升级半边（DoUpgrade）CLI 已在用；这里补「从 GitHub Release 发现新版」，
// 供 agent 工具与 WebAPI 调用。与 CLI 共享 BinVersion / CompareVersions。

// ghRepo 是内核发布仓库（GitHub Release 的来源）。
//
// 为什么不在调用点写死：插件迁移到 HologramAgent 组织后，内核仓也可能
// 随之迁移；届时只改这一处。环境变量可覆盖（测试/私有部署用）。
func ghRepo() string {
	if v := strings.TrimSpace(osGetenv("HOLOGRAM_RELEASE_REPO")); v != "" {
		return v
	}
	return "JianFeeeee/HologramAgent"
}

var kernelHTTPClient = &http.Client{Timeout: 20 * time.Second}

// ReleaseInfo 是一次「内核检查更新」的结果。
type ReleaseInfo struct {
	Current   string `json:"current"`              // 本机版本
	CurrentC  string `json:"current_commit"`       // 本机 commit
	Latest    string `json:"latest"`               // 远端最新 release tag
	LatestURL string `json:"latest_url,omitempty"` // release 页面
	Asset     string `json:"asset,omitempty"`      // 匹配到的资产名（tar.gz）
	AssetURL  string `json:"asset_url,omitempty"`  // 资产下载地址
	HasUpdate bool   `json:"has_update"`
	Reason    string `json:"reason,omitempty"` // 查不到时的说明
}

// CheckRelease 查 GitHub Release 并与本机版本比较。
//
// current 为 nil 时（读不到本机版本，如老内核没有 --version），
// 比较会降级为"未知"，但 release 信息照常返回 —— 与 CLI 探测同语义。
func CheckRelease(current *BinVersion) *ReleaseInfo {
	info := &ReleaseInfo{}
	if current != nil {
		info.Current = current.Version
		info.CurrentC = current.Commit
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", ghRepo())
	if err := httpGetJSON(url, &rel); err != nil {
		info.Reason = "无法查询 GitHub Release：" + err.Error()
		return info
	}
	if rel.TagName == "" {
		info.Reason = "仓库还没有任何 release"
		return info
	}
	info.Latest = TrimTag(rel.TagName)
	info.LatestURL = rel.HTMLURL

	// 找本平台资产（linux/amd64）
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, "linux") && strings.Contains(a.Name, "amd64") && strings.HasSuffix(a.Name, ".tar.gz") {
			info.Asset = a.Name
			info.AssetURL = a.BrowserDownloadURL
			break
		}
	}

	if current == nil || current.Version == "" {
		info.Reason = "本机版本未知（内核过老，无 --version）：无法比较，请人工确认"
		return info
	}
	info.HasUpdate = Newer(info.Latest, current.Version)
	return info
}

// 版本比较走 semver.go 的 Newer / TrimTag（子系统内唯一一份）。
//
// 合并前这里有 versionNewer + trimTagV 两个私有函数，与 kernel 侧、
// plugin 侧各写一份 —— 三份行为不一致是真实的 bug 温床，见 semver.go 注释。

func httpGetJSON(url string, v interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := kernelHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func osGetenv(k string) string { return os.Getenv(k) }
