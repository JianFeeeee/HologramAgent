package packaging_test

// 发行打包脚本的**结构性判据**（2026-10-17）。
//
// ## 为什么必须有这组判据
//
// 这轮打包实测撞到 **4 个「包打出来了、装上去不能用」**的缺陷，
// 而且它们**全部**满足「构建日志全绿 + 脚本退出码 0」：
//
//  1. `package-linux.sh` **完全不拷 `depgraph.bin`**
//     ⇒ 内核启动校验失败：`生产构建（内嵌 hash 非空）但读不到依赖图`
//     ⇒ **退出码 1，内核根本起不来**（实测确认）。
//  2. `deploy/packaging/build.sh` 的 `build_homed` **不生成图、不注入 digest**
//     ⇒ 与 (1) 同因，另一条独立构建路径。
//  3. `build.sh` 的 `LDFLAGS` 用**历史模块路径 `gitcode.com/`**
//     ⇒ `-X` 静默失效（Go 对未知包路径不报错）⇒ 发行包 `--version`
//     全是 `unknown`（实测：`waiter 1.4.0 (commit unknown, built unknown)`）。
//  4. （Makefile 侧）`-ldflags` 用**单引号**包 `$$GRAPH_HASH`
//     ⇒ 注入字面量 `$GRAPH_HASH` ⇒ 同 (1)。已在
//     `internal/kernelboot/makefile_inject_test.go` 守。
//
// ## 判据形态：源码扫描（此处**恰当**）
//
// 断言的是"这些脚本里**存在**某个必需的处置"（拷贝 depgraph / 用当前模块
// 路径 / 拒绝缺资产），属于"模式存在性"，不是接线或顺序断言
// —— 后者用源码扫描会骗人（见 docs/zh/kernel-modularization-design.md §8.6）。
//
// 端到端验证（真的装 deb 再跑内核）由发版流程做；这里守的是"结构性缺失"
// 这一类，它比端到端更早、更便宜。

import (
	"os"
	"strings"
	"testing"
)

const (
	pkgLinux = "../../deploy/packaging/package-linux.sh"
	buildSh  = "../../deploy/packaging/build.sh"
	makefile = "../../Makefile"
)

func readOrSkip(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("读不到 %s: %v", p, err)
	}
	return string(b)
}

// ★★★ 判据：打包时必须把 `depgraph.bin` 放进与 homed **同目录**。
//
// 违反后果（实测）：内核启动即失败退出 1 ——
//
//	kernelboot: 生产构建（内嵌 hash 非空）但读不到依赖图 <dir>/depgraph.bin
func TestPackageLinuxStagesDepgraph(t *testing.T) {
	src := readOrSkip(t, pkgLinux)
	if !strings.Contains(src, "stage_depgraph") {
		t.Fatal("★ package-linux.sh 没有 stage_depgraph —— " +
			"打出的包缺依赖图 ⇒ 内核拒绝启动（退出码 1）")
	}
	// 必须真的**调用**（定义而不用等于没接）
	if strings.Count(src, "stage_depgraph \"$staging\"") == 0 {
		t.Fatal("★ stage_depgraph 定义但从未被调用 —— 图不会进包")
	}
	// 必须放到 usr/bin（与 homed 同目录），不能是别处
	if !strings.Contains(src, "usr/bin/depgraph.bin") {
		t.Fatal("★ depgraph.bin 必须与 homed 同在 usr/bin —— " +
			"内核按「可执行文件同目录」找图，放别处等于没有")
	}
}

// ★★★ 判据：`build.sh` 构建 homed 时必须**生成图并注入 digest**。
//
// 这与 Makefile 是**两条独立构建路径**：只修 Makefile 不够
// （实测：用 build.sh 打出的 homed 缺图，起不来）。
func TestPackagingBuildShGeneratesGraphForHomed(t *testing.T) {
	src := readOrSkip(t, buildSh)
	if !strings.Contains(src, "homed-depgraph") {
		t.Fatal("★ deploy/packaging/build.sh 不构建 homed-depgraph —— " +
			"它无法产出依赖图 ⇒ 该路径打出的 homed 起不来")
	}
	if !strings.Contains(src, "EmbeddedGraphHash=") {
		t.Fatal("★ build.sh 未注入 EmbeddedGraphHash —— " +
			"即使有图，digest 与二进制不匹配同样拒绝启动")
	}
	// 必须用 -digest 拿**真实**的 digest（不能写死/留空）
	if !strings.Contains(src, "-digest") {
		t.Fatal("★ build.sh 必须调用 homed-depgraph -digest 取真实 digest")
	}
}

