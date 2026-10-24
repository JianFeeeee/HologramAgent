//go:build linux || darwin

package plugins

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// 真实外部插件（重编为 plugin.bin）经内核加载后的端到端冒烟（Part 6.3）。
//
// 与 internal/plugin/proc 的测试的区别：
// 那些用 testdata 假插件或临时编译的最小插件验证机制；
// 这里用 **example/ 里真实的 17 个插件产物**，验证「业务代码零改动 + 重编即可」
// 这一迁移承诺在完整内核装配下成立。
//
// 前置：插件需已用新版 hmapdev 重编（scripts/rebuild-plugins.sh）。
// 未重编时测试 skip 而非 fail——CI 上不强制要求先跑重编脚本。

// hostExecutableKind 按魔数判断产物是不是**本机**可执行文件，返回可读格式名。
//
// 为何需要：以前只按文件名找候选（plugin.bin_<os>_<arch> → plugin.bin），拿到
// darwin/windows 产物就直接 exec，报的是 "exec format error"——看起来像插件坏了，
// 实际上只是**开发环境里的产物平台不对**。本轮就踩了：跨平台示例构建把
// build/plugin.bin 覆盖成 Mach-O arm64，internal/plugins 6 个测试全红，排查半小时。
func hostExecutableKind(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "无法读取", false
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := f.Read(head)
	if n < 2 {
		return "文件过短", false
	}
	switch runtime.GOOS {
	case "linux":
		if n >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F' {
			return "ELF（本机）", true
		}
		if n >= 4 && head[0] == 0xcf && head[1] == 0xfa && head[2] == 0xed && head[3] == 0xfe {
			return "Mach-O 64 little-endian（macOS，不是本机）", false
		}
		if n >= 2 && head[0] == 'M' && head[1] == 'Z' {
			return "PE（Windows，不是本机）", false
		}
		return "未知格式（不是本机可执行文件）", false
	case "darwin":
		for _, m := range [][]byte{{0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
			if n >= 4 && bytes.Equal(head, m) {
				return "Mach-O（本机）", true
			}
		}
		return "不是本机可执行文件", false
	default:
		return "未知平台（默认放行）", true
	}
}

// realPluginDir 返回某个 example 插件的 linux 产物路径。
func realPluginBinary(t *testing.T, name string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "third_party", "homeagent-sdk", "example", name))
	if err != nil {
		t.Fatalf("解析插件目录: %v", err)
	}
	// bundle 模式产物带平台后缀，单平台模式不带
	candidates := []string{
		filepath.Join(root, "build", fmt.Sprintf("plugin.bin_%s_%s", runtime.GOOS, runtime.GOARCH)),
		filepath.Join(root, "build", "plugin.bin"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err != nil || st.IsDir() {
			continue
		}
		if kind, ok := hostExecutableKind(c); !ok {
			t.Skipf("插件 %s 的产物 %s 不是本机可执行格式（%s）；重建：cd %s && hmapdev build --target %s/%s --no-bundle",
				name, c, kind, root, runtime.GOOS, runtime.GOARCH)
		}
		return c
	}
	t.Skipf("插件 %s 未重编（先跑 scripts/rebuild-plugins.sh）", name)
	return ""
}

