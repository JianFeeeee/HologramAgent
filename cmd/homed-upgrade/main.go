// Command homed-upgrade 是**内核**升级闭环的 CLI 入口（默认只探测，不写盘）。
//
// 实现全部在 internal/update —— 内核侧的 kernel_update 工具走同一套
// 代码，避免"CLI 能升级、agent 调用却把机器搞坏"这类两份实现的漂移。
//
// ## 范围：只面向内核
//
// 只处理 homed / depgraph.bin。**不碰插件**——插件有独立的升级通道
// （pluginmgr），且与内核的兼容性由"自动禁用不兼容插件"兜底。
//
// ## 为什么默认只探测
//
// 替换二进制不可逆，且内核是这个机器上唯一常驻的关键服务。默认只做
// 探测与预检、把"会发生什么"完整报出来；`-apply` 才真动手。
//
// ## ★ 它**不重启**服务
//
// 这一条原先做错了：CLI 曾直接 `systemctl restart hologramagent`，而
// internal/update/kernel.go 的 Apply 文档专门论证过为什么不能这么做：
//
//	· 杀掉整个 cgroup（含 guard）—— 把监督者一起杀掉
//	· 丢失 exit 42 语义：guard 的 failures / failbackRound 状态全丢
//	· 发命令的进程正是被重启杀掉的进程 ⇒ 返回值不可靠
//
// 正确的重启决策在 guard 手里（只有它知道这次退出该不该重拉）。CLI 是
// 外部管理命令，它的职责到"磁盘上的产物已就绪、pending 已写"为止；
// 重启由**运行中的内核**经 KernelControlAPI 发起（restartCh → exit 42）。
//
// 若服务没在跑，连重启都不需要：下次启动自然加载新二进制并由第 0 层
// update 模块确认或回滚。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func main() {
	pkgPath := flag.String("pkg", "", "本地安装包（.tar.gz）；与 -release 二选一")
	release := flag.Bool("release", false, "从 GitHub 最新 Release 下载 OTA 包（默认仅探测；配合 -apply 执行）")
	binPath := flag.String("bin", "", "当前运行的内核二进制路径（默认取本进程自身；它不是 homed 时必须显式给）")
	pluginsDir := flag.String("plugins", "", "外部插件目录（探测兼容性用；为空则跳过该步）")
	dataDirFlag := flag.String("data", "", "内核数据目录（= 内核的 --data；不填则从 config.db 的 core.daemon.data_dir 读）")
	apply := flag.Bool("apply", false, "真正执行升级（默认只探测+预检）")

	// ★ 发布侧差分生成（不参与升级流程）。
	//
	// 为何放在这个 CLI 而不是单开一个命令：它只是把两个文件喂给
	// internal/update.MakeDeltaForRelease，而那个包已经在这里了。
	// 多发一个二进制意味着多一条要维护的发布链。
	makeDelta := flag.Bool("make-delta", false, "生成差分补丁（发布用）：-delta-old <旧homed> 与 -delta-new <新homed>")
	deltaOld := flag.String("delta-old", "", "旧版本 homed 路径（上一版发布的二进制）")
	deltaNew := flag.String("delta-new", "", "新版本 homed 路径")
	deltaOut := flag.String("delta-out", "kernel.delta", "补丁输出路径")
	flag.Parse()

	if *makeDelta {
		if *deltaOld == "" || *deltaNew == "" {
			fmt.Fprintln(os.Stderr, "-make-delta 需要 -delta-old <旧homed> 与 -delta-new <新homed>")
			os.Exit(2)
		}
		hdr, size, err := update.MakeDeltaForRelease(*deltaOld, *deltaNew, *deltaOut)
		if err != nil {
			fail("生成差分: %v", err)
		}
		st := hdr.Stats(size)
		fmt.Printf("补丁已写出: %s\n", *deltaOut)
		fmt.Printf("  旧大小   %d 字节\n  新大小   %d 字节\n  补丁     %d 字节（%.3f%%）\n",
			st.OldSize, st.NewSize, st.PatchSize, st.Ratio*100)
		fmt.Printf("  基线导入: %s\n", update.ShortSum(hdr.OldSHA256))
		fmt.Printf("  目标导入: %s\n", update.ShortSum(hdr.NewSHA256))
		fmt.Printf("\n发布资产名建议: %s\n",
			update.DeltaAssetName("<version>", "linux", "amd64", update.ShortCommit(hdr.OldSHA256)))
		return
	}

	if *pkgPath == "" && !*release {
		fmt.Fprintln(os.Stderr, "必须提供 -pkg <安装包.tar.gz> 或 -release")
		os.Exit(2)
	}

	// ── 0. 探测：安装形态决定后面每一步该怎么做 ──
	//
	// 必须在**动任何文件之前**做：deb/rpm 形态下直接替换会破坏包管理器
	// 数据库（下次 apt upgrade 覆盖回旧版，或 dpkg 拒绝后续升级）。
	bin := resolveBinPath(*binPath)
	if bin == "" {
		// ★ 拒绝，而不是猜。同目录没有 homed 时曾经兜底到 os.Executable()，
		// 于是“升级 homed”实际变成“把 CLI 自己覆盖成新包里的 homed”。
		fmt.Fprintln(os.Stderr, "找不到内核二进制。")
		fmt.Fprintln(os.Stderr, "本工具默认升级**与自己同目录**的 homed；找不到时请显式指定：")
		fmt.Fprintln(os.Stderr, "  homed-upgrade -bin /usr/bin/homed ...")
		fmt.Fprintln(os.Stderr, "提示：`systemctl show hologramagent -p ExecStart` 里能看到真实路径。")
		os.Exit(2)
	}
	dataDir := resolveDataDir(*dataDirFlag)
	capab := update.Detect(bin, dataDir)
	printCapabilities(capab)

	if *apply {
		if ch := capab.Channel(); ch != update.ChannelSelfReplace {
			// ★ 不是"不支持升级"，是"升级必须由别人执行"。
			// 区分这两件事很重要：前者听起来像缺陷，后者是可执行的操作指引。
			fmt.Printf("\n== 本机不能自替换 ==\n%s\n", capab.Reason)
			if cmd := capab.UpgradeCommand(""); cmd != "" {
				fmt.Printf("\n请执行：\n  %s\n", cmd)
			}
			os.Exit(1)
		}
	}
	if dataDir == "" {
		fmt.Println("\n⚠️ 无法确定内核数据目录：升级确认标记与备份无处可放（无回滚能力）")
		fmt.Println("   请显式指定 -data <dir>（与内核 unit 的 --data 一致）")
		if *apply {
			os.Exit(1)
		}
		fmt.Println("   （探测模式继续，-apply 会被拒绝）")
	}

	tmp, err := os.MkdirTemp("", "homed-upgrade-*")
	if err != nil {
		fail("建临时目录: %v", err)
	}
	defer os.RemoveAll(tmp)

	if *release {
		path := filepath.Join(tmp, "release.tar.gz")
		info, downloadErr := update.DownloadLatestRelease(path)
		if downloadErr != nil {
			fail("下载最新 GitHub Release: %v", downloadErr)
		}
		*pkgPath = path
		fmt.Printf("最新 Release: %s (%s)\n", info.Latest, info.Asset)
	}

	fmt.Printf("\n== 解包 %s ==\n", *pkgPath)
	root, err := update.ExtractPkg(*pkgPath, tmp)
	if err != nil {
		fail("解包: %v", err)
	}

	// ── 1. 当前版本 ──
	cur, curErr := update.ReadBinVersion(bin)
	if curErr != nil {
		fmt.Printf("⚠️ 读当前内核版本失败: %v\n", curErr)
		cur = nil
	} else {
		fmt.Printf("当前内核: %s (commit %s)\n", cur.Version, update.ShortCommit(cur.Commit))
	}

	// ── 2. 新版本 ──
	newBin := update.FindFile(root, "homed")
	if newBin == "" {
		fail("包内找不到 homed")
	}
	nv, err := update.ReadBinVersion(newBin)
	if err != nil {
		fail("新内核无法自报版本（可能不是本仓构建的二进制）: %v", err)
	}
	fmt.Printf("包内内核: %s (commit %s)\n", nv.Version, update.ShortCommit(nv.Commit))

	// ── 3. 版本比较 ──
	fmt.Printf("\n== 结论 ==\n%s\n", update.CompareVersions(cur, nv))

	// ── 4. depgraph 必须与新二进制同批 ──
	newDG := update.FindFile(root, "depgraph.bin")
	if newDG == "" {
		fmt.Println("⚠️ 包内缺 depgraph.bin —— 内核启动会因依赖图不匹配拒绝启动")
	} else {
		fmt.Printf("depgraph.bin: 包内已带（%d 字节），将与 homed 同批替换\n", update.FileSize(newDG))
	}

	// ── 5. 插件兼容性预判 ──
	//
	// ★ 目录为空时**跳过并明说**，不再打印"无需重编"。
	//   此前那个函数读 plugin.json 的 sdk_version 字段，而该字段根本
	//   不存在（生产 32 个插件 grep 命中 0）⇒ 恒返回空 ⇒ 永远打印
	//   "现有插件与新内核 SDK 主版本一致"。那是假话，比不检查更糟。
	reportPluginCompat(*pluginsDir, nv.SDKVersion)

	if !*apply {
		fmt.Println("\n（探测模式：未改动任何文件。确认无误后加 -apply）")
		return
	}

	// ── 6. 真正升级：预检 → 备份 → pending → 原子替换（**不含重启**）──
	out, err := update.Apply(root, bin, newDG, dataDir, nil)
	if err != nil {
		fail("升级失败: %v", err)
	}
	fmt.Printf("\n新内核已就位：%s → %s\n备份保留在 %s\n", out.From.Version, out.To.Version, out.BackupDir)

	// ── 7. 不再自己重启 ──
	//
	// 说清楚接下来会发生什么，而不是替用户拍一个可能破坏 guard 状态的决定。
	fmt.Printf("\n== 下一步 ==\n")
	if update.SystemdActive(update.DefaultServiceUnit) {
		fmt.Printf("内核正在运行，请让它走热重启通道（不丢 guard 状态）：\n")
		fmt.Printf("  · 重启会由内核自己发起（restartCh → exit 42 → guard 用新二进制重拉）\n")
		fmt.Printf("  · 若需手工触发，用 WebUI 的「重启」按钮或 POST /api/v1/kernel/restart\n")
		fmt.Printf("✅ 升级产物已就绪：%s → %s\n", out.From.Version, out.To.Version)
	} else {
		fmt.Printf("服务当前未运行：直接下次启动即生效（pending 标记会确认或回滚）。\n")
		fmt.Printf("✅ 升级产物已就绪：%s → %s\n", out.From.Version, out.To.Version)
	}
	fmt.Printf("\n启动后若新内核起不来，第 0 层 update 模块会自动回滚到 %s。\n", out.From.Version)
}

