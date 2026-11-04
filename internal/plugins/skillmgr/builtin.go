package skillmgr

// 内核自带的 skill：把**技术机制**写进 skill，由 skillmgr 注入，而不是塞进
// 系统提示词。
//
// ## 为什么（用户 2026-10-26 的两次裁定）
//
//  1. 「系统提示词把机制讲得太深太技术化，导致 agent 说话没人味」——
//     机制留在提示词里，模型会连**语气**一起模仿：回复变成说明书腔。
//  2. 「技术相关下沉到 skill，由 skillmgr 自行管理注入」——
//     提示词只留"必须做什么"，"怎么实现"由 skill 讲。
//
// 所以提示词里只剩一两句行为约束，机制在下面这四个 skill 正文里：
//
//	kernel-memory      记忆分层与 coldcontext_query 的回填语义
//	kernel-output      同步/异步通道怎么真正送达用户
//	kernel-tool-order  一次回复里多工具调用的调度与例外
//	kernel-plugins     按需插件怎么发现与拉起
//
// ## 为什么内嵌在二进制里，而不是让用户自己装
//
// 它们是**内核行为**的一部分：提示词引用了它们，就必须保证它们一定存在。
// 若依赖外部安装，用户删掉/改名后提示词会指向不存在的 skill —— 那比不写更糟。
//
// ## 落盘纪律：不覆盖用户版本
//
// 只在目标目录**不存在**时写入（与 adapters 的 `.bundled` 语义一致）。
// 用户改过的同名 skill 一律保留：内核不该覆盖人的修改。

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed builtin
var builtinSkills embed.FS

// builtinSkillDir 是内嵌 skill 的根目录名。
const builtinSkillDir = "builtin"

// skillFileName 与 plugin.LoadSKILL 认的文件名一致。
const skillFileName = "SKILL.md"

// builtinStampName 记录"上次由内核写入的版本"的摘要。
//
// ★ 为什么需要它（2026-11-04 实测缺陷）：
//
// 原规则是"目标目录已存在就跳过"，本意是保住用户改动。但它把两件事混成一件：
//
//	用户改过的版本   ⇒ 应该保留
//	内核自己写过的旧版 ⇒ 应该随内核更新
//
// 两者都表现为"目录已存在"，于是 **内核改名/改文档后，内置 skill 永远
// 更新不到已部署机器**。实测：b850fea 把 `memory_recall` 改成
// `stablemem_recall`，生产机的 kernel-memory 一直停在旧名，
// 模型照着旧名调工具必然失败。
//
// 现在用摘要区分：把写入时的内容 sha256 记进 `.builtin-sha256`。
//
//	当前内容 == 记录的 sha256  ⇒ 没人改过 ⇒ 可以安全覆盖成新版
//	当前内容 != 记录的 sha256  ⇒ 用户改过 ⇒ 保留，只在日志里说明
//	没有 stamp 文件（历史遗留）⇒ 保守处理，视为"用户版本"，不覆盖
//
// 保守默认很重要：老机器上没有 stamp，宁可不更新也不要覆盖掉人的修改。
const builtinStampName = ".builtin-sha256"

// materializeBuiltinSkills 把内嵌 skill 落到 skillsDir，返回新写入的数量。
//
// 覆盖规则见 builtinStampName 的说明：只有"内核写的且未被改动"才覆盖。
func materializeBuiltinSkills(skillsDir string) int {
	if skillsDir == "" {
		return 0
	}
	entries, err := builtinSkills.ReadDir(builtinSkillDir)
	if err != nil {
		log.Printf("[skillmgr] read embedded skills: %v", err)
		return 0
	}
	written := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dstDir := filepath.Join(skillsDir, name)
		data, readErr := fs.ReadFile(builtinSkills, builtinSkillDir+"/"+name+"/"+skillFileName)
		if readErr != nil {
			log.Printf("[skillmgr] embedded skill %s: %v", name, readErr)
			continue
		}
		// 已存在：判断是不是"内核写的且没人改过"，是才覆盖。
		if _, statErr := os.Stat(dstDir); statErr == nil {
			if !builtinSkillUnmodified(dstDir) {
				// 没有 stamp / 内容被改过 ⇒ 默认保留（人可能改过措辞）。
				//
				// ★ 但有一种情况必须升级：这个是**内核在 stamp 机制之前写的**
				// 旧版本。判据 = 磁盘上的 frontmatter version 明确低于内嵌版本。
				//
				// 为什么需要这一条（实测缺陷）：stamp 机制由 387ae94 引入，
				// 而生产机上 kernel-plugins 是更早的内核写的（mtime 2026-10-26），
				// 没有 .builtin-sha256 ⇒ builtinSkillUnmodified 恒为 false
				// ⇒ **这份 skill 永不更新**。后果是内核改进了 skill 文档，
				// 已部署的机器永远拿不到 —— 而这个 skill 正是教模型怎么用工具的。
				//
				// 为什么用 version 而不是内容差异：用户手工改措辞时通常不动
				// version，所以"version 更高"是"内核发布了新版"的可靠信号；
				// 反过来，人把 version 改高的情况几乎不存在。
				if !builtinSkillOutdated(dstDir, data) {
					continue // 用户改过（或无法确认）：保留人的版本
				}
				log.Printf("[skillmgr] upgrading stale builtin skill %s "+
					"(no stamp, on-disk version older than embedded)", name)
			}
			if cur, err := os.ReadFile(filepath.Join(dstDir, skillFileName)); err == nil && string(cur) == string(data) {
				continue // 内容已是最新，不用重写（保留 mtime）
			}
			if wErr := os.WriteFile(filepath.Join(dstDir, skillFileName), data, 0644); wErr != nil {
				log.Printf("[skillmgr] update %s: %v", dstDir, wErr)
				continue
			}
			writeBuiltinStamp(dstDir, data)
			log.Printf("[skillmgr] updated builtin skill %s (kernel-owned, unmodified by user)", name)
			written++
			continue
		}
		if mkErr := os.MkdirAll(dstDir, 0755); mkErr != nil {
			log.Printf("[skillmgr] create %s: %v", dstDir, mkErr)
			continue
		}
		if wErr := os.WriteFile(filepath.Join(dstDir, skillFileName), data, 0644); wErr != nil {
			log.Printf("[skillmgr] write %s: %v", dstDir, wErr)
			continue
		}
		writeBuiltinStamp(dstDir, data)
		written++
	}
	if written > 0 {
		log.Printf("[skillmgr] materialized %d kernel skills (mechanics live here, not in the system prompt)", written)
	}
	return written
}