// installRealPlugin 把真实插件产物装进测试用 plugins 目录。
func installRealPlugin(t *testing.T, plgDir, name string) {
	t.Helper()
	src := realPluginBinary(t, name)

	dst := filepath.Join(plgDir, name)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("建插件目录: %v", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读产物 %s: %v", src, err)
	}
	binPath := filepath.Join(dst, "plugin.bin")
	if err := os.WriteFile(binPath, data, 0o755); err != nil {
		t.Fatalf("写产物: %v", err)
	}

	// manifest 刻意写 "plugin.so"：验证工具链/内核都已不看 entry 值。
	// 17 个存量插件的 plg.json 都是这个值，没人去改——这正是「零改动」的含义。
	manifest := fmt.Sprintf(`{"name":%q,"name_zh":%q,"name_en":%q,"version":"1.0.0","entry":"plugin.so"}`,
		name, name, name)
	if err := os.WriteFile(filepath.Join(dst, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写 manifest: %v", err)
	}
}

// 真实插件经内核加载 → 注册工具 → **实际调用工具**。
//
// 之前的测试只验证到"注册"，这里验证调用往返：
// 内核 ExecuteTool → RPC → 插件进程 handler → 结果回传。
func TestRealPlugin_ToolInvokeRoundTrip(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRealPlugin(t, plgDir, "weather")

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}

	// 确认经 proc 通道加载（而非被同名内置插件遮蔽）
	if env.pluginReg.Get("weather") == nil {
		t.Fatal("weather 未加载")
	}

	// weather 注册的工具名带插件名前缀（内核 SetToolRegistrar 加的）
	var toolName string
	for _, def := range env.stageHost.GetToolDefs() {
		if strings.Contains(def.Name, "weather") {
			toolName = def.Name
			break
		}
	}
	if toolName == "" {
		t.Fatal("weather 未注册任何工具")
	}
	t.Logf("调用工具 %s", toolName)

	// 真实调用：weather 会发 HTTP 请求到 wttr.in，网络不通时返回错误而非 panic。
	// 这里只断言"调用链路通"——RPC 往返成功、handler 被执行、结果或错误正常回传。
	res, err := env.stageHost.ExecuteTool(toolName, map[string]interface{}{"city": "Beijing"})
	if err != nil {
		// 网络错误是可接受的：链路通了才能拿到插件侧的错误
		if strings.Contains(err.Error(), "not found in any plugin") {
			t.Fatalf("工具未注册到 stageHost: %v", err)
		}
		t.Logf("工具返回错误（网络受限环境正常）: %v", err)
		return
	}
	if res == nil {
		t.Error("工具返回 nil 结果且无错误")
	}
	t.Logf("工具返回: %.120v", res)
}

// sanitizer 的改写型 stage 在真实内核装配下生效。
//
// 这是迁移最核心的性质：C ABI 副本模型下多插件并发时实测 35.8~36.8%
// lost update（§8.4），共享内存 + 字段级脏写入后应为 0。
func TestRealPlugin_StageRewriteTakesEffect(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRealPlugin(t, plgDir, "sanitizer")

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}
	if env.pluginReg.Get("sanitizer") == nil {
		t.Fatal("sanitizer 未加载")
	}

	// sanitizer 注册 after_toolcall 清洗 ANSI 转义序列
	dirty := "结果：\x1b[31m告警文本\x1b[0m 结束"
	sc := &pubsdk.StageContext{
		Phase: pubsdk.StageAfterToolcall,
		ToolResults: []pubsdk.ToolResult{
			{CallID: "c1", Name: "some_tool", Result: dirty},
		},
	}

	env.stageHost.RunStage(pubsdk.StageAfterToolcall, sc)

	got, _ := sc.ToolResults[0].Result.(string)
	if got == dirty {
		t.Errorf("sanitizer 的清洗未生效（结果未变）：%q", got)
	}
	if strings.Contains(got, "\x1b[") {
		t.Errorf("ANSI 序列未被清除：%q", got)
	}
	t.Logf("清洗前: %q\n清洗后: %q", dirty, got)
}

// 多插件共享同一块共享段，只读插件不覆盖改写插件的结果。
//
// 若每插件一块段，「内核 ctx → 段 → 插件改 → 回读 ctx」会退化成副本模型，
// 最后回读者覆盖前者，lost update 原样复现。
func TestRealPlugin_MultiPluginShareOneSegment(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	// sanitizer 改写 ToolResults，weather 只读（不注册 after_toolcall 的改写）
	installRealPlugin(t, plgDir, "sanitizer")
	installRealPlugin(t, plgDir, "weather")

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}

	dirty := "输出：\x1b[33m黄色\x1b[0m"
	sc := &pubsdk.StageContext{
		Phase: pubsdk.StageAfterToolcall,
		ToolResults: []pubsdk.ToolResult{
			{CallID: "c1", Name: "t", Result: dirty},
		},
	}

	env.stageHost.RunStage(pubsdk.StageAfterToolcall, sc)

	got, _ := sc.ToolResults[0].Result.(string)
	if strings.Contains(got, "\x1b[") {
		t.Errorf("并发下清洗结果被覆盖（lost update）：%q", got)
	}
}

