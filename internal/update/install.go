package update

// 安装形态与升级能力**探测**。
//
// ## 为什么必须有这一层
//
// 同一次升级，在不同安装形态下的正确做法完全不同：
//
//	deb/rpm  → 文件归包管理器所有。直接替换会让 dpkg/rpm 数据库与实际文件
//	           不一致：下次 `apt upgrade` 会把改动覆盖回去，或者报
//	           "本地文件被修改"并拒绝升级。**必须交给包管理器**。
//	tar      → 没有包管理器，自己替换是正确做法。
//	container→ 容器文件系统是镜像的一部分，重启即还原。**升级必须重拉镜像**。
//
// 在这层存在之前，`Apply` 只有一条路（直接替换二进制），于是在 deb 形态下
// 它要么因权限失败、要么（以 root 运行时）悄悄把包管理器数据库搞脏。
//
// ## 为什么不是"猜路径"
//
// 判据全部来自**可观测的系统事实**（dpkg 的 info 目录、rpm 数据库、
// 容器标记文件），而不是"二进制在 /usr/bin 所以大概是 deb"这类推断。
// 推断错了的后果是选错升级通道 —— 而这类错误在生产上极难反查。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/events"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/systemd"
)

// DefaultServiceUnit 是内核的服务单元名。
//
// ★ 从 internal/systemd 取，**不在本包重复定义**：单元名曾经在两处各写一份，
// 改名时只改了一处（systemd.UnitName="hologramagent.service"），而
// cmd/homed-upgrade 仍硬编码 "homeagent" —— 升级成功的机器重启了错误的服务。
const DefaultServiceUnit = systemd.UnitName

// InstallKind 是本机的安装形态。
type InstallKind string

const (
	// KindDeb 表示由 dpkg 安装（.deb）。
	KindDeb InstallKind = "deb"
	// KindRPM 表示由 rpm 安装（.rpm）。
	KindRPM InstallKind = "rpm"
	// KindTar 表示解包安装（无包管理器记录）。
	KindTar InstallKind = "tar"
	// KindContainer 表示运行在容器里（升级必须重拉镜像）。
	KindContainer InstallKind = "container"
	// KindSource 表示从源码直接运行（dev 环境）。
	KindSource InstallKind = "source"
	// KindUnknown 表示探测不出来。
	//
	// ★ 它的语义是「不知道」，**不是**「没有包管理器所以大概是 tar」。
	//   调用方拿到 Unknown 必须拒绝自动升级，而不是退回一条猜的路径。
	KindUnknown InstallKind = "unknown"
)

// Variant 是发行包变体（决定包里有哪些文件）。
type Variant string

const (
	VariantFull      Variant = "full"      // homed + waiter + GUI + 模型 + ORT
	VariantServer    Variant = "server"    // homed + waiter + 模型 + ORT
	VariantClient    Variant = "client"    // waiter + GUI（**没有 homed**）
	VariantToolchain Variant = "toolchain" // homed + waiter + hmapdev + SDK + 模型
	VariantUnknown   Variant = "unknown"
)

// HasKernel 报告该变体是否包含内核二进制。
//
// client 包没有 homed ⇒ 它的「升级」不涉及内核，任何内核 OTA 对它都不适用。
func (v Variant) HasKernel() bool {
	return v != VariantClient && v != VariantUnknown
}

// Capabilities 是本机升级能力的快照。**所有升级决策的唯一输入**。
type Capabilities struct {
	Kind        InstallKind `json:"kind"`
	Variant     Variant     `json:"variant"`
	BinPath     string      `json:"bin_path"`
	DataDir     string      `json:"data_dir"`
	DepGraph    string      `json:"depgraph,omitempty"`
	PackageName string      `json:"package_name,omitempty"` // deb/rpm 的包名
	ServiceUnit string      `json:"service_unit,omitempty"`

	// CanWriteBin 报告当前进程能否**直接替换**二进制。
	//
	// ★ 这是"能不能自己动手"的唯一判据。为 false 时绝不能尝试替换：
	//   失败会在替换中途发生（例如 depgraph 已换、homed 没换），
	//   留下一个不一致的安装。
	CanWriteBin bool `json:"can_write_bin"`

	// Reason 说明为什么当前形态不能自替换（可直接呈现给用户）。
	Reason string `json:"reason,omitempty"`
}