// resolveBinPath 决定“要升级的那个二进制”在哪。找不到时返回**空串**。
//
// ## ★ 为什么不兜底到 os.Executable()
//
// 这个兜底是真的会搞坏东西的（实测踩到）：
//
//	本 CLI 叫 homed-upgrade，与 homed **不同名**。如果同目录没有 homed
//	（比如开发机、或把 CLI 单独拷到一个空目录里跑），旧兜底会返回
//	**CLI 自己的路径**，于是“升级 homed”实际变成“把 CLI 覆盖成新包里的
//	homed” —— 一个 1 字节的假包就能把工具本体换成一段 shell 脚本。
//
// 静默做错比直接报错危险得多，所以：找不到就返回空，由调用方拒绝。
//
// 取值顺序：
//
//	① -bin（人工显式给，最可靠）
//	② 本 CLI 同目录的 homed（发行包里两者总是同批安装）
func resolveBinPath(explicit string) string {
	if d := strings.TrimSpace(explicit); d != "" {
		return d
	}
	if exe, err := os.Executable(); err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			exe = resolved
		}
		cand := filepath.Join(filepath.Dir(exe), "homed")
		if update.FileExists(cand) {
			return cand
		}
	}
	return ""
}

// resolveDataDir 返回内核**真实**的数据目录（升级标记与备份的落点）。
//
// ## ★ 不猜路径，只认两个显式来源
//
//  1. `-data <dir>`（人工直接给，最高优先）
//  2. config.db 里的 `core.daemon.data_dir`
//
// 都拿不到就返回**空串**，由调用方拒绝 -apply 并说明怎么修。
//
// ## 为什么不再自己开 sqlite
//
// 旧实现用 `sql.Open("sqlite3", ...)` 只读查配置。问题是打包脚本用
// `CGO_ENABLED=0` 编这个 CLI，而 go-sqlite3 在无 cgo 时是 **stub**：
// 查询返回 "requires cgo to work"，错误被吞掉、返回空串 ⇒ 发行包里
// "自动发现 dataDir"这条路径**从未真正工作过**。
//
// 改为复用内核已有的 internal/config.ConfigRegistry：同一个打开逻辑、
// 同一套 key 语义，不需要 CGO 时也能编（它在无配置时返回默认值）。
func resolveDataDir(explicit string) string {
	if d := strings.TrimSpace(explicit); d != "" {
		return d
	}
	// 与内核一致的候选顺序：安装形态优先。
	bin, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err2 := filepath.EvalSymlinks(bin); err2 == nil {
		bin = resolved
	}
	candidates := []string{
		filepath.Join(filepath.Dir(filepath.Dir(bin)), "var/lib/hologram"),
		"/var/lib/hologram",
		filepath.Join(filepath.Dir(filepath.Dir(bin)), "newqqagent"),
		"/home/hologramagent",
		"/home/newqqagent",
	}
	for _, d := range candidates {
		if !update.FileExists(filepath.Join(d, "config.db")) {
			continue
		}
		if v := update.DataDirFromConfigDB(d); v != "" {
			fmt.Printf("数据目录来源: %s/config.db 的 core.daemon.data_dir = %s\n", d, v)
			return v
		}
	}
	return ""
}

