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
	"strings"
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
	// BackupDir 是升级前备份目录（Apply 创建），回滚从这里取二进制。
	BackupDir string `json:"backup_dir"`

	// HadDepGraph 记录升级**前**机器上是否存在 depgraph.bin。
	//
	// ★ 为什么必须记（这是一个真实的崩溃循环）：新包带了 depgraph、而原机器
	//   没有时，回滚只把 homed 换回旧版、不动 depgraph ⇒ 磁盘上是
	//   「旧 homed + 新 depgraph」。旧 homed 内嵌的 digest 对不上，
	//   kernelboot 装载期直接拒绕启动，而 marker 此时已被清 ⇒
	//   **既起不来也回滚不了**。零值 false 与“旧 marker 无此字段”天然兼容。
	HadDepGraph bool `json:"had_depgraph"`

	// Digests 是升级前各目标文件的 sha256（key 为**绝对路径**）。
	//
	// ★ 为什么不能用 `--version` 当回滚判据：旧内核（1.3.x 及以前）
	//   **没有 --version flag**，而旧内核恰恰是最常见的回滚目标。
	//   强行要求它自报版本 = 从旧版本升级失败后永远回滚不了（strict
	//   回读失败 → 拒绝启动 → 下次启动仍同一条路径）。
	//   digest 是“恢复的是当时那份文件”的证据，与二进制支不支持 --version 无关。
	//
	// 兼容性：缺失时回退到“能执行 + 版本相等”（旧逻辑），
	// 新写的人一定带它；老 marker 也能继续工作。
	Digests map[string]string `json:"digests,omitempty"`

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

	// ★★★ 先判"这次启动跑的是不是升级后的那个版本"。
	//
	// 这一步是必须的，而且顺序不能反（2026-10-26 真机实测踩到）：
	// 标记是在**替换之前**写的，所以升级后的**第一次启动**必然看到它。
	// 若不比对版本就直接回滚，那么每次升级都会在第一次启动时被立刻撤销。
	//
	// 语义区分（这才是两段式确认的本意）：
	//
	//	当前版本 == 标记里的 To  ⇒ 新版本**已经跑起来了** ⇒ 确认，清标记，继续启动
	//	当前版本 != To（或读不到）⇒ 新版本没能成为运行版本（exec 失败、被 guard 回退等）
	//	                          ⇒ 回滚到备份
	//
	// ★★★ 确认分支**必须**用严格版本（不可用带推断回退的 ReadBinVersion）。
	//
	// 这里曾经用 ReadBinVersion，于是与回滚校验是同一个病：它在 exec 失败时会
	// 回退到 InferVersionFromRunning()（读 /usr/local/bin/homed 的字符串、或跑
	// /usr/local/bin/waiter --version）。已实测的事故形态：binPath 是一个
	// 不可执行的残文件，marker.ToVer 恰好等于本机 waiter 的版本 ⇒ 推断回退从
	// **waiter** 借到相同版本 ⇒ 与 ToVer 相等 ⇒ confirmed=true ⇒ 清 marker、
	// **不回滚** ⇒ 机器带着跑不起来的二进制继续启动，且失去自愈机会。
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
		log.Printf("[update] create broken-binary dir %s failed: %v", brokenDir, err)
	} else if err := CopyFileAtomic(binPath, filepath.Join(brokenDir, "homed")); err != nil {
		// 备份失败不阻断回滚：回滚是主线，留现场是支线
		log.Printf("[update] backup of broken binary failed: %v", err)
	} else {
		log.Printf("[update] broken binary kept at %s", brokenDir)
	}

	// 原子替换回旧版
	if err := CopyFileAtomic(backupBin, binPath); err != nil {
		// ★ 不清 marker：拷贝失败时磁盘上可能已是**半新半旧**的文件，
		// 保留标记才有"下次启动再试"的机会。
		return false, "", "", fmt.Errorf("回滚二进制: %w（marker 保留，下次启动会再试）", err)
	}

	// ★★★ 回滚的可信性判据：**digest**，不是 `--version`。
	//
	// ## 为什么不能要求 `--version`（这是 PR #31 引入的兼容性回归）
	//
	// 旧内核（1.3.x 及以前）**没有 `--version` flag** —— 它是 2026-10-25
	// 才加的。而旧内核恰恰是最常见的回滚目标：从 1.3.0 升到 1.4.0、
	// 1.4.0 起不来，要回滚的位置就是 1.3.0。
	//
	// 要求回滚后读版本，结果就是：文件已正确还原成可执行的 1.3.0，
	// 但读取严格版本失败 ⇒ 报“回滚失败” ⇒ kernelmod/update 拒绝启动
	// ⇒ **从旧版本升级失败后永久回滚不了**，而 marker 保留使其卡在
	// 同一个循环里。
	//
	// ## digest 为什么是更强的判据
	//
	// 要证明的不是“它能报版本”，而是“恢复的就是升级前那一份”。
	// marker 里记了升级前每个文件的 sha256，回滚后逐一校对 ——
	// 比 `CopyFile 没报错` 强（字节搬完 ≠ 内容一致），也比 `--version`
	// 强（后者对旧版本不可用，对零字节备份可能撞到别的版本）。
	rb, verr := verifyRestored(rec, binPath)
	if verr != nil {
		// 不清 marker：让下次启动再试（也让人有机会介入）
		return false, "", "", fmt.Errorf(
			"回滚后校验失败: %v；marker 保留（备份可能已损坏，需人工用已知良好版本替换 %s）",
			verr, binPath)
	}

	// 回滚 depgraph.bin（必须与 homed 同批）
	//
	// ★ 三种情况，缺一不可（旧实现只处理了第一种）：
	//
	//	原来有 + 备份有  → 还原
	//	原来有 + 备份无  → 不算成功（两文件不同批）
	//	原来无           → **删掉**新包装上的那份，否则旧 homed 的内嵌
	//	                  digest 对不上 ⇒ kernelboot 拒绕启动 ⇒ 崩溃循环
	if err := restoreDepGraph(rec, binPath); err != nil {
		// ★ 不清 marker：homed 回滚了但 depgraph 没跟上 ⇒ 两者不同批。
		// 这种状态**不能**当成功：内核启动时会因依赖图不匹配而失败，
		// 保留 marker 才能让下次启动再试一遍（且人能看到日志）。
		return false, "", "", err
	}

	// 只有上面全部通过才清 marker：已确认回到旧版且**可执行**，
	// 不存在"待确认的新版"。
	if err := ClearPendingUpgrade(dataDir); err != nil {
		log.Printf("[update] clear pending after rollback: %v", err)
	}

	log.Printf("[update] rolled back to %s（备份 %s，已校验）", rb, rec.BackupDir)
	return true, rec.FromVer, rec.ToVer, nil
}

