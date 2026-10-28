// 内核升级闭环：探测 / 校验 / 备份 / 原子替换 / 重启 / 回滚。
// 包级说明见 doc.go（update 子系统）。
//
// 抽成独立包而不是留在 cmd/homed-upgrade：内核侧的 kernel_update 工具
// 与 CLI 必须走**同一套**逻辑。两份实现必然漂移，而这条路径上漂移的
// 后果是"CLI 能升级、agent 调用却把机器搞坏"。
//
// ## 范围：只面向内核
//
// 只处理 homed / depgraph.bin / waiter。**不碰插件** —— 插件有独立通道
// （plugin_update），且与内核的兼容性由"自动禁用不兼容插件"兜底。
package update

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// binVersion 是 homed --version 的输出。
type BinVersion struct {
	Version    string
	Commit     string
	BuildTime  string
	SDKVersion string
}

// readBinVersion 跑 `<bin> --version` 并解析。
//
// ★ 为什么用"跑它自己"而不是读文件里的字符串：
//
//	版本是 ldflags 注入的，从 ELF 里 grep 只能撞运气；而 --version 是内核
//	自己算出来的，还能一并拿到 commit（"同版本号不同 commit"很难察觉）。
func ReadBinVersion(bin string) (*BinVersion, error) {
	if _, err := os.Stat(bin); err != nil {
		return nil, err
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		// ★ 回退：老版本内核没有 --version（它是 2026-10-25 才加的）。
		//
		// 这正是 OTA 的**第一次**升级必然撞到的情况：新版工具要读线上版本，
		// 而线上那个版本还不支持被读。此时不能失败退出——否则所有人第一次
		// 升级都要人工介入。改为从运行中进程的环境/文件里找线索，找不到就
		// 诚实地说"未知"，让版本比较降级（其余预检照常）。
		if v := InferVersionFromRunning(); v != nil {
			return v, nil
		}
		return nil, fmt.Errorf("执行 --version: %v（老版本内核不支持 --version，且无法从运行进程推断）", err)
	}
	v := &BinVersion{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "version":
			v.Version = val
		case "commit":
			v.Commit = val
		case "build_time":
			v.BuildTime = val
		case "sdk":
			v.SDKVersion = val
		}
	}
	if v.Version == "" {
		return nil, fmt.Errorf("--version 输出里没有 version= 行: %q", strings.TrimSpace(string(out)))
	}
	return v, nil
}

// compareVersions 给出可读结论。返回文案而不是 bool：
// "能升但不能降"这类判断需要说明理由，用户才敢按 -apply。
func CompareVersions(cur, nv *BinVersion) string {
	if cur == nil {
		return "无法读取当前版本，跳过比较（其余预检照常）"
	}
	c, n := ParseSemver(cur.Version), ParseSemver(nv.Version)
	switch {
	case n == nil || c == nil:
		// 版本号形态不标准（如 1.4.0dev）：退化成字符串比较并说明
		if cur.Version == nv.Version && cur.Commit == nv.Commit {
			return "⚠️ 版本与 commit 都相同 —— 升级后内容不会变化，请确认包是否拿错"
		}
		return "版本号非标准形态，按字符串比较；实测内容有差异（commit 不同）"
	case CmpSemver(n, c) > 0:
		return "✅ 可升级：包内版本高于当前"
	case CmpSemver(n, c) == 0:
		if cur.Commit != nv.Commit {
			return fmt.Sprintf("⚠️ 同为 %s 但 commit 不同（%s → %s）—— 这是一次**同版本重发**，请确认是否有意为之",
				nv.Version, ShortCommit(cur.Commit), ShortCommit(nv.Commit))
		}
		return "⚠️ 与当前完全相同 —— 无需升级"
	default:
		return "❌ 这是**降级**（包内版本低于当前）。默认不建议；确认要回退请加 -allow-downgrade"
	}
}

