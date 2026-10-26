// Command homed-upgrade 是**内核**升级闭环的 CLI 入口（默认只探测，不写盘）。
//
// 实现全部在 internal/update —— 内核侧的 kernel_update 工具走同一套
// 代码，避免"CLI 能升级、agent 调用却把机器搞坏"这类两份实现的漂移。
//
// ## 范围：只面向内核
//
// 只处理 homed / depgraph.bin / waiter 三个文件。**不碰插件**——插件有独立的
// 升级通道（pluginmgr），且插件与内核的兼容性由"自动禁用不兼容插件"处理
// （见 internal/plugin/registry.go 的 maybeAutoDisableIncompatible）：
// 换内核后旧 SDK 编的插件会握手失败，内核把它持久禁用并写明原因，
// 用户在界面上直接看到"为什么没了"，而不是翻日志。
//
// ## 为什么默认只探测
//
// 替换二进制不可逆，且内核是这个机器上唯一常驻的关键服务。默认只做
// 探测与预检、把"会发生什么"完整报出来；`-apply` 才真动手。
// 沿同仓 homed-kb-migrate / homed-predicate-migrate 的惯例。
//
// ## 探测什么（-probe）
//
//  1. 当前运行的内核版本（从 /proc 找进程，读它自己的 --version 输出）
//  2. 新包里的内核版本
//  3. 两者比较：能否升级、是不是降级、commit 是否真的变了
//  4. depgraph.bin 与新二进制是否匹配（包内自带，必须同批替换）
//  5. 现有插件与**新**内核的兼容性预判（SDK 主版本）
//
// 第 5 条是 OTA 里最容易出事的地方：内核换了、插件没换。这里提前算出来，
// 好让用户在动手前就知道"升完会有 N 个插件被自动禁用"。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// fail 打印错误并以非零退出（CLI 专属；库内部一律返回 error）。
// upgradeDataDir 推导数据目录（升级确认标记的落点）。
//
// 与生产一致：内核按 `--data <dir>` 运行，标记必须落在同一个 dataDir 下，
// 否则 update 模块读不到它。推导规则：
//
//	① 环境变量 HOMEAGENT_DATA_DIR（部署方显式指定）
//	② 二进制同级的 ../newqqagent（/usr/local/bin/homed → /home/newqqagent）
//	   —— 太脆弱，改为最常见的部署形态：/usr/local/bin/homed → /home/newqqagent
//	③ 退回 /home/newqqagent（本仓默认部署路径）
func upgradeDataDir(binPath string) string {
	if d := strings.TrimSpace(os.Getenv("HOMEAGENT_DATA_DIR")); d != "" {
		return d
	}
	return "/home/newqqagent"
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func main() {
	pkgPath := flag.String("pkg", "", "本地安装包（.tar.gz）；与 -release 二选一")
	release := flag.Bool("release", false, "从 GitHub 最新 Release 下载 OTA 包（默认仅探测；配合 -apply 执行）")
	binPath := flag.String("bin", "/usr/local/bin/homed", "当前运行的内核二进制路径")
	pluginsDir := flag.String("plugins", "/home/newqqagent/plugins", "外部插件目录（探测兼容性用）")
	apply := flag.Bool("apply", false, "真正执行升级（默认只探测+预检）")
	flag.Parse()

	if *pkgPath == "" && !*release {
		fmt.Fprintln(os.Stderr, "必须提供 -pkg <安装包.tar.gz> 或 -release")
		os.Exit(2)
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

	fmt.Printf("== 解包 %s ==\n", *pkgPath)
	root, err := update.ExtractPkg(*pkgPath, tmp)
	if err != nil {
		fail("解包: %v", err)
	}

	// ── 1. 当前版本 ──
	cur, curErr := update.ReadBinVersion(*binPath)
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
	verdict := update.CompareVersions(cur, nv)
	fmt.Printf("\n== 结论 ==\n%s\n", verdict)

	// ── 4. depgraph 必须与新二进制同批 ──
	newDG := update.FindFile(root, "depgraph.bin")
	if newDG == "" {
		fmt.Println("⚠️ 包内缺 depgraph.bin —— 内核启动会因依赖图不匹配拒绝启动")
	} else {
		fmt.Printf("depgraph.bin: 包内已带（%d 字节），将与 homed 同批替换\n", update.FileSize(newDG))
	}

	// ── 5. 插件兼容性预判（OTA 最容易出事的点）──
	incompat := update.ProbePluginCompat(*pluginsDir, nv.SDKVersion)
	if len(incompat) > 0 {
		fmt.Printf("\n== 插件兼容性警告（%d 个）==\n", len(incompat))
		fmt.Println("换内核后这些插件会握手失败并被**自动禁用**（写入禁用配置 + 原因）：")
		for _, p := range incompat {
			fmt.Printf("  · %-16s SDK %s（新内核要求主版本 %s）\n", p.Name, p.SDKVersion, p.WantMajor)
		}
		fmt.Println("它们在界面上会显示为「已禁用」并带原因，重编插件或手动启用即可恢复。")
	} else {
		fmt.Println("\n== 插件兼容性 ==\n现有插件与新内核 SDK 主版本一致，无需重编。")
	}

	if !*apply {
		fmt.Println("\n（探测模式：未改动任何文件。确认无误后加 -apply）")
		return
	}

	// ── 6. 真正升级（备份 → 替换 → 重启 → 后置验证 → 失败回滚）──
	if err := update.DoUpgrade(root, *binPath, newDG, *pluginsDir, upgradeDataDir(*binPath)); err != nil {
		fail("升级失败: %v", err)
	}
}
