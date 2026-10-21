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
			"`github.com/JianFeeeee/HologramAgent`。\n" +
			"   `-X` 对未知路径**静默无效** ⇒ 发行包的 Version/Commit/BuildTime " +
			"全是默认值（实测 --version 输出 unknown）")
	}
	if !strings.Contains(src, "github.com/JianFeeeee/HologramAgent/internal/meta.Version") {
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
	for _, kw := range []string{"homeagent-toolchain", "hmapdev", "SDK", "depgraph"} {
		if !strings.Contains(c, kw) {
			t.Errorf("control-toolchain 的描述里应提到 %s（让 apt show 能说清装了什么）", kw)
		}
	}
}