// Semver / ParseSemver / CmpSemver 已收敛到 semver.go（子系统内唯一一份）。
// 合并前这里有一份私有实现，与 plugin 侧另写的一份行为不一致 —— 见 semver.go 头注释。

// pluginCompat 是一个与新内核 SDK 主版本不匹配的插件。
type PluginCompat struct {
	Name       string
	SDKVersion string
	WantMajor  string
}

// probePluginCompat 扫插件目录，读每个插件编译时的 SDK 版本。
//
// 数据来源：plugin.json 里**没有** sdk_version 字段，所以读不到就诚实地说
// "无法预判"，而不是瞎猜。能读到的（有 meta 文件的）才比较。
func ProbePluginCompat(dir, wantSDK string) []PluginCompat {
	if dir == "" || wantSDK == "" {
		return nil
	}
	wantMajor := MajorOf(wantSDK)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []PluginCompat
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pj := filepath.Join(dir, e.Name(), "plugin.json")
		b, err := os.ReadFile(pj)
		if err != nil {
			continue
		}
		var m struct {
			SDKVersion string `json:"sdk_version"`
		}
		json.Unmarshal(b, &m)
		if m.SDKVersion == "" {
			continue // plugin.json 不含该字段：无法预判，不虚报
		}
		if MajorOf(m.SDKVersion) != wantMajor {
			out = append(out, PluginCompat{Name: e.Name(), SDKVersion: m.SDKVersion, WantMajor: wantMajor})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func MajorOf(v string) string {
	if i := strings.IndexByte(v, '.'); i > 0 {
		return v[:i]
	}
	return v
}

func ShortCommit(c string) string {
	if c == "" || c == "unknown" {
		return "unknown"
	}
	if len(c) > 8 {
		return c[:8]
	}
	return c
}

// extractPkg 解包并返回包含 bin/ 的根目录。
func ExtractPkg(pkg, dst string) (string, error) {
	f, err := os.Open(pkg)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("非 gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		// 防路径穿越（包是被信任的，但这条成本几乎为零）
		clean := filepath.Clean(h.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return "", fmt.Errorf("包内有非法路径: %s", h.Name)
		}
		p := filepath.Join(dst, clean)
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(p, 0755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(p), 0755)
			out, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode))
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return "", err
			}
			out.Close()
		}
	}
	// 找含 bin/homed 的那层
	if FindFile(dst, "homed") == "" {
		return "", fmt.Errorf("包内无 homed")
	}
	return dst, nil
}

// findFile 在 root 下递归找文件名匹配的第一个。
func FindFile(root, name string) string {
	var found string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || found != "" {
			return nil
		}
		if info.Name() == name {
			found = p
		}
		return nil
	})
	return found
}

func FileSize(p string) int64 {
	if st, err := os.Stat(p); err == nil {
		return st.Size()
	}
	return 0
}

// DataDirEnv 是部署方声明数据目录的环境变量名。
const DataDirEnv = "HOMEAGENT_DATA_DIR"

// ResolveDataDir 推导数据目录（升级确认标记、备份、坏二进制留档的落点）。
//
// 三端（CLI / WebAPI / agent 工具）**必须**用同一份推导逻辑：pending 标记
// 落在哪，第 0 层 update 模块就从哪读。两边一旦漂移，升级启动失败时就
// **静默不回滚**，而症状（崩溃循环）与标记无关，极难反查。
//
// 推导顺序：
//
//	① HOMEAGENT_DATA_DIR（部署方显式声明，唯一可靠的来源）
//	② <exe 所在目录>/data 兜底（/usr/local/bin/homed → /usr/local/bin/data）
//
// ★ 兜底大概率**不是**内核 `--data` 的真实值，所以它只是"有个地方可写"的
// 最后手段，不是正确解 —— 部署方应显式设置 HOMEAGENT_DATA_DIR。
// 也**不**写死任何生产绝对路径（如 /home/newqqagent）：目录/用户迁移后它
// 指向一个不存在的目录，标记落错地方 ⇒ 静默不回滚（2026-10-26 迁移教训）。
func ResolveDataDir(exePath string) string {
	if d := strings.TrimSpace(os.Getenv(DataDirEnv)); d != "" {
		return d
	}
	if exePath == "" {
		exePath = KernelBinaryPath()
	}
	return filepath.Join(filepath.Dir(exePath), "data")
}