// ★★★ 判据：`-X` 的包路径必须是**当前**模块路径（不能是历史 gitcode.com）。
//
// Go 对未知 `-X` 路径**静默忽略**（不报错）⇒ 发行包版本/commit 全 unknown。
func TestPackagingLdflagsUseCurrentModulePath(t *testing.T) {
	src := readOrSkip(t, buildSh)
	if strings.Contains(src, "gitcode.com/JianFeeeee/HomeAgent") {
		t.Fatal("★ build.sh 的 LDFLAGS 用的是**历史**模块路径 " +
			"`gitcode.com/JianFeeeee/HomeAgent`；当前模块是 " +
			"`github.com/JianFeeeee/HomeAgent`。\n" +
			"   `-X` 对未知路径**静默无效** ⇒ 发行包的 Version/Commit/BuildTime " +
			"全是默认值（实测 --version 输出 unknown）")
	}
	if !strings.Contains(src, "github.com/JianFeeeee/HomeAgent/internal/meta.Version") {
		t.Fatal("★ build.sh 必须用 github.com/... 注入 meta.Version")
	}
}

// ★★★ 判据：Makefile 注入依赖图 digest 时引号必须**允许展开**。
//
// （与 internal/kernelboot/makefile_inject_test.go 同源，这里从打包视角再守一遍：
//
//	打包依赖 Makefile 产出的二进制，所以这条对打包也关键。）
func TestMakefileInjectsExpandableGraphHash(t *testing.T) {
	src := readOrSkip(t, makefile)
	idx := strings.Index(src, "EmbeddedGraphHash=$$GRAPH_HASH")
	if idx < 0 {
		t.Skip("Makefile 的注入方式已变，请同步更新判据")
	}
	lineStart := strings.LastIndex(src[:idx], "\n") + 1
	line := src[lineStart:idx]
	ld := strings.Index(line, "-ldflags")
	if ld < 0 {
		return
	}
	if strings.Count(line[ld:], "'")%2 == 1 {
		t.Fatal("★ Makefile 的 -ldflags 用单引号包住 $$GRAPH_HASH ⇒ shell 不展开 " +
			"⇒ 注入字面量 $GRAPH_HASH ⇒ 内核拒绝启动")
	}
}

// ★★★ 判据：onnx 资产缺失时打包必须**拒绝**（不能出「默认启用但用不了」的假包）。
//
// 用户明确要求 onnx 的 so 与模型进包；官方脚本的态度是"缺资产就失败"。
// 这条守的是那个**拒绝**仍然存在。
func TestPackagingRefusesMissingONNXAssets(t *testing.T) {
	src := readOrSkip(t, pkgLinux)
	for _, kw := range []string{"ONNXRUNTIME_LIB", "CHINESECLIP_BUNDLE_DIR"} {
		if !strings.Contains(src, kw) {
			t.Errorf("★ package-linux.sh 不再检查 %s —— "+
				"onnx 资产缺失时会打出「默认启用但装完不能用」的假包", kw)
		}
	}
	// 必须**真的**拒绝：出现 ERROR 且 return 1
	if !strings.Contains(src, "拒绝打包") {
		t.Error("★ onnx 资产缺失时打包脚本必须明确拒绝（ERROR + 非零退出）")
	}
	// 许可证必须随二进制分发
	if !strings.Contains(src, "ThirdPartyNotices") {
		t.Error("★ onnxruntime 的 ThirdPartyNotices 必须随包分发（MIT 要求）")
	}
}

// ★★★ 判据：`toolchain` 变体的 control 文件必须存在且描述准确。
func TestToolchainVariantHasControlFile(t *testing.T) {
	src := readOrSkip(t, pkgLinux)
	if !strings.Contains(src, "toolchain") {
		t.Skip("toolchain 变体已移除，请同步更新判据")
	}
	// 变体循环里必须含 toolchain
	if !strings.Contains(src, "for variant in") || !strings.Contains(src, "toolchain; do") {
		t.Error("★ 变体循环必须包含 toolchain")
	}
	b, err := os.ReadFile("../../deploy/packaging/linux/deb/control-toolchain")
	if err != nil {
		t.Fatal("★ 缺 control-toolchain —— toolchain 变体打不出来")
	}
	c := string(b)
	for _, kw := range []string{"hologramagent-toolchain", "hmapdev", "SDK", "depgraph"} {
		if !strings.Contains(c, kw) {
			t.Errorf("control-toolchain 的描述里应提到 %s（让 apt show 能说清装了什么）", kw)
		}
	}
}

// ★★★ 判据：打包产物里的**安装路径与单元名**必须已随品牌改名（批 2）。
//
// 这条守的是"改一半就静默失效"那一类：包能打出来、退出码 0，但内容里
// 还写着旧路径 —— 装上去之后 systemd 找不到单元、数据目录对不上、
// ONNX 运行库指向空目录，而且没有任何一步报错。
//
// 只扫**产物布局相关**的旧串，不扫 module 路径（那是批 1 的地盘，
// 见 TestPackagingLdflagsUseCurrentModulePath）。
func TestPackagingUsesRenamedInstallPaths(t *testing.T) {
	src := readOrSkip(t, pkgLinux)

	// 安装路径 / 单元文件：一个都不许留旧值。
	for _, stale := range []string{
		"/var/lib/homeagent",
		"/usr/lib/homeagent",
		"/usr/share/doc/homeagent",
		"deploy/homeagent.service",
		"/etc/systemd/system/homeagent.service",
		"/usr/bin/homeagent-gui",
		"/usr/lib/homeagent-gui",
		`pkg_name="homeagent-`,
	} {
		if strings.Contains(src, stale) {
			t.Errorf("★ package-linux.sh 仍含旧安装路径/包名 %q —— "+
				"打出的包会装到旧位置（日志全绿，装完路径对不上）", stale)
		}
	}

	// 新值必须真的出现（否则上面的"无旧值"可能只是整段被删了）。
	for _, want := range []string{
		"/var/lib/hologram",
		"/usr/lib/hologram",
		"/usr/share/doc/hologramagent",
		"deploy/hologramagent.service",
		"/etc/systemd/system/hologramagent.service",
		"/usr/bin/hologram-gui",
		"/usr/lib/hologram-gui",
		`pkg_name="hologramagent-`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("★ package-linux.sh 缺少 %q —— 改名没落到位", want)
		}
	}
}

