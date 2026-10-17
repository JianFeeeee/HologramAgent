package system

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ProtectedPaths L0 写前留档保护的系统路径前缀。主 agent 通过 files 插件写
// 这些路径前，先自动把原文存档到 <dataDir>/file_baseline/，供 guard 还原。
//
// 该集合强依赖当前 Linux 发行版布局（/etc、/home）。默认值仅作兜底；
// 生产环境应由 homed/main 或 guard.yaml 显式注入（SetProtectedPaths），
// 以适配具体发行版/部署路径，避免硬编码失效。
var protectedPrefixes = []string{
	"/etc/",
}

// SetProtectedPaths 覆盖受保护路径前缀集合（发行版/部署路径适配入口）。
// 幂等地保留 / 前缀兜底；传入 nil/空则恢复默认。
func SetProtectedPaths(prefixes []string) {
	if len(prefixes) == 0 {
		protectedPrefixes = []string{"/etc/"}
		return
	}
	ns := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if p = strings.TrimSpace(p); p != "" {
			ns = append(ns, p)
		}
	}
	protectedPrefixes = ns
}

// ProtectedPaths 返回当前受保护路径前缀（副本）。
func ProtectedPaths() []string {
	out := make([]string, len(protectedPrefixes))
	copy(out, protectedPrefixes)
	return out
}

// IsProtectedPath 判断路径是否属于受保护的写前留档范围。
func IsProtectedPath(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, p := range protectedPrefixes {
		if strings.HasPrefix(abs, p) {
			return true
		}
	}
	return false
}

// IsProtectedPathExplicit 同 IsProtectedPath，但仅匹配传入的显式前缀集
// （不改全局状态时的判断入口，供测试/工具使用）。
func IsProtectedPathExplicit(prefixes []string, path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, p := range prefixes {
		if strings.HasPrefix(abs, p) {
			return true
		}
	}
	return false
}

// ArchiveBeforeWrite 在覆盖某个受保护路径前调用：若目标为已存在的普通文件，
// 则把原文复制到 <dataDir>/file_baseline/<相对路径>（幂等：已存在同 hash 则跳过），
// 返回是否发生了留档。用于 L0 写 /etc 前的自动留档。
//
// archive base 为 <dataDir>/file_baseline。path 为绝对路径。
func ArchiveBeforeWrite(baseDir, path string) (archived bool, err error) {
	if !IsProtectedPath(path) {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // 新建文件无需留档
		}
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}

	rel := strings.TrimPrefix(path, "/")
	dst := filepath.Join(filepath.Join(baseDir, "file_baseline"), rel)
	if _, err := os.Stat(dst); err == nil {
		// 已留档（内容未再变化时避免重复）
		if same, _ := sameContent(path, dst); same {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(dst, data, 0600); err != nil {
		return false, err
	}
	return true, nil
}

func sameContent(a, b string) (bool, error) {
	da, ea := os.ReadFile(a)
	if ea != nil {
		return false, ea
	}
	db, eb := os.ReadFile(b)
	if eb != nil {
		return false, eb
	}
	return string(da) == string(db), nil
}

// FileBaselineDir 返回写前留档根目录。
func FileBaselineDir(dataDir string) string {
	return filepath.Join(dataDir, "file_baseline")
}

// RestoreFileFromBaseline 从写前留档恢复一个受保护文件（guard 使用）。
// 若留档存在则写回并返回 true 与恢复路径；否则 false。
func RestoreFileFromBaseline(dataDir, path string) (bool, error) {
	if !IsProtectedPath(path) {
		return false, nil
	}
	rel := strings.TrimPrefix(path, "/")
	dst := filepath.Join(FileBaselineDir(dataDir), rel)
	data, err := os.ReadFile(dst)
	if err != nil {
		return false, nil
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return false, fmt.Errorf("restore from baseline %s: %w", dst, err)
	}
	return true, nil
}
