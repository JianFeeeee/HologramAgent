package update

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// kernelHTTPClient 用于**查询**（JSON 元数据，小、快）。
//
// 20s 总超时在这里是合适的：查不到就该快速失败，让用户看到"查不了"，
// 而不是挂着一个"正在检查更新"的界面。
var kernelHTTPClient = &http.Client{Timeout: 20 * time.Second}

// downloadHTTPClient 用于**下载安装包**。
//
// ★ 为什么不能复用上面那个客户端（实测踩到）：发行包约 880MB，
// 而 Client.Timeout 是**整条请求**的上限（含读完 body）—— 20s 下必然
// `context deadline exceeded (Client.Timeout ... while reading body)`。
// 那会让"从 Release 自动更新"永远失败，而错误看起来像网络问题。
//
// ⇒ 下载客户端**不设总超时**，改用分层超时：
//
//	· 建连/TLS 握手/等响应头 —— 有界，卡住就失败
//	· 读 body —— 不限总时长（大文件），靠连接空闲超时 + 传输层兜底
var downloadHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// ReleaseInfo 是一次「内核检查更新」的结果。
type ReleaseInfo struct {
	Current     string `json:"current"`                // 本机版本
	CurrentC    string `json:"current_commit"`         // 本机 commit
	Latest      string `json:"latest"`                 // 远端最新 release tag
	LatestURL   string `json:"latest_url,omitempty"`   // release 页面
	Asset       string `json:"asset,omitempty"`        // 匹配到的资产名（tar.gz）
	AssetURL    string `json:"asset_url,omitempty"`    // 资产下载地址
	AssetDigest string `json:"asset_digest,omitempty"` // GitHub SHA-256 digest

	// DeltaAsset / DeltaURL 是本版针对**当前本机版本**的差分资产（可选）。
	//
	// ★ 为何单独一字段而不是复用 Asset：
	//   整包与 delta 是两条不同的取包路径，一条失败能退回另一条。
	//   混在一个字段里就无法表达“有整包、但 delta 不适用”。
	//
	// 空串 = 该 release 没有为当前版本提供 delta（首次安装、跳版本、
	// 或发布时判定不适合差分）⇒ 调用方直接走整包。
	DeltaAsset string `json:"delta_asset,omitempty"`
	DeltaURL   string `json:"delta_url,omitempty"`

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
			Digest             string `json:"digest"`
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
			info.AssetDigest = a.Digest
			break
		}
	}

	// 找针对**当前本机 commit** 的 delta（可选）。
	//
	// 命名约定：...-from-<oldCommit8>.delta。带上来源 commit 而不是
	// “随便挑一个 delta”：差分只能对特定旧版本应用，挑错基线会静默产出一份
	// 看起来正常的垃圾（大小结构都对、就是内容错）。
	//
	// 找不到就不填 —— 调用方直接走整包，不是错误。
	if info.CurrentC != "" && info.CurrentC != "unknown" {
		want := "from-" + ShortCommit(info.CurrentC) + ".delta"
		for _, a := range rel.Assets {
			if strings.HasSuffix(a.Name, want) {
				info.DeltaAsset = a.Name
				info.DeltaURL = a.BrowserDownloadURL
				break
			}
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

// DownloadLatestRelease downloads the latest Linux amd64 release asset into dst.
// The package path is supplied by the caller; the update subsystem owns release
// discovery, download, and digest verification. It is safe to use from CLI,
// WebAPI, or an agent tool. No files are replaced by this function.
func DownloadLatestRelease(dst string) (*ReleaseInfo, error) {
	info := CheckRelease(nil)
	if info.AssetURL == "" {
		return info, fmt.Errorf("latest release has no linux/amd64 tar.gz asset: %s", info.Reason)
	}
	if err := downloadFile(info.AssetURL, dst); err != nil {
		return info, err
	}
	if strings.HasPrefix(info.AssetDigest, "sha256:") {
		want := strings.TrimPrefix(info.AssetDigest, "sha256:")
		got, err := sha256File(dst)
		if err != nil {
			return info, err
		}
		if !strings.EqualFold(want, got) {
			return info, fmt.Errorf("release asset sha256 mismatch: want %s got %s", want, got)
		}
	}
	return info, nil
}

func downloadFile(url, dst string) error {
	resp, err := downloadHTTPClient.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dst)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
