package plugin

// KERNEL-REGISTRY-SETLOCK（2026-10-24）：Registry Set* 注入路径的**写-写竞争**判据。
//
// ## 为什么必须有这条判据
//
// Registry 上那批 Set*（SetLuaVM / SetBaseAPIKey / SetStageHost / SetIndexer …）
// 长期是单行无锁直写：`func (r *Registry) SetLuaVM(vm *luaVM.VM) { r.lua = vm }`。
// 结构体里明明有 r.mu 这个 RWMutex，但这批 setter 一次都没取过锁。
//
// 平时看不出来，因为注入通常发生在启动早期。内核模块化之后就不成立了：
// **两个 goroutine 会同时注入同一批字段** ——
//
//	internal/kernelmod/agent/mod.go       wirePluginSDK  → SetLuaVM/SetBaseAPIKey/SetStageHost/…
//	internal/kernelmod/pluginhost/mod.go  injectBaseDeps → SetLuaVM/SetBaseAPIKey/SetStageHost/…
//
// 两者同属启动图第 4 层、并行执行，于是 r.lua / r.baseKey / r.stageHost / r.idx
// 被两个 goroutine 同时写。Go 内存模型下这是 data race；interface 字段尤其危险
// （type 与 data 两字不是同一轮写入 ⇒ 读到撕裂值，调用时直接崩）。
//
// ## 两条判据，一快一准
//
//  1. TestSettersConcurrentWriteRace —— **运行时**判据：并发 Set + 并发读，
//     -race 下未加锁必然报 DATA RACE。这是正面证据（真跑真竞争）。
//  2. TestRegistrySettersTakeWriteLock —— **源码级**判据：逐个 setter 检查
//     函数体内必须出现 r.mu.Lock()。race 是概率性的，这条把"有人把锁删了"
//     这类回退 100% 挡在门口（变异实验里它是确定性命中的那一条）。
//
// ★ 两条都必须"活的"：把任一 setter 的锁去掉（换回直写），两条都要报红。
// ★ 本文件只依赖 Registry 的公开 setter 与 r.mu 本身，不引用任何内部访问器 ——
// 因此在**未修复**与**已修复**两版 registry.go 上都能编译，
// 才可能拿它当"修前红 / 修后绿"的**同一条**判据。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"runtime"
	"sync"
	"testing"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/lua"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// setlockRounds 是每个注入 goroutine 的轮数。
//
// 取值依据：单轮 6 次写 × 2 个写 goroutine，未加锁代码上 -race 稳定命中
// （实测连续多次运行均报红，见交付证据）；再往上只是拖长测试。
const setlockRounds = 300

// TestSettersConcurrentWriteRace 钉住 setter 之间的写-写竞争。
func TestSettersConcurrentWriteRace(t *testing.T) {
	r := NewRegistry()

	// 两组 goroutine 用**不同的值**写同一批字段：
	// 写相同值容易被优化掉，不同值才能让 race detector 看到
	// "读到组合了两次写入的状态"。
	vmA, vmB := &lua.VM{}, &lua.VM{}
	shA := &raceToolSource{tag: "A"}
	shB := &raceToolSource{tag: "B"}
	idxA := &memory.Indexer{}
	idxB := &memory.Indexer{}
	// IOManager 只作为被写入/读回的句柄，判据不调其方法。
	iomA, iomB := &agentIO.IOManager{}, &agentIO.IOManager{}
	// ★ 目录只取一次：放在循环里 t.TempDir() 会造出上万个临时目录。
	const dirA, dirB = "dir-A", "dir-B"

	inject := func(vm *lua.VM, key, dir string, sh sdk.ToolSource, idx *memory.Indexer, iom *agentIO.IOManager) {
		for i := 0; i < setlockRounds; i++ {
			// 与 wirePluginSDK / injectBaseDeps 交叠的核心项
			r.SetLuaVM(vm)
			r.SetBaseAPIKey(key)
			r.SetStageHost(sh)
			r.SetIndexer(idx)
			// 同一批装配里相邻的另外两项
			r.SetIOManager(iom)
			r.SetPluginDir(dir)
		}
	}

	var writers sync.WaitGroup
	writers.Add(2)
	go func() { defer writers.Done(); inject(vmA, "key-A", dirA, shA, idxA, iomA) }()
	go func() { defer writers.Done(); inject(vmB, "key-B", dirB, shB, idxB, iomB) }()

	// 读侧并行：真实内核里 buildSDK 读这些字段与注入是交织的。
	// 读侧持 RLock（本次修复不动读侧）—— 同时验证"没有把并发读改成串行"。
	stop := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				vm, key, sh, idx := r.injectedSnapshot()
				// 必须真的用掉读到的值，否则编译器可能把读优化掉，
				// race detector 就看不到这次读。
				if vm == nil && key != "" && sh != nil && idx == nil {
					t.Error("setlock: 读到不可能的注入组合（读侧失去意义）")
					return
				}
			}
		}
	}()

	writers.Wait()
	close(stop)
	readers.Wait()
}

