package update

// 升级确认与回滚（内核启动时的自愈逻辑）。
//
// ## 问题：升级后内核起不来，机器进崩溃循环
//
// 升级 = 换二进制 + 重启。若新二进制有缺陷（依赖图不匹配、协议变了、
// 某个模块 Init 失败），worker 会反复崩溃，guard 只能靠 failback（受限启动）
// 兜底 —— 那是最后手段，不该承担"换错二进制"这种**已知可回滚**的故障。
//
// ## 解法：两段式确认
//
//	升级成功 ──→ 写 pending 标记（"这个版本尚未被确认能启动"）
//	                ↓
//	        重启 → 内核启动 → update 模块（第 0 层）读标记
//	                ↓
//	   ┌────────────┴────────────┐
//   标记还在                     标记已不在
//   （上次升级后没能活到清除）      （上次升级已确认成功）
//        ↓                            ↓
//   回滚到备份版本                  正常继续
//   并清除标记
//
// 清除时机：update 模块跑完自检后立刻清。它只依赖"内核进程活着且能执行到
// 第 0 层"，不依赖任何其他模块 ⇒ 只要内核基本可用就能清除，不会误判。
//
// ## 为什么不用 TTL / 重启次数
//
// 那些是 guard 已有的机制（failback），但判据是"崩了几次"，不是"这次升级
// 该不该撤销"。用重启次数会在"新版本偶发崩溃"时误回滚 —— 而那种情况该修的
// 是崩溃，不是回退版本。这里只认一个事实：**上次升级留下的版本没能启动成功**。

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// PendingFileName 是待确认升级标记的文件名（位于 dataDir 下）。
const PendingFileName = "kernel-upgrade-pending.json"

// PendingRecord 是待确认升级标记的内容。
type PendingRecord struct {
	// FromVer / FromCommit 是升级**前**的版本（回滚目标）。
	FromVer    string `json:"from_version"`
	FromCommit string `json:"from_commit"`
	// ToVer / ToCommit 是升级**后**的版本（当前正在试的）。
	ToVer    string `json:"to_version"`
	ToCommit string `json:"to_commit"`
	// BackupDir 是升级前备份目录（DoUpgrade 创建），回滚从这里取二进制。
	BackupDir string `json:"backup_dir"`
	// At 是写入时间（诊断用）。
	At time.Time `json:"at"`
}

// pendingPath 返回标记文件路径。
func pendingPath(dataDir string) string {
	return filepath.Join(dataDir, PendingFileName)
}

// MarkPendingUpgrade 写入待确认标记（升级成功后立刻调用）。
//
// 由 DoUpgrade 在替换二进制**之前**写：若替换后重启失败，标记一定在。
// 反过来（替换后才写）会在"替换成功但没来得及写标记就崩了"时丢标记，
// 于是下次启动不知道该回滚。
func MarkPendingUpgrade(dataDir string, rec PendingRecord) error {
	if dataDir == "" {
		return fmt.Errorf("dataDir 为空，无法写升级确认标记")
	}
	rec.At = time.Now()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := pendingPath(dataDir)
	if err := os.WriteFile(p, b, 0644); err != nil {
		return fmt.Errorf("写 %s: %w", p, err)
	}
	return nil
}

// ClearPendingUpgrade 清除标记（版本已确认能启动）。
func ClearPendingUpgrade(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	p := pendingPath(dataDir)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清除 %s: %w", p, err)
	}
	return nil
}

// ReadPendingUpgrade 读标记；不存在返回 (nil, nil)。
func ReadPendingUpgrade(dataDir string) (*PendingRecord, error) {
	b, err := os.ReadFile(pendingPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec PendingRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("解析 %s: %w（删掉它即可跳过回滚）", pendingPath(dataDir), err)
	}
	return &rec, nil
}

// RollbackIfUnconfirmed 在发现未确认升级时回滚二进制。
//
// 返回 (rolled, from, to, err)：
//
//	rolled=false, err=nil  —— 没有待确认标记，正常继续
//	rolled=true,  err=nil  —— 已回滚（from/to 记录版本轨迹）
//	rolled=*,     err!=nil —— 回滚失败，调用方**必须中止启动**
//	                          （继续装配等于用一个已知起不来的版本）
func RollbackIfUnconfirmed(dataDir, binPath string) (bool, string, string, error) {
	rec, err := ReadPendingUpgrade(dataDir)
	if err != nil {
		return false, "", "", err
	}
	if rec == nil {
		return false, "", "", nil
	}

	log.Printf("[update] found unconfirmed upgrade: %s → %s（上次升级后内核未能启动成功）",
		ShortCommit(rec.FromCommit), ShortCommit(rec.ToCommit))

	// 回滚源：升级前备份的 homed
	backupBin := filepath.Join(rec.BackupDir, "homed")
	if !FileExists(backupBin) {
		// 备份不在 ⇒ 无法回滚。此时**中止启动**而不是硬上：
		// 继续跑这个已知有问题的版本只会进崩溃循环。
		return false, "", "", fmt.Errorf(
			"备份不存在（%s）：无法回滚。请人工用上一个已知良好版本替换 %s，"+
				"或删除 %s 后重试（放弃回滚）", backupBin, binPath, pendingPath(dataDir))
	}

	// 先备份"当前这个坏的"——万一回滚目标也有问题，至少能回到现场
	ts := NowStamp()
	brokenDir := filepath.Join(filepath.Dir(binPath), "homed-broken-"+ts)
	os.MkdirAll(brokenDir, 0755)
	if err := CopyFile(binPath, filepath.Join(brokenDir, "homed")); err != nil {
		// 备份失败不阻断回滚：回滚是主线，留现场是支线
		log.Printf("[update] backup of broken binary failed: %v", err)
	} else {
		log.Printf("[update] broken binary kept at %s", brokenDir)
	}

	// 原子替换回旧版
	if err := CopyFile(backupBin, binPath); err != nil {
		return false, "", "", fmt.Errorf("回滚二进制: %w", err)
	}

	// 回滚 depgraph.bin（必须与 homed 同批）
	if backupDG := filepath.Join(rec.BackupDir, "depgraph.bin"); FileExists(backupDG) {
		if err := CopyFile(backupDG, filepath.Join(filepath.Dir(binPath), "depgraph.bin")); err != nil {
			return false, "", "", fmt.Errorf("回滚 depgraph.bin: %w", err)
		}
	}

	// 清标记：已回到旧版，不再有"待确认的新版"
	if err := ClearPendingUpgrade(dataDir); err != nil {
		log.Printf("[update] clear pending after rollback: %v", err)
	}

	log.Printf("[update] rolled back to %s（备份 %s）", rec.FromVer, rec.BackupDir)
	return true, rec.FromVer, rec.ToVer, nil
}