func printCapabilities(c update.Capabilities) {
	fmt.Printf("== 本机安装形态 ==\n")
	fmt.Printf("  形态:     %s\n", c.Kind)
	fmt.Printf("  变体:     %s\n", c.Variant)
	fmt.Printf("  二进制:   %s\n", c.BinPath)
	if c.DepGraph != "" {
		fmt.Printf("  依赖图:   %s\n", c.DepGraph)
	}
	if c.PackageName != "" {
		fmt.Printf("  包名:     %s\n", c.PackageName)
	}
	fmt.Printf("  可自替换: %v\n", c.CanWriteBin)
	if !c.CanWriteBin && c.Reason != "" {
		fmt.Printf("  原因:     %s\n", c.Reason)
	}
	fmt.Printf("  升级通道: %s\n", c.Channel())
}

// reportPluginCompat 报告插件与新内核的兼容性预判。
//
// ★ 无数据时**明确说无数据**，不打印"无需重编"。
//
// 旧实现的判据是 plugin.json 的 sdk_version 字段，而内核 manifest 结构里
// 根本没有这个字段（生产 32 个插件 grep 命中 0）⇒ ProbePluginCompat 恒
// 返回空 ⇒ CLI 永远打印"现有插件与新内核 SDK 主版本一致，无需重编"。
//
// 用户读着这句话点 -apply，升完发现一半插件被自动禁用 —— 这不是"没实现"，
// 是"实现了但在撒谎"。真实数据在握手时才有（proc/process.go 的 res.SDKVersion），
// 静态预判拿不到，那就如实说拿不到。
func reportPluginCompat(dir, wantSDK string) {
	if dir == "" {
		fmt.Println("\n== 插件兼容性 ==\n未指定 -plugins，跳过预判（升级后内核会自动禁用不兼容的插件并写明原因）")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("\n== 插件兼容性 ==\n无法读取 %s（%v）：跳过预判\n", dir, err)
		return
	}
	compat := update.ProbePluginCompat(dir, wantSDK)
	if len(compat) > 0 {
		fmt.Printf("\n== 插件兼容性警告（%d 个）==\n", len(compat))
		fmt.Println("换内核后这些插件会握手失败并被**自动禁用**（写入禁用配置 + 原因）：")
		for _, p := range compat {
			fmt.Printf("  · %-16s SDK %s（新内核要求主版本 %s）\n", p.Name, p.SDKVersion, p.WantMajor)
		}
		fmt.Println("它们在界面上会显示为「已禁用」并带原因，重编插件或手动启用即可恢复。")
		return
	}
	// 没有可判定的数据时，说清楚原因，而不是给一个假的"全部兼容"。
	fmt.Printf("\n== 插件兼容性 ==\n共 %d 个插件目录，但**无法静态预判**兼容性：\n", len(entries))
	fmt.Println("  plugin.json 不含 sdk_version 字段（该字段不存在），真实 SDK 版本只在握手时上报。")
	fmt.Println("  ⇒ 升级后内核会逐个握手，主版本不符的会被自动禁用并在界面上写明原因。")
	fmt.Println("  ⇒ 本题结论为「未检查」，**不是**「全部兼容」。")
}

var _ = time.Now