// UpgradeChannel 是本机应当走的升级通道。
type UpgradeChannel string

const (
	// ChannelPackageManager 表示升级必须由包管理器执行（deb/rpm）。
	ChannelPackageManager UpgradeChannel = "package-manager"
	// ChannelSelfReplace 表示可以自替换（tar 且目录可写）。
	ChannelSelfReplace UpgradeChannel = "self-replace"
	// ChannelContainerImage 表示必须重拉镜像。
	ChannelContainerImage UpgradeChannel = "container-image"
	// ChannelUnsupported 表示无法自动升级（需人工介入）。
	ChannelUnsupported UpgradeChannel = "unsupported"
)

// Channel 决定本机该走哪条升级通道。
//
// ## 为什么 deb/rpm 一律走包管理器（即使用户有 root）
//
// 「能写文件」不等于「该写文件」。dpkg 记录了每个受管文件的校验和，
// 绕过它替换的后果：
//
//	· 下次 apt upgrade 覆盖回旧版（升级"消失"）
//	· 或 dpkg 检测到本地修改，**拒绝**后续升级（机器被卡住）
//	· 两种都必须人工介入才能恢复
func (c Capabilities) Channel() UpgradeChannel {
	switch c.Kind {
	case KindContainer:
		return ChannelContainerImage
	case KindDeb, KindRPM:
		return ChannelPackageManager
	case KindTar:
		if c.CanWriteBin {
			return ChannelSelfReplace
		}
		return ChannelUnsupported
	default:
		return ChannelUnsupported
	}
}

// UpgradeCommand 返回本机**应当执行**的升级命令（给人看的）。
//
// 空串表示“没有可建议的命令”（container/source/unknown）。
func (c Capabilities) UpgradeCommand(target string) string {
	switch c.Channel() {
	case ChannelPackageManager:
		pkg := c.PackageName
		if pkg == "" {
			pkg = "hologramagent"
		}
		if c.Kind == KindRPM {
			return "sudo dnf upgrade " + pkg
		}
		if target != "" {
			return "sudo apt install --only-upgrade " + pkg + "  # 或: sudo dpkg -i " + target
		}
		return "sudo apt install --only-upgrade " + pkg
	case ChannelSelfReplace:
		return "homed-upgrade -apply   # 本机安装形态允许自替换"
	case ChannelUnsupported:
		// ★ tar 但目录不可写（最常见的本机形态）也必须给命令。
		//
		// 此时 Channel() 返回 Unsupported（因为不能自替换），但**升级
		// 本身是可行的** —— 只需提权。返回空串会让用户看到“无法升级”
		// 却不知道下一步做什么，而实际上一行 sudo 就解决了。
		//
		// 判据：形态是 tar 且唯一障碍是权限。
		if c.Kind == KindTar && strings.Contains(c.Reason, "没有写权限") {
			return "sudo homed-upgrade -apply   # 需要提权：服务以非 root 运行，动不了 " + filepath.Dir(c.BinPath)
		}
		return ""
	default:
		return ""
	}
}

// Detect 探测本机安装形态与能力。
//
// binPath 为空时取当前进程的内核二进制路径。**这个兜底只适用于本进程
// 就是 homed 的情形**（kernelmod 启动路径）；外部工具（homed-upgrade）
// 必须自己解析并显式传入，否则会把“自己”当成内核。
//
// ★ 实测踩到：CLI 同目录没有 homed 时，resolveBinPath 曾兜底到
//
//	os.Executable() ⇒ “升级 homed”实际变成“把 CLI 覆盖成新包里的 homed”，
//	一个 1 字节的假包就能把工具本体换成一段 shell 脚本。
//	现在 resolveBinPath 找不到就返回空，而 Apply 对空 binPath 直接拒绝。
//
// 任何一步探测失败都不会 panic：返回已确定的字段 + Unknown/空值。
func Detect(binPath, dataDir string) Capabilities {
	if binPath == "" {
		binPath = KernelBinaryPath()
	}
	c := Capabilities{
		BinPath:     binPath,
		DataDir:     dataDir,
		ServiceUnit: DefaultServiceUnit,
		Kind:        KindUnknown,
		Variant:     VariantUnknown,
	}

	if dg := filepath.Join(filepath.Dir(binPath), "depgraph.bin"); FileExists(dg) {
		c.DepGraph = dg
	}

	// 容器优先判：容器里也可能装了 deb（基础镜像带来的），
	// 但那时替换文件毫无意义 —— 重启容器就回到镜像原样。
	if inContainer() {
		c.Kind = KindContainer
		c.Reason = "运行在容器内：镜像文件系统只读且重启即还原，升级应重拉镜像"
	} else {
		c.Kind, c.PackageName = detectPackageKind(binPath)
	}

	c.Variant = detectVariant(binPath)
	c.CanWriteBin, c.Reason = canReplaceBinary(c.Kind, binPath)
	return c
}