// Apply 执行真实升级：预检 → 备份 → 写 pending → 原子替换。
//
// ## ★ 本函数**不重启**内核
//
// 重启必须由**调用方**通过内核控制通道发起，理由见
// cmd/homed/kernel_control.go 的说明：worker 是被守护方，只有 guard
// 知道这次退出该不该重拉（exit 42 → 重拉且不计失败轮次）。
//
// 本包若自己调 `systemctl restart`，会带来三个问题：
//
//	· 杀掉整个 cgroup（含 guard）—— 把监督者一起杀掉
//	· 丢失 exit 42 语义：guard 的 failures/failbackRound 状态全丢
//	· 发命令的进程正是被重启杀掉的进程 ⇒ 返回值不可靠，
//	  一次成功的升级会被误判成失败而触发回滚
//
// 所以这里只承诺到"磁盘上的产物已就绪，pending 标记已写"。
// 后续重启 → 新内核启动 → update 模块（第 0 层）确认或回滚。
//
// 每一步失败都往回退：替换中途失败 ⇒ 立即用备份还原并清标记
// （此时尚未重启，回滚在本进程内完成，机器始终可启动）。
func Apply(root, binPath, newDG, dataDir string, bus *events.Bus) (*Outcome, error) {
	// ── ★ 最前置的拒绝条件：没有 dataDir 就没有回滚能力，一律不升级 ──
	//
	// pending 标记、备份目录、坏二进制留档全都落在 dataDir 下。空 dataDir
	// 意味着标记写不进去 ⇒ 新内核起不来时没有任何人知道该回滚。
	// 这种升级不是升级，是赌运气，赌输的代价是机器进崩溃循环。
	//
	// 所以在**动任何文件之前**直接失败，把问题交给调用方去修环境
	// （设置 HOMEAGENT_DATA_DIR），而不是悄悄产出一个"看起来成功、
	// 实际没有回滚能力"的升级。
	if dataDir == "" {
		return nil, fmt.Errorf("dataDir 为空，拒绝升级：无 pending 标记就无回滚能力")
	}

	newBin := FindFile(root, "homed")
	if newBin == "" {
		return nil, fmt.Errorf("包内无 homed")
	}

	// ── 预检：新二进制必须能自报版本（说明它是可执行的、架构对的）──
	nv, err := ReadBinVersion(newBin)
	if err != nil {
		return nil, fmt.Errorf("预检失败，新二进制不可执行: %w", err)
	}
	publishProgress(bus, "kernel", "", "verify", "新内核可执行，版本 "+nv.Version)
	fmt.Printf("\n== 预检通过 ==\n新内核 %s 可执行\n", nv.Version)

	// ── 备份：只备本次会动的文件（不整目录拷贝，那是几百 MB）──
	// ★ 备份必须落在 dataDir 下，不能是 /root：
	// 服务以 ProtectSystem=strict + ReadWritePaths=/var/lib/hologram 运行，
	// 非 root 身份对 /root 没有写权限 ⇒ MkdirAll 必 EACCES ⇒ 首次升级必挂。
	ts := NowStamp()
	backup := filepath.Join(dataDir, BackupDirPrefix+ts)
	if err := os.MkdirAll(backup, 0755); err != nil {
		return nil, fmt.Errorf("建备份目录: %w", err)
	}
	targets := []string{binPath}
	if dg := filepath.Join(filepath.Dir(binPath), "depgraph.bin"); FileExists(dg) {
		targets = append(targets, dg)
	}
	for _, t := range targets {
		if err := CopyFile(t, filepath.Join(backup, filepath.Base(t))); err != nil {
			return nil, fmt.Errorf("备份 %s: %w", t, err)
		}
	}
	publishProgress(bus, "kernel", "", "backup", fmt.Sprintf("已备份 %d 个文件", len(targets)))
	fmt.Printf("已备份 %d 个文件 → %s\n", len(targets), backup)

	// ── 写待确认升级标记（在替换**之前**）──
	//
	// 顺序很关键：先写标记再替换。若反过来（替换后才写），"替换成功但
	// 没来得及写标记就崩了"会丢标记 ⇒ 下次启动不知道该回滚。
	curVer := &BinVersion{}
	if v, verr := ReadBinVersion(binPath); verr == nil {
		curVer = v
	}
	if perr := MarkPendingUpgrade(dataDir, PendingRecord{
		FromVer:    curVer.Version,
		FromCommit: curVer.Commit,
		ToVer:      nv.Version,
		ToCommit:   nv.Commit,
		BackupDir:  backup,
	}); perr != nil {
		// 写不成功 ⇒ 放弃本次升级：没有标记就没有回滚能力，
		// 而不带回滚的升级等于赌运气。
		return nil, fmt.Errorf("写升级确认标记失败（放弃升级，未替换任何文件）: %w", perr)
	}
	fmt.Printf("已写升级确认标记 → %s\n", pendingPath(dataDir))

	// restore 在替换失败时把已动的文件还原（尚未重启，机器仍可启动）。
	restore := func(cause error) (*Outcome, error) {
		publishProgress(bus, "kernel", "", "rollback", cause.Error())
		fmt.Printf("\n❌ %v\n== 就地还原（尚未重启，机器仍可启动）==\n", cause)
		if cerr := ClearPendingUpgrade(dataDir); cerr != nil {
			fmt.Printf("  ⚠️ 清除升级确认标记失败: %v\n", cerr)
		}
		for _, t := range targets {
			src := filepath.Join(backup, filepath.Base(t))
			if err := CopyFile(src, t); err != nil {
				fmt.Printf("  ⚠️ 还原 %s 失败: %v（需人工处理）\n", t, err)
				continue
			}
			fmt.Printf("  已恢复 %s\n", t)
		}
		publishResult(bus, "kernel", "", false, curVer.Version, nv.Version, true, cause.Error())
		return nil, fmt.Errorf("替换失败已还原（未重启）: %w", cause)
	}

	// ── 替换（depgraph 必须与 homed 同批，顺序无关但都要成）──
	if newDG != "" {
		dst := filepath.Join(filepath.Dir(binPath), "depgraph.bin")
		if err := CopyFile(newDG, dst); err != nil {
			return restore(fmt.Errorf("替换 depgraph.bin: %w", err))
		}
		fmt.Println("已替换 depgraph.bin")
	}
	if err := CopyFile(newBin, binPath); err != nil {
		return restore(fmt.Errorf("替换 homed: %w", err))
	}
	fmt.Println("已替换 homed")
	publishProgress(bus, "kernel", "", "ready", "新内核已就位，等待重启")

	// ★ 成功路径也必须发 result 事件。
	//
	// 此前只有失败路径（restore）发 result，成功路径什么都不发 ⇒ 订阅方
	// 只能靠"进度停在最后一步且没看到 failed"来猜成功，猜错的方向是
	// "以为还在升级"。先发事件再返回：返回后调用方可能立刻重启，进程消失。
	publishResult(bus, "kernel", "", true, curVer.Version, nv.Version, false, "")

	// 备份必须 GC：否则每次升级都在 dataDir 下留几十 MB，旧注释写
	// "确认稳定后可删"，但全仓从来没有一行删除逻辑。
	PruneOldBackups(dataDir)

	return &Outcome{
		From:      curVer,
		To:        nv,
		BackupDir: backup,
	}, nil
}