// ★★★ 判据：deb 的 postinst / prerm 必须操作**新**单元名。
//
// postinst 的 SERVICE_NAME 若还是旧名，装完 systemctl enable 找不到单元：
// 它带 `|| true`，所以一声不响地什么也没启用 —— 装完不会开机自启。
func TestDebMaintainerScriptsUseRenamedService(t *testing.T) {
	for _, p := range []string{
		"../../deploy/packaging/linux/deb/postinst",
		"../../deploy/packaging/linux/deb/prerm",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Skipf("读不到 %s: %v", p, err)
		}
		s := string(b)
		if !strings.Contains(s, `SERVICE_NAME="hologramagent"`) {
			t.Errorf("★ %s 的 SERVICE_NAME 不是 hologramagent —— "+
				"systemctl enable/stop 会作用在不存在的单元上（|| true 吞掉失败）", p)
		}
		if strings.Contains(s, `SERVICE_NAME="homeagent"`) {
			t.Errorf("★ %s 仍用旧 SERVICE_NAME=homeagent", p)
		}
		if strings.Contains(s, "/var/lib/homeagent") || strings.Contains(s, "/usr/lib/homeagent") {
			t.Errorf("★ %s 仍含旧安装路径", p)
		}
	}
}

// ★★★ 判据：四个 deb control 的包名与 SDK 安装路径必须已改。
//
// `dpkg-deb -f <pkg> Package` 的输出就是这里的 Package 字段；
// 旧名会让 apt 认为是另一个包（升级路径直接断成"装了第二个包"）。
func TestDebControlFilesUseRenamedPackage(t *testing.T) {
	for _, variant := range []string{"client", "server", "full", "toolchain"} {
		p := "../../deploy/packaging/linux/deb/control-" + variant
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读不到 %s: %v", p, err)
		}
		s := string(b)
		want := "Package: hologramagent-" + variant + "\n"
		if !strings.Contains(s, want) {
			t.Errorf("★ %s 缺少 %q", p, strings.TrimSpace(want))
		}
		if strings.Contains(s, "Package: homeagent-") {
			t.Errorf("★ %s 的 Package 仍是旧的 homeagent-*", p)
		}
		if strings.Contains(s, "/usr/lib/homeagent") {
			t.Errorf("★ %s 的 SDK 安装路径仍是 /usr/lib/homeagent", p)
		}
	}
	if b, err := os.ReadFile("../../deploy/packaging/linux/deb/control-toolchain"); err == nil {
		if !strings.Contains(string(b), "/usr/lib/hologram/sdk") {
			t.Error("★ control-toolchain 没把 SDK 装到 /usr/lib/hologram/sdk")
		}
	}
}

// ★★★ 判据：systemd 单元文件本身（deploy/hologramagent.service）。
//
// 文件名改了、内容没改是最典型的一半：ExecStart 仍写旧数据目录，
// 于是 StateDirectory / ReadWritePaths 与实际 --data 不一致，
// 内核起得来但写到只读路径上（或反之）—— 取决于 sandbox 设置，难查。
func TestSystemdUnitUsesRenamedPaths(t *testing.T) {
	b, err := os.ReadFile("../../deploy/hologramagent.service")
	if err != nil {
		t.Fatalf("★ 缺 deploy/hologramagent.service: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		"--data /var/lib/hologram",
		"WorkingDirectory=/var/lib/hologram",
		"StateDirectory=hologram",
		"ReadWritePaths=/var/lib/hologram",
		"ReadOnlyPaths=/etc/hologram",
		"ONNXRUNTIME_DIR=/usr/lib/hologram/onnxruntime",
		"LoadCredential=homed_security_key:/etc/hologram/security.key",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("★ hologramagent.service 缺 %q", want)
		}
	}
	// 注释里可以留旧路径（升级说明），但**指令行**不能有。
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "/var/lib/homeagent") || strings.Contains(trimmed, "/usr/lib/homeagent") {
			t.Errorf("★ hologramagent.service 指令行仍含旧路径: %s", trimmed)
		}
	}
	if _, err := os.Stat("../../deploy/homeagent.service"); err == nil {
		t.Error("★ deploy/homeagent.service 仍然存在 —— 旧单元文件没删，两个单元会打架")
	}
}