// inContainer 判断是否运行在容器里。
//
// 用容器运行时留下的标准标记（Docker 的 /.dockerenv、Podman/CRI-O 的
// /run/.containerenv）。**不用** /proc/1/cgroup 里的 "docker" 字样：
// systemd-nspawn、WSL2、以及某些嵌套 cgroup 环境会误判。
func inContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// detectPackageKind 判断二进制是否归包管理器所有，并返回包名。
//
// 判据是**文件属主记录**，不是路径：
//   - dpkg 把每个包的文件清单写在 /var/lib/dpkg/info/<pkg>.list
//   - rpm 有 rpm 命令可查
//
// 因此 deb 包装在 /opt 也能被认出来，tar 包解到 /usr/bin 也不会被误认。
func detectPackageKind(binPath string) (InstallKind, string) {
	if name := dpkgOwner(binPath); name != "" {
		return KindDeb, name
	}
	if name := rpmOwner(binPath); name != "" {
		return KindRPM, name
	}
	// 无包管理器记录：区分“源码编译产物”与“tar 解包安装”。
	//
	// ★ 只认 go build 的临时目录形态（/go-build<N>/），**不能用** "/tmp/"。
	//   那条太宽了：用户把 tar 包解到 /tmp 试装、或 CI 在 /tmp 里跑
	//   集成测，都会被误判成“源码运行”而拒绝升级。判据必须指向
	//   **构建行为本身**，而不是“文件放在哪”。
	if isGoBuildTempPath(binPath) {
		return KindSource, ""
	}
	return KindTar, ""
}

// isGoBuildTempPath 判断路径是否是 `go run`/`go test` 的临时产物。
//
// 形态：<tmpdir>/go-build<digits>/b001/exe/<name>
// 只匹配这个结构，不匹配“放在 tmp 下”。
func isGoBuildTempPath(p string) bool {
	i := strings.Index(p, "/go-build")
	if i < 0 {
		return false
	}
	// go-build 后面必须紧跟数字（go 的实际命名），否则可能是用户自建的目录。
	rest := p[i+len("/go-build"):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	return j > 0 && j < len(rest) && rest[j] == '/'
}

// dpkgOwner 在 dpkg 数据库里查 binPath 属于哪个包。
//
// 直接扫 info 目录里以本仓包名前缀开头的 .list，比 exec dpkg -S 更快
// 且不依赖 dpkg 可执行文件（容器里常被精简掉）。
func dpkgOwner(binPath string) string {
	abs, err := filepath.Abs(binPath)
	if err != nil {
		return ""
	}
	entries, err := os.ReadDir("/var/lib/dpkg/info")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".list") {
			continue
		}
		pkg := strings.TrimSuffix(name, ".list")
		// 只看本仓的包：避免为了查一个文件而读几百个包的清单
		if !strings.HasPrefix(pkg, "hologramagent") && !strings.HasPrefix(pkg, "homeagent") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/var/lib/dpkg/info", name))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == abs {
				return pkg
			}
		}
	}
	return ""
}

// rpmOwner 用 rpm 查文件属主（rpm 数据库格式不稳定，交给官方命令）。
func rpmOwner(binPath string) string {
	out, err := runQuiet("rpm", "-qf", binPath)
	if err != nil {
		return ""
	}
	out = strings.TrimSpace(out)
	// rpm 未命中时输出 "file ... is not owned by any package" 且退出码非 0，
	// 走到这里说明命中了。
	if out == "" || strings.Contains(out, "not owned") {
		return ""
	}
	if i := strings.IndexByte(out, '\n'); i > 0 {
		out = out[:i]
	}
	return strings.TrimSpace(out)
}