// verifyRestored 校验回滚后的状态，返回它现在报的版本（仅作日志用）。
//
// 判据优先级（新 marker 走 digest，旧 marker 降级）：
//
//	① marker 里有 Digests[binPath] ⇒ 逐字节 sha256 比对（最可靠）
//	② 没有 digest（旧 marker）⇒ 至少要求能执行且版本 == FromVer
//	③ FromVer 为空 ⇒ 只要求能执行
//
// ★ 降级路径不删除：现场可能还躺着旧 marker（升级进行中、marker 由旧版
// 本写的），而那种 marker 没有 digests。让它显式降级，而不是直接失败。
func verifyRestored(rec *PendingRecord, binPath string) (string, error) {
	abs, err := filepath.Abs(binPath)
	if err != nil {
		abs = binPath
	}
	want, hasDigest := rec.Digests[abs]
	if !hasDigest {
		want, hasDigest = rec.Digests[binPath]
	}
	if hasDigest {
		got, serr := sha256File(binPath)
		if serr != nil {
			return "", fmt.Errorf("计算 %s 的 sha256: %w", binPath, serr)
		}
		if !strings.EqualFold(got, want) {
			return "", fmt.Errorf("digest 不符：期望 %s，实际 %s", ShortSum(want), ShortSum(got))
		}
		// digest 对上已足够。版本仅用于日志：老内核报不出就报 "unknown"。
		if v, verr := readBinVersionStrict(binPath); verr == nil {
			return v.Version, nil
		}
		return rec.FromVer, nil
	}

	// 旧 marker：没有 digest，降级为“还原内容 == 备份内容”。
	//
	// ★ 为什么不再用 `--version`（PR #31 的回归）：老内核没有这个 flag，
	// 而老内核恰恰是最常见的回滚目标 —— 要求它就把“从旧版升级失败”
	// 变成永久不能回滚。
	//
	// 这个降级只能证明“拷过去的就是备份里那份”，不能证明备份本身完好 ——
	// 而在旧 marker 上没有更好的证据可用（digest 是本次新增的字段）。
	// 新写的人一定有 digest，所以这个分支只在升级进行中、跨版本的
	// 遗留 marker 上生效。
	backupBin := filepath.Join(rec.BackupDir, filepath.Base(binPath))
	wantSum, werr := sha256File(backupBin)
	if werr != nil {
		return "", fmt.Errorf("备份不可读（%s）: %w", backupBin, werr)
	}
	gotSum, gerr := sha256File(binPath)
	if gerr != nil {
		return "", fmt.Errorf("读回 %s: %w", binPath, gerr)
	}
	if !strings.EqualFold(gotSum, wantSum) {
		return "", fmt.Errorf("还原内容与备份不一致（期望 %s，实际 %s）",
			ShortSum(wantSum), ShortSum(gotSum))
	}
	// 备份可能与 marker 的 FromVer 不符（备份被别的升级覆盖过）。
	// 读得到版本就校，读不到（老内核）就放行 —— 内容一致已经是本分支的上限。
	if v, verr := readBinVersionStrict(binPath); verr == nil {
		if rec.FromVer != "" && v.Version != rec.FromVer {
			return "", fmt.Errorf("版本不符：期望 %s（标记里的 FromVer），实际 %s",
				rec.FromVer, v.Version)
		}
		return v.Version, nil
	}
	return rec.FromVer, nil
}

