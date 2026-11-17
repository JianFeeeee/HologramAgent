package security

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
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
	// ★ 记下原文件的**权限位**（写成 <file>.mode 侧车）。
	//
	// 为什么不能省：还原时用固定 0644 会把原本 0600 的凭据文件**放宽** ——
	// 那不是"恢复原状"，是"恢复内容但顺手改了权限"。实测生产留档里就有一条
	// `gzu-rss.service`（systemd 单元，原本 0644）与两条 llmsproxy lua。
	// 权限漂移在 /etc 下后果不对称：放宽了没人发现，收紧了服务起不来。
	mode := uint32(info.Mode().Perm())
	if err := os.WriteFile(dst+".mode", []byte(strconv.FormatUint(uint64(mode), 8)), 0600); err != nil {
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
		return false, nil // 没有留档 ≠ 错误：该文件从未被写过
	}
	// 权限位：有侧车就按它还原；没有（老留档）退回 0644 并留痕。
	perm := os.FileMode(0o644)
	if raw, err := os.ReadFile(dst + ".mode"); err == nil {
		if v, perr := strconv.ParseUint(strings.TrimSpace(string(raw)), 8, 32); perr == nil {
			perm = os.FileMode(v)
		}
	} else {
		log.Printf("[guard] %s: no recorded mode, restoring with 0644", path)
	}
	// 先建父目录：目标可能已经被删（那是更彻底的损坏形态）。
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("recreate parent for %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		return false, fmt.Errorf("restore from baseline %s: %w", dst, err)
	}
	// ★ WriteFile 的 perm **只在新建时生效** —— 目标已存在（正是被改坏那种）
	// 时它沿用旧 mode，所以必须显式 chmod。实测：不加这一行，
	// 原本 0600 的文件还原后仍是 644（TestRestorePreservesFileMode 抓到）。
	if err := os.Chmod(path, perm); err != nil {
		return false, fmt.Errorf("restore mode for %s: %w", path, err)
	}
	return true, nil
}

// ArchivedFiles 返回留档目录里**全部**已存档文件的原路径（绝对路径）。
//
// 用途：L1 还原时不必事先知道"哪些文件被改过" —— 留档目录本身就是那份清单
// （只有真的被覆盖过的受保护文件才会出现在里面）。按路径排序，便于日志比对。
//
// 跳过 `<file>.mode` 侧车：它是权限元数据，不是被存档的内容。
func ArchivedFiles(dataDir string) ([]string, error) {
	root := FileBaselineDir(dataDir)
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil // 从未留过档
			}
			return err
		}
		if info.IsDir() || strings.HasSuffix(p, ".mode") {
			return nil
		}
		// file_baseline/<去掉前导 / 的原路径> ⇒ 还原成 /...
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		out = append(out, string(filepath.Separator)+rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