// detectVariant 从安装布局推断变体。
//
// 判据用**文件存在性**（包内容），不用包名：包名里的变体后缀
// 在迁移/改名后未必可靠，而"有没有 GUI / hmapdev / 模型"是事实。
func detectVariant(binPath string) Variant {
	const (
		modelDir = "/usr/lib/hologram/models"
		guiDir   = "/usr/lib/hologram-gui"
	)
	hasKernel := FileExists(binPath)
	hasModel := dirExists(modelDir)
	hasGUI := dirExists(guiDir)
	hasToolchain := FileExists("/usr/bin/hmapdev")

	switch {
	case !hasKernel:
		// 没有 homed：只有 client 包是这样
		if FileExists("/usr/bin/waiter") || hasGUI {
			return VariantClient
		}
		return VariantUnknown
	case hasToolchain && hasModel:
		return VariantToolchain
	case hasGUI && hasModel:
		return VariantFull
	case hasModel:
		return VariantServer
	case hasKernel:
		// 有内核但无模型：老包或最小安装
		return VariantServer
	default:
		return VariantUnknown
	}
}

// canReplaceBinary 判定能否直接替换二进制，并给出不可行的原因。
//
// 四个条件全部满足才允许：
//  1. 不是包管理器拥有的文件（否则会破坏数据库一致性）
//  2. 不在容器里
//  3. 不是从源码直接运行（那是 dev 环境，升级无意义）
//  4. 当前进程对**目标目录**有写权限（rename 需要目录可写，不是文件可写）
func canReplaceBinary(kind InstallKind, binPath string) (bool, string) {
	switch kind {
	case KindDeb:
		return false, "由 dpkg 安装：直接替换会让包数据库与实际文件不一致，必须用 apt/dpkg 升级"
	case KindRPM:
		return false, "由 rpm 安装：直接替换会让包数据库与实际文件不一致，必须用 dnf/rpm 升级"
	case KindContainer:
		return false, "运行在容器内：文件系统随容器销毁还原，必须重拉镜像"
	case KindSource:
		// ★ 必须显式拒绝：源码运行环境的“升级”是重新构建，不是换磁盘上的文件。
		//   旧版本的 Channel() 对 source 返回 unsupported，而 CanWriteBin 却因为
		//   目录可写而为 true —— 两个判据互相矛盾，调用方会拿到“能写但不能升”。
		return false, "从源码直接运行：升级方式是重新构建，而不是替换二进制"
	case KindUnknown:
		return false, "无法确定安装形态：拒绝自动替换（可能破坏包管理器状态）"
	}
	dir := filepath.Dir(binPath)
	if !dirWritable(dir) {
		return false, "当前用户对 " + dir + " 没有写权限（服务以非 root 用户运行），需提权或改用包管理器"
	}
	return true, ""
}