// restoreDepGraph 把 depgraph.bin 恢复到升级前的状态。
//
// 这个函数的存在理由是一个真实的崩溃循环：新包带了 depgraph、而原机器
// 没有时，回滚只换 homed 不删 depgraph ⇒ 「旧 homed + 新 depgraph」。
// 生产构建的 homed 会校验编译期内嵌的图 digest，不匹配则**拒绝启动**，
// 而 marker 此时已被清 ⇒ 既起不来也回滚不了。
func restoreDepGraph(rec *PendingRecord, binPath string) error {
	dgPath := filepath.Join(filepath.Dir(binPath), "depgraph.bin")

	// ★ 先看备份里到底有没有 depgraph，再看 marker 怎么说。
	//
	// 为什么不能只看 rec.HadDepGraph：这个字段是本次新增的，**存量 marker
	// 里都是零值 false**。而一个从旧版本升上来的机器显然是有 depgraph 的
	// （打包脚本强制同装）。只看字段会把“该还原”误判成“该删除”。
	//
	// 反向也成立：字段说 true 但备份里没有，说明备份不完整 —— 那时不能
	// 把依图留着不管（会与旧 homed 不批），必须报错。
	//
	// 所以：**备份里有就是事实**，它优先级高于 marker 字段。
	backupDG := filepath.Join(rec.BackupDir, "depgraph.bin")
	haveBackup := FileExists(backupDG)

	if !haveBackup && !rec.HadDepGraph {
		// 升级前没有 ⇒ 现在也不该有。
		if err := os.Remove(dgPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("移除升级时新增的 %s: %w（marker 保留）", dgPath, err)
		}
		log.Printf("[update] removed %s（升级前不存在）", dgPath)
		return nil
	}

	if !haveBackup {
		return fmt.Errorf("备份缺 depgraph.bin（%s）：homed 已回滚但依图未跟上，marker 保留", backupDG)
	}
	if err := CopyFileAtomic(backupDG, dgPath); err != nil {
		return fmt.Errorf("回滚 depgraph.bin: %w（homed 已回滚但 depgraph 未跟上，marker 保留）", err)
	}

	// 有 digest 就校：依图错了一样本会拒绕启动。
	abs, _ := filepath.Abs(dgPath)
	if want, ok := rec.Digests[abs]; ok {
		if got, serr := sha256File(dgPath); serr != nil || !strings.EqualFold(got, want) {
			return fmt.Errorf("depgraph.bin digest 不符（marker 保留）")
		}
	}
	log.Printf("[update] restored %s", dgPath)
	return nil
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
