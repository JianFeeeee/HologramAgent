package kstate_test

// 判据：`kernelboot`（装配引擎）不得依赖业务包。
//
// ## 为什么这条是本计划的总判据
//
// 拆分前 `KernelState` 与装配引擎同居 `internal/kernelboot`。数据结构要认识
// 各业务类型，于是 kernelboot 被迫依赖 19 个业务包：
//
//	kernelboot ──▶ agent/core ──▶ …
//	           ──▶ plugin
//	           ──▶ security
//
// 后果是所有底层包都无法反向引用 kernelboot，只能各自造通道：
//
//	core      → Agent 字段（每种 agent 构造点都要记得传）
//	plugin    → 12 个 SetXxx
//	sdk       → SDKConfig 传参
//	security  → 包级单例 Global() / RuntimeGlobal() / ConfigCodec()
//
// 这四条通道是同一个根因的四个表现。**只要 kernelboot 还依赖业务包，
// 任何"把共享数据收敛到统一挂载点"的努力都会退回去**——因为底层包读不到它。
//
// ## 判据的形态
//
// 1. **结构判据**：`go list -deps ./internal/kernelboot` 不得含业务包。
// 2. **`kstate` 叶子性**：`kstate` 不得 import `kernelboot` / `kernelmod` / `cmd`。
// 3. **挂载点行为**：`Mount` / `Get` / `MountForTest` 的语义。
//
// ★ 第 1 条在 T1（别名过渡期）会**红**，这是**故意的**：
// 别名文件要 import kstate，而 kstate 认识业务类型 ⇒ 依赖仍在。
// T2 删掉别名、把 54 处引用改为 kstate.State 后才会转绿。
//
// 也就是说这条判据现在就在保护 T2：如果 T2 做了一半（漏改几处又把别名
// 加回去），它会立刻红。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// TestCoreCanImportKstate 是**本计划的总判据**（2026-11-04）。
//
// ## 它守的到底是什么
//
// 不是"kernelboot 零业务依赖"——那不可能：`kstate.State` 必须认识
// `*plugin.Registry`、`*memory.GraphDB` 等类型，否则字段就只能是 any
// （而 any 会把"拼错/取错类型"从编译错误降级成运行期断言，违背本结构体
// 的设计原则：类型化字段让错误编译期暴露）。
//
// 真正的不变量是：**`internal/agent/core` 能 import `kstate`**。
//
// 因为 `core` 是环的一极：只要 `kstate` 反过来依赖 `core`，
// `core` 就永远读不到统一挂载点，只能自建通道：
//
//	core → Agent 字段（每种 agent 构造点都要记得传）
//	core → security.Global() 包级单例
//
// 实测这两条都真实发生过（skill 索引没进子 agent 提示词、16 处 Global()）。
//
// ## 判据怎么做
//
// 不能只检查 import 列表（`core` 目前确实没 import `kstate`）——
// 那样即使环存在也不会红。必须**真的让 core 引 kstate 并编译**。
func TestCoreCanImportKstate(t *testing.T) {
	probe := filepath.Join("..", "agent", "core", "zz_kstate_probe_test.go")
	src := `package core

// 临时探针：证明 core 能 import kstate（无 import 环）。
import _ "github.com/JianFeeeee/HologramAgent/internal/kstate"
`
	if err := os.WriteFile(probe, []byte(src), 0o644); err != nil {
		t.Fatalf("写探针失败: %v", err)
	}
	defer func() { _ = os.Remove(probe) }()

	out, err := exec.Command("go", "build", "github.com/JianFeeeee/HologramAgent/internal/agent/core").CombinedOutput()
	if err != nil {
		t.Fatalf("★ core 无法 import kstate —— 环又出现了：\n%s\n\n"+
			"原因通常是 kstate.State 里加了 *agentCore.Xxx 类型的字段。\n"+
			"正确做法：其他包把内容挂到 kstate（存最小接口或 any + 断言），\n"+
			"而不是让 kstate 反向认识 core。", out)
	}
}