// builtinSkillUnmodified 报告 skillsDir 下的 skill 是否"内核写的且未被改动"。
//
// 判定依据是 .builtin-sha256 与**当前** SKILL.md 的实际摘要是否一致。
// 没有 stamp（历史遗留或用户手动放置）⇒ 返回 false（保守：不覆盖）。
func builtinSkillUnmodified(dir string) bool {
	stamp, err := os.ReadFile(filepath.Join(dir, builtinStampName))
	if err != nil {
		return false
	}
	cur, err := os.ReadFile(filepath.Join(dir, skillFileName))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(stamp)) == sha256Hex(cur)
}

// writeBuiltinStamp 记录本次写入内容的摘要，供下次判断"有没有人改过"。
//
// 写失败只记日志、不影响 skill 本身可用：没有 stamp 的后果是
// "下次不会被自动更新"（保守侧），而不是 skill 坏了。
func writeBuiltinStamp(dir string, data []byte) {
	if err := os.WriteFile(filepath.Join(dir, builtinStampName), []byte(sha256Hex(data)), 0644); err != nil {
		log.Printf("[skillmgr] write stamp %s: %v", dir, err)
	}
}

// sha256Hex 返回 b 的十六进制 sha256。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// builtinSkillOutdated 判断磁盘上的 skill 是否是**内核写的旧版本**。
//
// 只在没有 stamp 时才被调用（有 stamp 的走 unmodified 判定）。
//
// 判据：磁盘文件的 frontmatter `version` 严格低于内嵌版本。
//
// ## 为什么这条判据是安全的
//
// 它只在"两份都能解析出版本号"且"磁盘更低"时返回 true：
//
//	人手工改措辞      → 通常不动 version ⇒ 不低于 ⇒ 保留（正确）
//	人手工把版本改高   → 不触发升级 ⇒ 保留（正确，虽然几乎不会发生）
//	内核旧版（无 stamp）→ 磁盘版本更低 ⇒ 升级（这正是要修的缺陷）
//	解析不出版本号     → 返回 false ⇒ 保留（保守）
//
// 也就是说：**误升级的唯一条件是"人把 version 改低了"**，而那种改动
// 本身就是要放弃内核版本的意思，却反而被升级——接受这个极小的风险，
// 换取"内核能更新 skill"这个基本能力。
func builtinSkillOutdated(dir string, embedded []byte) bool {
	cur, err := os.ReadFile(filepath.Join(dir, skillFileName))
	if err != nil {
		return false
	}
	diskVer := skillVersionOf(string(cur))
	embVer := skillVersionOf(string(embedded))
	if diskVer == "" || embVer == "" {
		return false
	}
	return compareVersion(diskVer, embVer) < 0
}

// skillVersionOf 从 SKILL.md 的 frontmatter 里取 version。
//
// 只认 frontmatter 块（首个 --- 到闭合 ---），不扫正文 —— 正文里的
// `version:` 可能是示例代码，误读会让判据做出错误决定。
func skillVersionOf(content string) string {
	lines := strings.Split(content, "\n")
	in := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if i == 0 && t == "---" {
			in = true
			continue
		}
		if !in {
			continue
		}
		if t == "---" {
			break // frontmatter 结束
		}
		if strings.HasPrefix(strings.ToLower(t), "version:") {
			v := strings.TrimSpace(t[len("version:"):])
			v = strings.Trim(v, "\"'")
			return v
		}
	}
	return ""
}

// compareVersion 按点分段数值比较，返回 -1 / 0 / 1。
//
// 非数值段按字符串比较（例如 "1.0.0-beta"），空段视为 0。
// 不做语义化版本的完整解析 —— 这里只需要"新是否旧"，越简单越不会错。
func compareVersion(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		// ★ 缺失段必须视为 "0"，不能留空串。
		//
		// 判据实测抓到过：留空串时 Atoi("") 失败 ⇒ 落到字符串比较
		// ⇒ `"" < "0"` ⇒ "1.0" 被判成低于 "1.0.0"（错误，二者等价）。
		// 后果是版本号少写一段的 skill 会被无谓地重复升级。
		av, bv := "0", "0"
		if i < len(as) && as[i] != "" {
			av = as[i]
		}
		if i < len(bs) && bs[i] != "" {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		// 非数值段：字符串比较
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}
