package update

// OTA 的**来源**解析：从 Release 自动更新，或指定本地安装包。
//
// ## 为什么把"来源"做成一个显式类型
//
// 两种来源的后半段完全一致（解包 → 预检 → 备份 → 替换 → pending），
// 只有"包从哪来"不同。若在各入口各写一遍 if release { 下载 } else { 用路径 }，
// 就会重演"三端三份实现慢慢漂移"的老问题。
//
// 所以：**来源解析一次，升级流程只有一条**。
//
//	Source{Kind: FromRelease}        → 查 GitHub Release → 下载 → 解包
//	Source{Kind: FromPackage, Path}  → 直接用本地包 → 解包
//	                        ↓
//	                  Apply() 同一条路

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SourceKind 是 OTA 包的来源类别。
type SourceKind string

const (
	// FromPackage 表示"用户直接给了安装包路径"。
	//
	// 为什么保留它：离线机器、内网部署、灰度验证都需要先自己拿到包再升级；
	// 而且它是**可复现**的（同一个文件升同一台机器）。
	FromPackage SourceKind = "package"

	// FromRelease 表示"从仓库最新 GitHub Release 自动取包"。
	//
	// 适用场景：常规运维升级，不想手工下载。
	// ★ 拿到的是**最新 Release**，不一定是用户以为的那个版本 ——
	//   所以 Resolve 会把 release tag 一并回报，让调用方打印出来。
	FromRelease SourceKind = "release"
)

// Source 是一次 OTA 的包来源。
type Source struct {
	Kind SourceKind
	// Path 是本地安装包路径（仅 Kind=FromPackage 时有意义）。
	Path string
}

// ParseSource 从参数解析来源（CLI/WebAPI 共用，避免各写一套判空逻辑）。
//
// 规则：
//
//	pkg 非空          → 本地包（**优先**：显式给的路径比"自动取最新"更明确）
//	pkg 为空 + release → 从 Release 取
//	两者都空           → 报错（不允许"猜一个默认动作"）
func ParseSource(pkg string, release bool) (Source, error) {
	pkg = strings.TrimSpace(pkg)
	switch {
	case pkg != "":
		return Source{Kind: FromPackage, Path: pkg}, nil
	case release:
		return Source{Kind: FromRelease}, nil
	default:
		return Source{}, fmt.Errorf("必须指定安装包路径（pkg），或选择从 Release 自动更新（release）")
	}
}

// Resolve 把来源解析成"解包后的根目录"，供 Apply 使用。
//
// workDir 是调用方提供的临时目录（由调用方负责清理）。
// 返回的 rel 仅在 Kind=FromRelease 时非 nil（携带用的是哪个 tag/资产）。
func (s Source) Resolve(workDir string) (root string, rel *ReleaseInfo, err error) {
	switch s.Kind {
	case FromPackage:
		if s.Path == "" {
			return "", nil, fmt.Errorf("来源是本地包，但路径为空")
		}
		st, serr := os.Stat(s.Path)
		if serr != nil {
			return "", nil, fmt.Errorf("安装包不存在: %s（%v）", s.Path, serr)
		}
		if st.IsDir() {
			return "", nil, fmt.Errorf("安装包是目录，不是文件: %s", s.Path)
		}
		root, err = ExtractPkg(s.Path, workDir)
		return root, nil, err

	case FromRelease:
		pkgPath := filepath.Join(workDir, "release.tar.gz")
		info, derr := DownloadLatestRelease(pkgPath)
		if derr != nil {
			return "", info, derr
		}
		root, err = ExtractPkg(pkgPath, workDir)
		return root, info, err

	default:
		return "", nil, fmt.Errorf("未知的 OTA 来源: %q", s.Kind)
	}
}
