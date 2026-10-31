package update

// 备份目录的垃圾回收。
//
// ## 为什么必须有 GC
//
// 每次 Apply 都在 dataDir 下留一个 homed-upgrade-backup-<ts>（homed +
// depgraph.bin，通常几十 MB），每次启动时回滚坏二进制还会多一个
// homed-broken-<ts>。旧代码里 Outcome.BackupDir 的注释写着
// "确认稳定后可删"，但**全仓没有一行删除逻辑** —— 注释承诺了一个
// 不存在的机制，而这类注释比没注释更糟：读代码的人会以为已经处理了。
//
// 不删的后果不是"占点磁盘"这么温和：dataDir 和 pending 标记在同一个
// 文件系统上，磁盘被备份吃满之后 pending 标记就写不进去 ⇒ Apply 拒绝
// 升级（H1 的保护是对的，但保护频繁触发本身就是故障）。
//
// ## 保留策略：upgrade 备 3 个，broken 备 1 个
//
// 为什么不对称：
//
//	· homed-upgrade-backup-* 是**回滚源**，可能被任意一次"上次升级的
//	  pending 标记"引用。多留几个才能覆盖"升级→重启→再升级"的连环操作，
//	  避免 GC 把当下那次升级还需要的那一份删掉。留 3 个够。
//	· homed-broken-* 是**事后现场**，只有人工排查时才看，且总是看最近
//	  一次。留 1 个（而非 0 个）是为了不丢现场。
//
// 排序用目录名而不是 mtime：名字里的 ts 是 NowStamp() 的
// "20060102-150405"（定长、字典序 == 时间序），不依赖文件系统时间戳的
// 精度和时区，也不会被一次 cp -a 打乱。

import (
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// BackupDirPrefix 是升级前备份目录名的前缀，需与 Apply 中拼接的一致。
	BackupDirPrefix = "homed-upgrade-backup-"
	// BrokenDirPrefix 是回滚时留存的"坏二进制"目录名前缀。
	BrokenDirPrefix = "homed-broken-"

	// keepUpgradeBackups 保留的升级备份个数（回滚源，需要覆盖连环升级）。
	keepUpgradeBackups = 3
	// keepBrokenDirs 保留的坏二进制现场个数（只用于事后排查）。
	keepBrokenDirs = 1
)

// BrokenDirPath 返回某个时间戳的坏二进制留档目录（位于 dataDir 下）。
//
// ★ 为什么不放在二进制目录旁：二进制目录通常是 /usr/local/bin，
// 在 ProtectSystem=strict 下只读，写不进去就等于**丢掉现场**。
func BrokenDirPath(dataDir, ts string) string {
	return filepath.Join(dataDir, BrokenDirPrefix+ts)
}

// PruneOldBackups 清理 dataDir 下过量的备份目录。
//
// 保留最近 keepUpgradeBackups 个 homed-upgrade-backup-* 与最近
// keepBrokenDirs 个 homed-broken-*，其余 RemoveAll。
//
// 设计上的克制（这几条都是为了"GC 本身不能造成故障"）：
//
//	· dataDir 为空 ⇒ 直接返回。空值曾被当作"当前目录"，在这类函数里
//	  那就是灾难（会去删 CWD 下匹配前缀的东西）。
//	· 任何一次 RemoveAll 失败都只 log，不返回 error —— 调用点在 Apply
//	  成功路径的末尾，GC 失败绝不能让一次**已经成功的升级**变成失败。
//	· 只处理目录、只处理名字精确匹配 "<前缀><ts>" 形态的项（ts 必须是
//	  NowStamp 的形状，见 isBackupStamp）—— 不递归、不碰 pending 标记、
//	  不碰名字碰巧带同样前缀的其它东西。GC 的删除权只能给"确实是自己
//	  造的东西"。
func PruneOldBackups(dataDir string) {
	if dataDir == "" {
		return
	}
	pruneByPrefix(dataDir, BackupDirPrefix, keepUpgradeBackups)
	pruneByPrefix(dataDir, BrokenDirPrefix, keepBrokenDirs)
}

// isBackupStamp 判断 s 是否是 NowStamp() 的形状（20060102-150405）。
//
// 只认定长数字+一个分隔短横：定长保证字典序 == 时间序，数字保证
// 不会被任意名字混进来。
func isBackupStamp(s string) bool {
	if len(s) != len("20060102-150405") {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// pruneByPrefix 保留名字以 prefix 开头的最新 keep 个目录，其余删除。
func pruneByPrefix(dir, prefix string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 目录还不存在（首次升级）就没什么可清的
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue // 只删目录：磁盘满时备份里可能还有半截文件，按名字处理不到就跳过
		}
		n := e.Name()
		if !strings.HasPrefix(n, prefix) || len(n) == len(prefix) {
			continue
		}
		// ★ 后缀必须是时间戳形态。
		//
		// 只查前缀不查后缀的话，一个名字碰巧以该前缀开头的目录（如手工建的
		// homed-upgrade-backup-manual）会被当成备份，且因为 'm' > '9' 会排在
		// 时间戳**前面**当作"最新" ⇒ 它挤掉真正的备份、然后真正的最新备份
		// 反被删。GC 只应动它自己造的东西。
		if !isBackupStamp(n[len(prefix):]) {
			continue
		}
		names = append(names, n)
	}
	if len(names) <= keep {
		return
	}
	// 名字里的时间戳定长且字典序 == 时间序（见文件头），倒序即最新在前。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[keep:] {
		p := filepath.Join(dir, n)
		if err := os.RemoveAll(p); err != nil {
			log.Printf("[update] prune %s failed: %v", p, err)
			continue
		}
		log.Printf("[update] pruned old backup %s（保留最近 %d 个）", p, keep)
	}
}
