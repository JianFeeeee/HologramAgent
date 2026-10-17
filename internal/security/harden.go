package security

// 数据目录与敏感文件的权限收敛。
//
// ## 为什么必须有这一层（与内嵌主密钥配套）
//
// 用户选择把主密钥**内嵌**进二进制（`embedded.go`）。那条路的代价是：
// 密钥就在 `homed` 里，而 `homed` 出厂是 0755、`config.db` 是 0644 ——
// 二者同级可达，于是「能读二进制 ⇒ 能拿密钥 ⇒ 能解密数据库」。
//
// 缓解手段很直接：**把可达性收窄到 root**。这不是把内嵌变成安全设计，
// 而是把它的暴露面从"本机任何用户"缩小到"root"——而那正是唯一本来就
// 能读全部凭据的身份（服务以 root 运行，进程内存里本就有明文）。
//
// ## 为什么写进代码而不是手工 chmod
//
// 手工 chmod 会在下一次重建/换机器/恢复备份时**悄悄失效**，
// 而症状是"加密还在，但保护已经没了"——没人会注意到。
// 所以收敛必须每次启动都跑（幂等），且**收紧失败要响亮**：
// 权限没收紧意味着"以为受保护其实没有"，属于必须知道的安全事实。
//
// ## 只收紧、不放松
//
// 绝不把已经更严的权限改回宽（除非显式要求）：一个被管理员手工设成 0400
// 的文件不该被启动流程"修正"成 0600。所以只在**比目标宽**时才动。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// 目标权限（尽量小；全部服务以 root 运行，实测无其他读者）。
const (
	// DirPerm 数据目录：0750 —— 同组（root）可进入，其他用户不可。
	// 不用 0700 是因为保留 group 访问的余地（将来若拆出专用组，只需改组属主）。
	DirPerm = 0o750

	// FilePerm 敏感文件（config.db 及其 WAL/SHM、密钥、密钥环、变量库）：0600。
	FilePerm = 0o600
)

// HardenResult 报告收敛动作（供日志与自检工具使用）。
type HardenResult struct {
	// Changed 是被实际改动的路径（含从→到）。
	Changed []string
	// Errors 是收紧失败的项。**非空必须响亮**：权限没收紧 = 保护没生效。
	Errors []string
}

// sensitiveFileNames 是需要 0600 的文件/通配（相对数据目录）。
//
// 为什么把 config.db 的 `-wal` / `-shm` 也算上：SQLite 的 WAL 模式会把
// **未合并的最新数据**放在 `-wal` 里 —— 只收紧主库而留下 0644 的 `-wal`，
// 等于把最近写入的（可能正是密钥的）内容继续暴露给所有人。
// 这个坑很隐蔽：`ls -l config.db` 看起来是对的。
var sensitiveFileNames = []string{
	"config.db",
	"config.db-wal",
	"config.db-shm",
	KeyFileName,     // security.key
	KeyringFileName, // security.keyring
}

// sensitiveDirs 是需要 0750 的子目录（相对数据目录）。
var sensitiveDirs = []string{
	"security", // 变量库 variables.json 所在处
}

// HardenDataDir 幂等地把数据目录与敏感文件收敛到最小权限。
//
// 它**不**因为个别失败就中止启动：一个权限收紧失败不该让服务不可用
// （那会把安全加固变成可用性事故）。但失败项会逐条返回，
// 调用方必须**响亮记录**。
func HardenDataDir(dataDir string) HardenResult {
	var res HardenResult
	if dataDir == "" {
		return res
	}

	// ① 数据目录本身
	if ch, err := tightenPath(dataDir, DirPerm); err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", dataDir, err))
	} else if ch != "" {
		res.Changed = append(res.Changed, ch)
	}

	// ② 敏感子目录
	for _, d := range sensitiveDirs {
		p := filepath.Join(dataDir, d)
		if _, err := os.Stat(p); err != nil {
			continue // 不存在就跳过（不是错误）
		}
		if ch, err := tightenPath(p, DirPerm); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
		} else if ch != "" {
			res.Changed = append(res.Changed, ch)
		}
	}

	// ③ 敏感文件
	for _, f := range sensitiveFileNames {
		p := filepath.Join(dataDir, f)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if ch, err := tightenPath(p, FilePerm); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
		} else if ch != "" {
			res.Changed = append(res.Changed, ch)
		}
	}

	// ④ 变量库（在 security/ 下）
	for _, f := range []string{"variables.json", "variables.json.tmp"} {
		p := filepath.Join(dataDir, "security", f)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		if ch, err := tightenPath(p, FilePerm); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
		} else if ch != "" {
			res.Changed = append(res.Changed, ch)
		}
	}

	if len(res.Errors) > 0 {
		// 响亮：权限没收紧意味着"以为受保护其实没有"。
		for _, e := range res.Errors {
			log.Printf("[security] ★★ 权限收紧失败（该项仍可能对同机其他用户可读）: %s", e)
		}
	}
	return res
}

// tightenPath 在**当前权限比目标宽**时收紧到目标。
//
// 返回非空字符串表示确实改了（内容形如 "0755→0750 /path"），
// 便于日志说清"改了什么"。
//
// ★ 只收紧不放松：已经更严（如 0400）的文件保持原样 ——
// 一个被管理员特意设严的文件不该被启动流程"修正"。
func tightenPath(p string, want os.FileMode) (string, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	cur := st.Mode().Perm()
	if cur == want || !isLooser(cur, want) {
		return "", nil
	}
	if err := os.Chmod(p, want); err != nil {
		return "", err
	}
	return fmt.Sprintf("%04o→%04o %s", cur, want, p), nil
}

// isLooser 报告 cur 是否比 want 宽松（任一位是 cur 有而 want 没有的）。
//
// 例如 cur=0644 / want=0600 → 宽松（group/other 可读）；
//
//	cur=0400 / want=0600 → **不**宽松（反而更严，不该动）
func isLooser(cur, want os.FileMode) bool {
	return cur&^want != 0
}