// Outcome 是一次替换的结果（**不含重启**，重启由调用方走内核控制通道）。
type Outcome struct {
	From      *BinVersion // 升级前版本（回滚目标）
	To        *BinVersion // 已就位的新版本
	BackupDir string      // 备份目录（确认稳定后可删）
}

func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	tmp := dst + ".tmp-upgrade"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// 原子替换：先写临时文件再 rename，避免"写到一半进程在跑"
	return os.Rename(tmp, dst)
}

func FileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func NowStamp() string {
	return time.Now().Format("20060102-150405")
}

// inferVersionFromRunning 在老内核不支持 --version 时，尽力推断当前版本。
//
// 手段按可靠性排序，任一成功即返回：
//  1. 读 /proc/<pid>/exe 指向的文件，用 strings 找 "version=" 形态（不保证有）
//  2. 读 WebUI 的 /api/v1/agents（那里返回 meta.Version）—— 需服务在跑
//
// 都失败则返回 nil，调用方把比较降级为"未知"，**不阻断**升级。
func InferVersionFromRunning() *BinVersion {
	// 手段 1：从二进制里找注入的版本串。
	//
	// ldflags 注入的字符串在 rodata 里是裸的，形如 "1.4.0dev"；
	// 配合 "commit=" 一起找才有意义，所以这里只认成对出现的情况。
	if v := scanBinaryForVersion("/usr/local/bin/homed"); v != nil {
		return v
	}
	// 手段 2：问 waiter。
	//
	// ★ 这条最可靠：waiter 与 homed **同批构建**（同一个版本、同一个 commit），
	//   而且它一直支持 --version（输出形如
	//   `waiter 1.4.0 (commit 98850e1, built 2026-10-17T09:20:25Z)`）。
	//   老内核没有 --version，但同批的 waiter 有 —— 用它反推。
	if v := versionFromWaiter("/usr/local/bin/waiter"); v != nil {
		return v
	}
	return nil
}

