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
//
// ## ★ 为什么必须临时文件 + fsync + rename
//
// 旧实现直接 os.WriteFile 覆写目标文件。它**不是原子**的：WriteFile
// 先 O_TRUNC 再写，如果这之间掉电（或 OOM-kill、机器硬重启），
// 磁盘上留下的是一个**半截 JSON**。下一轮启动读到半截 JSON 会解析失败，
// 而解析失败在旧代码里被原样上抛 ⇒ 内核拒启动 ⇒ 而回滚分支根本走不到
// ⇒ **既起不来也回滚不了**（这是所有升级故障里最难救的一种）。
//
// 所以要：
//
//	写 <path>.tmp → fsync(文件) → rename(原子替换) → fsync(目录)
//
// rename 在同文件系统内是原子的：任何时刻读到的都是**完整的旧版本**或
// **完整的新版本**，不存在中间态。fsync 目录是为了让 rename 本身也落盘
// （否则掉电后可能连 rename 都不生效，但留下的仍是完整旧文件 —— 安全方向）。
//
// 注意：fsync 失败时**不**当作失败处理。有些文件系统（叠加层/容器/tmpfs）
// 不支持对目录 fsync，返回 EINVAL；此时 rename 已完成、内容已正确，
// 为此拒绝升级只会把可用环境弄成不可用。只记录日志。
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
	tmp := p + ".tmp"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("建临时标记 %s: %w", tmp, err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("写 %s: %w", tmp, err)
	}
	// ★ fsync 必须在 Close 之前：Close 只把数据交给内核页缓存，
	// 掉电仍可能丢。这里失败要报错（内容没落盘 ⇒ 不能假装标记写好了）。
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s → %s: %w", tmp, p, err)
	}
	// 目录 fsync：让 rename 自身落盘。失败仅告警（见函数头注释）。
	if d, derr := os.Open(dataDir); derr == nil {
		if serr := d.Sync(); serr != nil {
			log.Printf("[update] fsync dir %s: %v（rename 已完成，内容正确）", dataDir, serr)
		}
		d.Close()
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
//
// 返回 error 的两种情况：
//
//	· 文件存在但**解析不了**（掉电截断、被外部写坏）
//	· 读失败（权限等）
//
// ★ 调用方**不得**因此拒绝启动 —— 见 RollbackIfUnconfirmed 的处理：
// 解析不了的标记意味着"不知道上次升了什么"，此时保守做法是**保留现场
// 并继续启动**，而不是把内核卡死。启动失败比"可能少回滚一次"严重得多：
// 前者是机器不可用，后者只是多跑一个已知能起得来的版本（能跑到这行
// 说明当前二进制本身是可执行的）。
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
//	rolled=true,  err=nil  —— 已回滚且**回读校验通过**（from/to 记录版本轨迹）
//	rolled=*,     err!=nil —— 回滚失败，调用方**必须中止启动**
//	                          （继续装配等于用一个已知起不来的版本）
//
// ## ★ 回滚的可信性靠"回读校验"，不靠"拷贝没报错"
//
// 这是本函数最重要的不变量：CopyFile 返回 nil 只说明字节搬完了，
// **不说明搬过去的东西是可执行的**。备份文件本身可能是零字节 / 截断 /
// 架构不对（例如磁盘满时写坏的备份），拷贝依旧会成功。
//
// 旧代码就卡在这里：零字节备份 ⇒ CopyFile 成功 ⇒ 清 marker ⇒
// 返回 rolled=true, err=nil ⇒ 日志打 "rolled back to 1.4.0" ⇒
// 调用方以为自愈完成继续启动，而磁盘上躺着的是一个不可执行的二进制。
// 更糟的是 marker 已被清掉 ⇒ **失去第二次自愈机会**。
//
// 所以现在改成：替换后必须 readBinVersionStrict(binPath) 回读，并且版本 ==
// rec.FromVer。任何一环不过都算回滚失败：不清 marker、返回 error、
// 让调用方停在下边（不用坏二进制继续跑）。
func RollbackIfUnconfirmed(dataDir, binPath string) (bool, string, string, error) {
	rec, err := ReadPendingUpgrade(dataDir)
	if err != nil {
		// ★ 解析不了 ≠ 中止启动。
		//
		// marker 被截断（掉电）或写坏时说"不知道上次升了什么"。把内核
		// 卡在启动阶段是最差的选择：能执行到这里已经证明**当前二进制是
		// 可执行的**，而拒绝启动会让机器彻底不可用、且还得人工上机器。
		//
		// 保守做法：告警 + 把坏掉的 marker 改名留存（不删，保留现场供
		// 人工判断），然后当作"无待确认升级"继续启动。代价只是可能少
		// 回滚一次；而不是把一台本来能用的机器搞成起不来。
		quarantined, qerr := quarantineMarker(dataDir)
		if qerr != nil {
			log.Printf("[update] pending marker unreadable (%v) and quarantine failed: %v", err, qerr)
		} else {
			log.Printf("[update] pending marker unreadable, quarantined to %s: %v", quarantined, err)
		}
		log.Printf("[update] 继续启动（不因标记损坏而拒绝启动）；本次不做自动回滚")
		return false, "", "", nil
	}
	if rec == nil {
		return false, "", "", nil
	}

	// ★★★ 先判"这次启动跑的是不是升级后的那个版本"。
	//
	// 这一步是必须的，而且顺序不能反（2026-10-26 真机实测踩到）：
	// 标记是在**替换之前**写的，所以升级后的**第一次启动**必然看到它。
	// 若不比对版本就直接回滚，那么每次升级都会在第一次启动时被立刻撤销 ——
	// 实测表现：`-apply` 报"升级完成"，重启后日志却是
	// "ROLLED BACK kernel 1.4.0dev → 1.4.0dev（上一次升级未能确认启动）"，
	// 也就是升级永远不可能生效。
	//
	// 语义区分（这才是两段式确认的本意）：
	//
	//	当前版本 == 标记里的 To  ⇒ 新版本**已经跑起来了** ⇒ 确认，清标记，继续启动
	//	当前版本 != To（或读不到）⇒ 新版本没能成为运行版本（exec 失败、被 guard 回退等）
	//	                          ⇒ 回滚到备份
	// ★★★ 确认分支**必须**用严格版本（不可用带推断回退的 ReadBinVersion）。
	//
	// 这里曾经用 ReadBinVersion，于是与 B1 是**同一个病**，只是从"回滚后校验"
	// 转移到了"确认"：ReadBinVersion 在 exec 失败时会回退到
	// InferVersionFromRunning()（读 /usr/local/bin/homed 的字符串、或跑
	// /usr/local/bin/waiter --version）。
	//
	// 已实测的事故形态：binPath 是一个不可执行的残文件，marker.ToVer 恰好
	// 等于本机 waiter 的版本 ⇒ 推断回退从 **waiter** 借到 1.4.0/98850e1
	// ⇒ 与 ToVer 相等 ⇒ confirmed=true ⇒ 清 marker、**不回滚** ⇒ 机器带着
	// 跑不起来的二进制继续启动，且失去自愈机会。
	//
	// 而这条缺陷是**环境相关**的：没装 homed/waiter 的机器（如 CI）会绿，
	// 生产装了就会红 —— 最难查的那一类。所以不能靠"本机恰好有没有"来验。
	//
	// 判据只有一条：这个文件自己能不能报出版本。报不出 ⇒ confirmed=false
	// ⇒ 走回滚。绝不用推断值去证明"新版已跑起来"。
	confirmed := false
	if cur, cerr := readBinVersionStrict(binPath); cerr == nil {
		sameVer := cur.Version == rec.ToVer
		sameCommit := rec.ToCommit == "" || cur.Commit == rec.ToCommit
		confirmed = sameVer && sameCommit
	}
	if confirmed {
		if cerr := ClearPendingUpgrade(dataDir); cerr != nil {
			log.Printf("[update] %s 已生效，但清除标记失败: %v", rec.ToVer, cerr)
		} else {
			log.Printf("[update] upgrade to %s confirmed (running binary matches), marker cleared", rec.ToVer)
		}
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
	//
	// ★ 落在 dataDir 下，不是二进制目录：二进制目录通常是 /usr/local/bin，
	// 只读（ProtectSystem=strict）⇒ MkdirAll 静默失败（旧代码没查 err），
	// 现场丢失且日志里只有一条无关的 copy 失败。
	ts := NowStamp()
	brokenDir := BrokenDirPath(dataDir, ts)
	if err := os.MkdirAll(brokenDir, 0755); err != nil {
		// 备份失败不阻断回滚：回滚是主线，留现场是支线
		log.Printf("[update] create broken-binary dir %s failed: %v", brokenDir, err)
	} else if err := CopyFile(binPath, filepath.Join(brokenDir, "homed")); err != nil {
		log.Printf("[update] backup of broken binary failed: %v", err)
	} else {
		log.Printf("[update] broken binary kept at %s", brokenDir)
	}

	// 原子替换回旧版
	if err := CopyFile(backupBin, binPath); err != nil {
		// ★ 不清 marker：拷贝失败时磁盘上可能已是**半新半旧**的文件，
		// 保留标记才有"下次启动再试"的机会。
		return false, "", "", fmt.Errorf("回滚二进制: %w（标记保留，下次启动会再试）", err)
	}

	// ★★★ 回读校验：这是"回滚成功"的**唯一**判据。
	//
	// CopyFile 成功只代表字节搬完了。备份若本身是零字节/截断/架构不对
	// （磁盘满时极易发生），搬过去的东西依旧不可执行。旧代码就在这里
	// 清掉 marker 并报 success，于是机器带着一个不可执行的二进制继续启动，
	// 而且失去了第二次自愈机会。
	// ★ 回读用**严格**版本：不可执行就是不可执行。
	//
	// 不能用 ReadBinVersion：它含"从运行中进程推断"的回退，而这个回退可能
	// 从**另一个文件**（或老进程）拿到版本号，于是把一个零字节的坏备份
	// 验成"回滚成功"—— 这正是本判据要防的那个事故。
	rb, verr := readBinVersionStrict(binPath)
	if verr != nil {
		// 不清 marker：让下次启动再试（也让人有机会介入）
		return false, "", "", fmt.Errorf(
			"回滚后回读失败（%s 不可执行）: %v；marker 保留（备份可能已损坏，需人工用已知良好版本替换 %s）",
			binPath, verr, binPath)
	}
	if rb.Version != rec.FromVer {
		// 版本不符：磁盘上不是我们要回退到的那个版本。不清 marker。
		return false, "", "", fmt.Errorf(
			"回滚后版本校验不符：期望 %s（标记里的 FromVer），实际 %s；marker 保留"+
				"（备份可能与标记不匹配，需人工核验 %s）",
			rec.FromVer, rb.Version, rec.BackupDir)
	}
	log.Printf("[update] rollback verified: %s 回读为 %s", binPath, rb.Version)

	// 回滚 depgraph.bin（必须与 homed 同批）
	if backupDG := filepath.Join(rec.BackupDir, "depgraph.bin"); FileExists(backupDG) {
		if err := CopyFile(backupDG, filepath.Join(filepath.Dir(binPath), "depgraph.bin")); err != nil {
			// ★ 不清 marker：homed 回滚了但 depgraph 没跟上 ⇒ 两者不同批。
			// 这种状态**不能**当成功：内核启动时会因依赖图不匹配而失败，
			// 保留 marker 才能让下次启动再试一遍（且人能看到日志）。
			return false, "", "", fmt.Errorf(
				"回滚 depgraph.bin: %w（homed 已回滚但 depgraph 未跟上，marker 保留）", err)
		}
	}

	// 只有上面全部通过才清 marker：已确认回到旧版且**可执行**，
	// 不存在"待确认的新版"。
	if err := ClearPendingUpgrade(dataDir); err != nil {
		log.Printf("[update] clear pending after rollback: %v", err)
	}

	log.Printf("[update] rolled back to %s（备份 %s，已回读校验）", rec.FromVer, rec.BackupDir)
	return true, rec.FromVer, rec.ToVer, nil
}

// quarantineMarker 把读不懂的 marker 改名留存（不删）。
//
// 为什么改名而不是删除：那个文件里可能有部分可用信息（比如完整的
// from_version 但 to_ 被截断），是排查"上次升级到底发生了什么"的唯一线索。
// 删了就永久没了。改名能让下一次启动干净地当作"无标记"，同时保留现场。
func quarantineMarker(dataDir string) (string, error) {
	src := pendingPath(dataDir)
	dst := src + ".corrupt-" + NowStamp()
	if err := os.Rename(src, dst); err != nil {
		return "", err
	}
	return dst, nil
}