// injectedSnapshot 以 RLock 一次性读齐 4 个注入字段（模拟 buildSDK 读侧）。
func (r *Registry) injectedSnapshot() (*lua.VM, string, sdk.ToolSource, *memory.Indexer) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.lua, r.baseKey, r.stageHost, r.idx
}

// raceToolSource 是 sdk.ToolSource 的最小实现（判据里只需要个非 nil 值）。
type raceToolSource struct{ tag string }

func (s *raceToolSource) GetToolDefs() []sdk.ToolDef  { return nil }
func (s *raceToolSource) ToolDef(string) *sdk.ToolDef { return nil }
func (s *raceToolSource) ExecuteTool(string, map[string]interface{}) (interface{}, error) {
	return s.tag, nil
}

// registrySetterNames 是需要持写锁的 setter 清单（与 registry.go 对应）。
// 新增 Set* 时必须同步登记 —— 源码判据按这份清单咬。
var registrySetterNames = []string{
	"SetIOManager", "SetEventBus", "SetMemory", "SetTextMemory",
	"SetDocStore", "SetKnowledge", "SetProviderManager", "SetConfigRegistry",
	"SetPluginDir", "SetDataDir", "SetLuaVM", "SetBaseAPIKey",
	"SetKernelControl", "SetToolRegistrar", "SetStageRegistrar", "SetAPIRegistrar",
	"SetToolCleaner", "SetStatusProvider", "SetSupervisor", "SetTracker",
	"SetConfig", "SetStageHost", "SetDeviceAuthQuery", "SetIndexer",
}

// TestRegistrySettersTakeWriteLock 是源码级判据：每个 Registry.Set* 函数体里
// 必须出现 r.mu.Lock()。
//
// 为什么需要它（与运行时判据并存）：
//
//	race detector 依赖"这一次运行恰好交错到了"，是概率性的。
//	而"某人把锁删了"这类回退必须 100% 被挡在 CI 门口 —— 源码判据确定性命中。
func TestRegistrySettersTakeWriteLock(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	src, err := os.ReadFile(thisFile[:len(thisFile)-len("registry_setlock_test.go")] + "registry.go")
	if err != nil {
		t.Fatalf("读 registry.go 失败: %v", err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "registry.go", src, 0)
	if err != nil {
		t.Fatalf("解析 registry.go 失败: %v", err)
	}

	locked := map[string]bool{}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || fd.Name == nil || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if len(name) < 4 || name[:3] != "Set" {
			continue
		}
		// 函数体内是否出现 r.mu.Lock()。
		//
		// ★ 形状注意：`r.mu.Lock()` 的 CallExpr.Fun 是
		//   SelectorExpr{ Sel: "Lock", X: SelectorExpr{ Sel: "mu", X: Ident{"r"} } }
		// 不是 X 直接为 Ident —— 按 x.Name == "r" 判会漏掉**全部**加锁的 setter，
		// 判据变成"恒报红"，等于没判（本判据第一版就踩了这个坑）。
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Lock" {
				return true
			}
			mu, ok := sel.X.(*ast.SelectorExpr)
			if !ok || mu.Sel == nil || mu.Sel.Name != "mu" {
				return true
			}
			if id, ok := mu.X.(*ast.Ident); ok && id.Name == "r" {
				found = true
			}
			return true
		})
		if found {
			locked[name] = true
		}
	}

	for _, want := range registrySetterNames {
		if !locked[want] {
			t.Errorf("Registry.%s 未取写锁（r.mu.Lock）—— "+
				"这会重新引入启动图第 4 层并行注入的 DATA RACE（KERNEL-REGISTRY-SETLOCK）。"+
				"修法：函数体开头 r.mu.Lock(); defer r.mu.Unlock()", want)
		}
	}
}

// TestBuildSDKConcurrentWithSetters 钉住**真实读路径**与 Set* 的读写竞争。
//
// 为什么需要它（前两条判据覆盖不到的地方）：
//
//	TestSettersConcurrentWriteRace 的读侧用的是本文件自己写的 injectedSnapshot
//	——它**自带 RLock**，所以只证明了"setter 之间不再互撞"，
//	完全测不到 registry.go 里 buildSDK / snapshotDeps 那条真实读路径。
//	把 snapshotDeps 的 RLock 删掉，前两条判据仍会全绿（实测过）——
//	那就是判据缺口：read 侧的回退没人管。
//
// 本判据走 buildSDK（插件加载时真实调用的函数），setter 并发写同一批字段：
// 读侧若失去 RLock，-race 必报，且报告栈会直接指向 registry.go 的读点。
func TestBuildSDKConcurrentWithSetters(t *testing.T) {
	r := NewRegistry()
	r.SetPluginDir(t.TempDir())
	r.SetDataDir(t.TempDir())

	const rounds = 200
	var wg sync.WaitGroup

	// 写侧：模拟 agent 与 pluginhost 两个模块并行注入。
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			r.SetLuaVM(nil)
			r.SetConfig(nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			r.SetStageHost(nil)
			r.SetIndexer(nil)
		}
	}()

	// 读侧：走真实路径 buildSDK（内部 snapshotDeps 取依赖快照）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = r.buildSDK("setlock-probe")
		}
	}()

	wg.Wait()
}