// versionFromWaiter 解析 waiter 的版本行反推内核版本。
//
// 只认 "waiter X.Y.Z (commit ABC, built ...)" 这一种形态；
// 格式变了就返回 nil（宁可说"未知"，不可瞎猜）。
func versionFromWaiter(path string) *BinVersion {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(string(out))
	// 取第一个语义化版本号
	v := FirstSemverLike(line)
	if v == "" {
		return nil
	}
	commit := ""
	if i := strings.Index(line, "commit "); i >= 0 {
		rest := line[i+len("commit "):]
		if j := strings.IndexAny(rest, ",)"); j > 0 {
			commit = strings.TrimSpace(rest[:j])
		}
	}
	return &BinVersion{Version: v, Commit: commit}
}

// scanBinaryForVersion 在二进制里搜版本串（老内核的兜底手段）。
func scanBinaryForVersion(path string) *BinVersion {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	// 只读前 200MB：版本串在 rodata，通常在文件前 1/3
	buf := make([]byte, 200<<20)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]

	// 找 SDKCompatibleVersion 附近的三元组：本仓 meta 变量相邻存放，
	// 命中 "1.3.0" 且附近有语义化版本形态时，取最近的一个当内核版本。
	// ★ 这是**尽力而为**：可能撞到别的版本号，所以只作为"未知"的替代，
	//   且返回的 commit 恒为 unknown（无法可靠恢复）。
	idx := strings.Index(string(buf), "SDKCompatibleVersion")
	if idx < 0 {
		return nil
	}
	// 在附近 4KB 里找 X.Y.Z 形态
	window := string(buf[maxInt(0, idx-4096):minInt(len(buf), idx+4096)])
	if v := FirstSemverLike(window); v != "" {
		return &BinVersion{Version: v, Commit: "unknown"}
	}
	return nil
}

func FirstSemverLike(s string) string {
	for i := 0; i+5 <= len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		dots := 0
		for j < len(s) && ((s[j] >= '0' && s[j] <= '9') || s[j] == '.') {
			if s[j] == '.' {
				dots++
			}
			j++
		}
		if dots == 2 && j-i >= 5 {
			return s[i:j]
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