// 崩溃隔离：kill 掉插件子进程，homed（测试进程）必须存活。
//
// 对比 C ABI：插件 panic 直接带崩整个 homed 进程（§1.2，现网已发生）。
func TestRealPlugin_CrashDoesNotKillKernel(t *testing.T) {
	env := setupIntegration(t)
	defer env.cleanup()

	plgDir := filepath.Join(env.tmpDir, "plugins")
	installRealPlugin(t, plgDir, "editdoc")

	if err := env.pluginReg.Load(plgDir); err != nil {
		t.Fatalf("加载插件: %v", err)
	}
	if env.pluginReg.Get("editdoc") == nil {
		t.Fatal("editdoc 未加载")
	}

	// 找插件子进程并 SIGKILL。
	//
	// 必须拿 plgDir 限定范围：旧实现用全系统 pgrep -f plugin.bin 后
	// 只比“路径含 editdoc”，于是在跑着生产实例的机器上，它会把
	// /data/homeagent/plugins/editdoc/plugin.bin 当成目标杀掉（实测 9 次，
	// 全部落在有人跑 go test 的时段）。更糟的是此时本测试仍会通过：
	// 它断言的是测试内核存活，而那个内核的插件压根没死——**它在测一件
	// 没发生的事**，同时还把生产环境打坏了。
	pid := findPluginPID(t, plgDir, "editdoc")
	if pid == 0 {
		t.Skip("未找到本测试自己拉起的插件子进程")
	}
	t.Logf("kill 插件进程 pid=%d (exe 在 %s 下)", pid, plgDir)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill: %v", err)
	}

	// 先确认目标进程真的死了。
	//
	// 这步不能省：旧版直接断言“内核存活”，而内核本来就活着——
	// 即使 SIGKILL 发错了对象（杀了生产实例的插件）测试也会结束。
	// 先验“目标真死”再验“内核未被连带”，两步都成立才能证明隔离生效。
	deadline := time.Now().Add(3 * time.Second)
	dead := false
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			dead = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !dead {
		t.Fatalf("pid=%d 在 SIGKILL 后 3s 内未退出，崩溃隔离无从验证", pid)
	}

	// 内核（本测试进程）必须存活并能继续工作
	if env.pluginReg.List() == nil {
		t.Fatal("内核在插件崩溃后不可用")
	}
	t.Logf("插件进程已确认退出，内核存活，已加载插件数=%d", len(env.pluginReg.List()))
}

// findPluginPID 在**指定插件目录下**找插件子进程 pid。
//
// root 参数是硬约束，不是可选过滤器：本函数的唯一用途是给崩溃隔离
// 测试提供一个“可以安全 SIGKILL 的 pid”，而安全的定义就是它必须属于
// 本测试自己的临时目录。不带这个约束就会误杀同机生产实例的插件。
//
// 匹配依据是 /proc/<pid>/exe 的真实路径必须以 root 为前缀。
// 用 exe 而不用 cmdline：cmdline 可被进程自行改写，而 exe 符链由内核维护。
// root 先过一道 EvalSymlinks：/tmp 在部分发行版上是符链（如 macOS 的
// /tmp -> /private/tmp），不归一化会让前缀比较永远不命中，退化成静默 Skip。
func findPluginPID(t *testing.T, root, name string) int {
	t.Helper()
	if root == "" {
		t.Fatal("findPluginPID: root 不得为空（防止误杀全系统同名插件）")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		realRoot = root
	}

	out, err := exec.Command("pgrep", "-f", "plugin.bin").Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Fields(string(out)) {
		pid := 0
		fmt.Sscanf(line, "%d", &pid)
		if pid == 0 {
			continue
		}
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil {
			continue
		}
		// 两道条件同时成立才算命中：在本测试的目录树内，且是目标插件
		if !strings.HasPrefix(exe, realRoot+string(os.PathSeparator)) {
			continue
		}
		if !strings.Contains(exe, name) {
			continue
		}
		return pid
	}
	return 0
}