// TestKstateDoesNotDependOnCore 是上面那条的**静态版**（无需起编译）。
//
// 两条都要有：静态版能立刻指出"谁把 core 拖进来的"，
// 编译版能抓到静态检查漏掉的间接环。
func TestKstateDoesNotDependOnCore(t *testing.T) {
	deps := goListDeps(t, "github.com/JianFeeeee/HologramAgent/internal/kstate")
	for _, dep := range deps {
		if dep == "github.com/JianFeeeee/HologramAgent/internal/agent/core" {
			t.Fatalf("★ kstate 依赖了 internal/agent/core ⇒ core 再也无法 import kstate\n" +
				"（具体是哪个字段见 internal/kstate/state.go 里 `agentCore.` 的引用）")
		}
	}
}

// TestKstateIsLeaf 钉住 kstate 不得依赖**上层**（装配器 / 模块 / 入口）。
//
// 允许它依赖业务**类型**包（plugin / memory / security …）——那是 State
// 的类型化字段需要的。不允许的是反向依赖"使用它的上层"。
func TestKstateIsLeaf(t *testing.T) {
	deps := goListDeps(t, "github.com/JianFeeeee/HologramAgent/internal/kstate")

	forbidden := map[string]bool{
		"github.com/JianFeeeee/HologramAgent/internal/kernelboot": true,
		"github.com/JianFeeeee/HologramAgent/internal/kernelmod":  true,
		"github.com/JianFeeeee/HologramAgent/cmd/homed":           true,
	}
	for _, dep := range deps {
		if forbidden[dep] {
			t.Fatalf("★ kstate 依赖了 %s —— 挂载点必须是叶子", dep)
		}
	}
}

// TestMountGetRoundTrip 钉住挂载点基本语义。
func TestMountGetRoundTrip(t *testing.T) {
	prev := kstate.Get()
	restore := kstate.MountForTest(&kstate.State{DataDir: "/tmp/rt"})
	defer restore()

	got := kstate.Get()
	if got == nil {
		t.Fatal("Mount 之后 Get 返回 nil")
	}
	if got.DataDir != "/tmp/rt" {
		t.Fatalf("DataDir = %q，期望 /tmp/rt", got.DataDir)
	}
	if prev == got {
		t.Fatal("MountForTest 没有替换挂载点")
	}
}

// TestGetNilWhenUnmounted 钉住"未装配返回 nil、不 panic"。
//
// 这是热路径的前提：单测与独立工具（memgc 等）不装挂载点，
// 调用方靠 `if ks := kstate.Get(); ks != nil` 零成本跳过。
func TestGetNilWhenUnmounted(t *testing.T) {
	restore := kstate.MountForTest(nil)
	defer restore()

	if got := kstate.Get(); got != nil {
		t.Fatalf("未装配时 Get 应返回 nil，实际 %v", got)
	}
}

// TestMountForTestRestores 钉住恢复语义（并行测试隔离的基础）。
//
// 变红条件：restore 把挂载点恢复成 nil 而不是"进入前的值" ⇒ 嵌套测试互相破坏。
func TestMountForTestRestores(t *testing.T) {
	outer := &kstate.State{DataDir: "/outer"}
	restoreOuter := kstate.MountForTest(outer)
	defer restoreOuter()

	inner := &kstate.State{DataDir: "/inner"}
	restoreInner := kstate.MountForTest(inner)
	if kstate.Get().DataDir != "/inner" {
		t.Fatal("内层 MountForTest 未生效")
	}
	restoreInner()

	if got := kstate.Get(); got == nil || got.DataDir != "/outer" {
		t.Fatalf("restore 后应回到外层挂载点 /outer，实际 %v", got)
	}
}

// TestMountIsConcurrencySafe 钉住挂载点的跨 goroutine 安全（-race 下有效）。
func TestMountIsConcurrencySafe(t *testing.T) {
	restore := kstate.MountForTest(&kstate.State{DataDir: "/base"})
	defer restore()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = kstate.Get()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			kstate.Mount(&kstate.State{DataDir: "/hot"})
		}
	}()
	wg.Wait()
}

// goListDeps 返回某包的完整依赖列表。
//
// ★ 必须传**完整导入路径**而不是 `./internal/xxx`：`go test` 的进程 cwd
// 是**包目录**（internal/kstate），相对路径会解析成
// internal/kstate/internal/kernelboot 这种不存在的路径。
func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("go list -deps %s 失败: %v\n%s", pkg, err, stderr)
	}
	return strings.Fields(string(out))
}