// dirWritable 报告能否在 dir 里创建/改名文件。
//
// ★ 必须测**目录**而不是文件：原子替换用的是 rename，它要求的是
//
//	目录写权限。文件本身可写但目录不可写时，rename 依然失败。
func dirWritable(dir string) bool {
	probe := filepath.Join(dir, ".ha-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// runQuiet 执行一个只读查询命令，返回 stdout（失败返回 error，由调用方忽略）。
//
// 单独抽出来是为了让探测代码不直接依赖 os/exec：探测必须**永不 panic、
// 永不阻塞**，而 exec 的失败模式（命令不存在、权限、超时）都该收敛在一处。
func runQuiet(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// SystemdActive 报告某个 systemd 单元是否正在运行。
//
// 用途只有一个：CLI 升级完成后要告诉用户“接下来会发生什么”——
// 服务在跑就说明必须走热重启通道；没跑就说明下次启动即生效。
//
// ★ 不用它做“要不要自替换”的判据：那由 Capabilities.CanWriteBin 决定，
//
//	两件事没有关系（服务没跑也照样可能没有写权限）。
func SystemdActive(unit string) bool {
	if unit == "" {
		return false
	}
	out, err := runQuiet("systemctl", "is-active", unit)
	return err == nil && strings.TrimSpace(out) == "active"
}

// DataDirFromConfigDB 只读地取 config.db 里的 core.daemon.data_dir。
//
// ## 为什么要单独一个入口（而不是让调用方自己开库）
//
// data_dir 就在 config.db 里，而 config.db 就在 data_dir 里 —— 这是个
// 循环依赖，所以“找到一个候选 config.db”永远是猜的。但这不是问题：
// **猜的是线索，不是结论**。读出来的值是内核自己写下的，不是我们的推测。
//
// ## ★ 为什么复用 internal/config 而不是自己 sql.Open
//
// 旧实现自己 `sql.Open("sqlite3", ...)` + `PRAGMA query_only=1`。问题是
// cmd/homed-upgrade 用 `CGO_ENABLED=0` 打包，而 go-sqlite3 在无 cgo 时是
// **stub**：连接能开、查询必然失败并返回 "requires cgo to work" ——
// 错误被吞掉、函数返回空串 ⇒ 发行包里“自动发现 dataDir”这条路径
// **从未真正工作过**，而且症状是“无法确定数据目录”，指着完全错误的方向。
//
// 复用 ConfigRegistry 的好处：同一套打开/读取语义、随仓库一起演进，
// 且它本来就处理“库不存在/表不存在”的情形（返回默认值）。
func DataDirFromConfigDB(dir string) string {
	p := filepath.Join(dir, "config.db")
	if !FileExists(p) {
		return ""
	}
	reg := config.NewConfigRegistry(p)
	defer reg.Close()
	return strings.TrimSpace(reg.GetString("core.daemon.data_dir", ""))
}

// ── 内核数据目录的**权威源** ─────────────────────────────────────
//
// ## 为什么要有这两个函数
//
// pending 标记、备份目录、坏二进制留档全落在 dataDir 下。而"内核实际用的是
// 哪个 dataDir"只有一个可靠来源：内核启动时的 `-data`（ks.DataDir）。
//
// 历史教训（三类都踩过）：
//
//	① 从 exe 路径推导 → /usr/bin/data，而生产是 /var/lib/hologram
//	   ⇒ 备份目录建在只读目录（EROFS），升级必挂
//	② 硬编码生产路径 → 目录/用户迁移后指向不存在的目录
//	   ⇒ 标记落到内核读不到的地方 ⇒ **升级失败静默不回滚**
//	③ 读 HOMEAGENT_DATA_DIR → 部署方可能不设，设了也未必与 -data 一致
//
// 所以：**不猜**。内核模块在启动时注入真值，其余调用方读它；
// 取不到就返回空串，由 Apply 的空值守卫拒绝升级。
//
// 「不知道」伪装成「知道」比没有守卫更危险：守卫看起来在守，
// 实际守了个永远非空的常量。
var (
	kernelDataDirMu sync.RWMutex
	kernelDataDir   string
)

// SetKernelDataDir 由内核适配层（internal/kernelmod/update）在启动时注入
// 真实的 `-data` 值。**不是给普通调用方用的**。
func SetKernelDataDir(dir string) {
	kernelDataDirMu.Lock()
	defer kernelDataDirMu.Unlock()
	kernelDataDir = dir
}

// KernelDataDir 返回内核实际使用的 dataDir（取不到时为空串）。
func KernelDataDir() string {
	kernelDataDirMu.RLock()
	defer kernelDataDirMu.RUnlock()
	return kernelDataDir
}

// ── 事件总线的**内核侧注入** ──────────────────────────────────

var (
	eventBusMu sync.RWMutex
	eventBus   *events.Bus
)

// SetEventBus 由内核适配层在启动时注入事件总线。
//
// ## ★ 为什么改成注入，而不是让调用方每次传进来
//
// `Apply` 需要发进度/结果事件。此前签名收 `*events.Bus`，于是**插件**必须
// 拿到总线才能触发升级 —— 而插件拿总线正是"安全监控可被绕过"的根源：
// 一旦把 *Bus 交给插件，它就能 bus.Publish 伪造任意事件。
//
// 改成内核注入后：
//
//	· 插件调用 Apply 不用再碰总线（传 nil 即可）
//	· 进度事件由**内核子系统**以内核身份发出（Source="update"），语义也正确
//	· 插件侧再没有理由持有 *events.Bus
func SetEventBus(b *events.Bus) {
	eventBusMu.Lock()
	defer eventBusMu.Unlock()
	eventBus = b
}

// eventBusOr 返回传入的总线；为空时退回内核注入的那个。
func eventBusOr(b *events.Bus) *events.Bus {
	if b != nil {
		return b
	}
	eventBusMu.RLock()
	defer eventBusMu.RUnlock()
	return eventBus
}
