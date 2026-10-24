package all

// 「真实登记表 → 整机启动图」的判据（2026-10-24）。
//
// ## 迁移记录（ID:13，2026-10-24）
//
// 本文件原先挂在 `internal/kernelboot/modules/registry_test.go`，那个包
// **零生产调用**（死包：Register/BuildModules 是 kernelmod 那套的**第二份
// 实现**，外部调用点 = 0），只为了安放这组判据才留着。
//
// 现迁移到**真实聚合入口** `internal/kernelmod/all`：本包本来就是
// 「把全部模块 import 进来」的那一层，判据放在这里，用的就是生产路径
// 同一条 import 链（all → 各模块 init → kernelmod.Register）。
// 死包与其第二份注册表实现已删除。
//
// ★ 迁移没有削弱任何判据的能力：下面判据 5 的真相源（扫 `..` 源码树的
// 类型大小）与宿主包无关，换宿主前后都照样咬得住「登记了但漏 import」。
// 变异亲测见本文件「## 失败面」一节里标题后注「逐条实测过」的那一处
// （不是 TestEveryRegisteredModuleIsCompiledIn 上方注「已实测咬红」的那一处）
// 第 1–3 条。★ 按**该节的区分词**找，不按行号/位置找：本文件有**两处**失败面
// 小节，且章节位置会随重构漂移。
//
// ## 这个文件补的是什么（此前完全没有的覆盖）
//
// `internal/kernelboot` 自身的判据很密（图编解码、分层、调度、迁移回滚、
// 并发压测），但**全部用假模块**（`fakeModule`）在图上验算法。
// 也就是说：分层算法被验过，而「**真实的** 17 个模块按真实登记表装配成
// 一台整机、逐层起来、再严格逆序关掉」这件事**一条用例都没有**。
//
// 这中间藏着一整类失败：算法对、单模块对，但**装配起来**不对 ——
// 漏挂一个模块（依赖图缺设施）、某层的关闭函数没登记（漏关）、
// 关闭顺序反了（先关被依赖者，后关依赖者）。
//
// ## 为什么判据必须用真实登记表，而不能造一份"期望清单"
//
// 造假模块：任何"漏挂"都测不出来（假模块之间自洽）。
// 抄一份期望模块名清单：清单在「新增模块但忘了 import」时**也跟着漏**
// ⇒ 判据照绿（本仓已有先例：TestEveryModuleDirIsImported 的初版就栽在
// 手写清单上）。
//
// ⇒ 判据 1–4 用 `kernelmod.Registered()` 作为装配输入，断言的对象是
// **装配结果**（图 + 就绪状态 + 关闭顺序 + 资源回收），而不是某份清单。
//
// ## ★★ 真相源不能是要检测对象的入口（本文件最重要的一条设计约束）
//
// 判据 5（`TestEveryRegisteredModuleIsCompiledIn`）**不能**扫
// `kernelmod.Register(...)` 调用点作为真相源，因为"模块静默消失"有两种形态：
//
//	a. 四方法写全、init() 里调了 Register，只差 all/all.go 的聚合 import
//	   ⇒ Register 调用点仍在源码里 ⇒ 扫调用点**抓得住**
//	b. 四方法写全，但 init() 根本没写（或写了又被删）
//	   ⇒ Register 调用点不存在 ⇒ **要检测的入口自己消失了**
//	   ⇒ 源码侧与登记表侧同时少一个 ⇒ 集合比对照常相等 ⇒ **假绿**
//
// b 才是"静默消失"最危险的形态：编译通过、四方法齐全、只是没人注册。
// 把「被测对象」同时当「检测入口」，入口一被破坏，判据就**失去视野**——
// 这与本仓此前几次判据假绿同病（真相源与被测对象同源）。
//
// ⇒ 判据 5 的真相源换成与 Register 无关的**接口形状**：
//   「目录 + 类型名」复合键 + 「Name/Requires/Provides/Init 四方法齐全」。
//   模块哪怕不 Register、不被 import，四方法仍写在源码里 ⇒ 一定扫得到。
//
//   · 为什么必须带**目录**：本仓大量类型都叫 `Module`，只按类型名做键会撞成
//     一堆（实测撞完只剩 2 个）
//   · 为什么必须**四方法集**而不是"有个 Name()"：只有 Name 的类型到处都是，
//     四个方法齐才是 `kernelboot.Module` 的形状
//   · 为什么取 Name() 的**字符串字面量**：非字面量会让该模块从视野消失，
//     故显式报错**而不静默跳过**（跳过 = 又一次假绿）
//
// ★ 下次谁要改这条判据，先回答：**把它的真相源破坏掉，它还会绿吗？**
//   若会绿，说明真相源选在了被测对象身上，必须换（见上面 b）。
//
// ## 失败面（**逐条实测过**，2026-10-24；不是注释声称）
//
// 记法：⇒ 后面写「谁红 + 红在哪一句」。凡实测与预期不符的，照实写实际形状。
//
//  1. 删掉**有下游依赖**的模块的 `func init() { kernelmod.Register(...) }`
//     （实测对象：tracker）
//     ⇒ 判据 1/2/3/4 红，红在「真实登记表算不出启动图：依赖了不存在的设施
//        [tracker（agent 需要）…]、等待环 [agent plugin-host]」。
//     ★ 但**判据 5 也红** —— 见第 2 条，两条互为冗余。
//
//  2. 从 `internal/kernelmod/all/all.go` 去掉任一 `_ ".../kernelmod/xxx"` 行
//     ⇒ 实测同上（模块根本没被编译进来）。
//
//  3. ★ 删掉**无下游依赖的叶子模块**的 init()（实测对象：multimodal）
//     ⇒ **只有判据 5 红**，判据 1–4 全绿。
//     为什么 1–4 咬不住：没人依赖它 ⇒ 图能解、层顺序对、哨兵数对、无残留。
//     这正是判据 5 存在的唯一理由（初版判据在此**假绿**，见其函数注释）。
//
//  4. 把 Run 的层执行顺序改成逆序
//     ⇒ 判据 1/2/3/4 红，但**红在装配失败**（第 4 层模块拿到 nil 依赖：
//        「agent 依赖的设施 “memory” 对应的数据为 nil」），
//        不是「层间顺序断言」咬的 —— 装配先于断言就炸了。
//     ⇒ 把某一层整个跳过：**内核 panic**（llm.buildProviderManager 拿到
//        0x0 的 LuaVM，SIGSEGV）。仍是红的，但形态是崩溃不是可读断言。
//     ★ 以上是 **ID:14 之前**的形态（2026-10-24 实测记录，保留下来
//        作为“为什么需要执行期校验”的原始证据）。
//
//     ✅ **已完成（ID:14，提交 e70c2b5）**：kernelboot.Run 现在在**每层 Init
//        之前**先校“本层依赖 ⊆ 此前真跑完的层实际产出的设施”，缺就返回
//        *DependencyNotReadyError（指名模块名 + 缺哪些设施），**本层一个
//        Init 都不调**。用真实登记表实测，同一个变异变成可读报错：
//          · 逆序      ⇒ 第 **0** 层就拒（produced 为空）：
//                        「模块 agent 缺设施 [docmem knowledge llm …]；
//                         模块 plugin-host 缺设施 [config docmem …]」
//          · 跳过第 2 层 ⇒ 第 **2** 层拒：
//                        「模块 stage-host 缺设施 [knowledge llm textmem]」
//        **两种都不再是 panic / SIGSEGV**。
//
//        ⇒ 下次看到本条，请不要据此认为“Run 不拦 nil 依赖”；它现在拦。
//        另：该行为的专用回归判据在 internal/kernelmod/all/runtime_dep_test.go
//        （A 正序不误报 / B 交换两层 / C 删提供者整层）与
//        internal/kernelboot/runtimedep_test.go（纯逻辑四形状 + 防预填自检）——
//        那两组比在本节做整机变异快得多，改代码时优先跑它们。
//
//  5. 某层不再登记关闭函数（模块忘调 AddCloser / 层边界钩子丢了）
//     ⇒ 判据 3 红（哨兵数量不符）。与第 7 条同支路，实测由第 7 条覆盖。
//
//  6. 关闭改成与启动同序（正序）
//     ⇒ 判据 3 红：实测「有 5 层，但只观测到 0 个…」/「关闭顺序不是严格逆序」。
//
//  7. 关闭函数不真正执行（CloseAll 变 no-op，等价于「漏 Stop/Close」）
//     ⇒ 判据 3 红（哨兵 0 个）+ **判据 4 红**（「第 1 轮关闭后仍多 3 个
//        goroutine（基线 11，现 14，允许抖动 2）」）。两条各自独立咬住。
//
// ## 为什么这些断言不需要"扫描源码"或"反射读模块内部"
//
// 全部断言都落在**装配产物**上：分层计划（Layers）、syncbus 的就绪顺序、
// 关闭函数的实际执行顺序、进程级 goroutine 数与文件系统状态。
// 源码怎么组织都无所谓 —— 只认"装配起来之后发生了什么"。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	// ★ 判据与被测对象现在同属一个包（internal/kernelmod/all）：
	// 本文件就在聚合 import 的**那一层**里。因此不需要（也不能）再
	// 空白 import 自己 —— 只要本包被编译，all.go 的聚合 import 就已生效。
	// 这正是迁移的收益之一：判据用的是生产同一条 import 链，而非旁路。
)

// ─────────────────────────── 整机夹具 ───────────────────────────

// machine 是一台**真实装配起来**的内核（全部模块都是 kernelmod 里的真货）。
type machine struct {
	t   *testing.T
	ks  *kernelboot.KernelState
	bus *kernelboot.SyncBus
	// plan 是真实登记表算出的分层计划（启动图）。
	plan *kernelboot.Plan
	// res 是执行结果（层数 / 峰值并发 / 完成顺序）。
	res *kernelboot.RunResult
	// dataDir 是本机的数据目录（真实文件系统，不是内存桩）。
	dataDir string
	// layerClosers 记录"每层边界登记的关闭函数"被调用的层号顺序。
	// 这是"严格逆序关闭"的**观测点**：层边界处 AddCloser 一个哨兵，
	// 它必然排在该层所有模块的关闭函数**之后**（OnLayerDone 在层内 Init
	// 全部完成后才跑），所以哨兵的调用顺序 = 层的关闭顺序。
	layerClosers []int
	closed       bool
}

// layerSentinelFacilityPrefix 用于在断言消息里区分"哨兵"与"真模块"。
const shutdownProbeName = "boot-graph-test/layer-probe"

// bootMachine 用真实登记表装配一台整机。
//
// 参数 httpAddr 走 127.0.0.1:0（内核会把它传给 webui 插件）：
// 不占固定端口 ⇒ 并发跑 CI/本地多次不会 "bind: address already in use"
// （实测：写死默认 :8080 时本机起两次就红）。
func bootMachine(t *testing.T) *machine {
	t.Helper()

	dir := t.TempDir()
	// 只建**真实启动会建**的目录（与 cmd/homed 的 ensureDataDirs 对齐）。
	for _, d := range []string{
		"memory", "memory/text", "memory/documents", "knowledge",
		"adapters", "agentfs", "plugins",
	} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatalf("建数据目录: %v", err)
		}
	}

	reg := internalConfig.NewConfigRegistry(filepath.Join(dir, "config.db"))
	// ★ 注册表不在这里 t.Cleanup 关闭：本判据会在**同一个测试**里反复启停
	// 多轮，t.Cleanup 要到测试结束才跑 ⇒ 每轮都留一个 *sql.DB 的
	// connectionOpener goroutine 活着 ⇒ 泄漏判据会被自家夹具污染
	// （实测：3 轮后 goroutine 数 2→5，逐轮 +1，全部来自 database/sql）。
	// 关闭点放到 shutdown()（与生产的停机序列同处），并由 machine 保证只关一次。

	ks := kernelboot.NewKernelState()
	ks.CfgReg = reg
	ks.DataDir = dir
	ks.AgentWorkDir = filepath.Join(dir, "agentfs")
	ks.Cfg = reg.ToConfig()
	ks.Cfg.Daemon.DataDir = dir
	ks.Cfg.Plugin.Dir = filepath.Join(dir, "plugins")
	ks.HTTPAddr = "127.0.0.1:0"

	mods := kernelmod.Registered()
	if len(mods) == 0 {
		t.Fatal("★ 真实登记表是空的 —— `internal/kernelmod/all` 的聚合 import 断了")
	}
	if d := kernelmod.Duplicates(); len(d) > 0 {
		t.Fatalf("★ 模块名重复注册: %v", d)
	}

	plan, err := kernelboot.Build(mods)
	if err != nil {
		t.Fatalf("★ 真实登记表算不出启动图（漏挂模块 / 缺依赖 / 环）: %v", err)
	}

	bus := kernelboot.NewSyncBus()
	// 与生产 RunKernelModules 同序：先 MarkDeclared（让拼错校验有真相源），
	// 再 Run（Run 内部还会 MarkDeclared，幂等）。
	var declared []string
	for _, layer := range plan.Layers {
		for _, m := range layer {
			declared = append(declared, m.Provides()...)
		}
	}
	bus.MarkDeclared(declared)

	m := &machine{t: t, ks: ks, bus: bus, plan: plan, dataDir: dir}
	res, err := kernelboot.Run(plan, kernelboot.RunOptions{
		State:       ks,
		Bus:         bus,
		MaxParallel: 4, // 与生产一致（cmd/homed 的 RunKernelModules）
		OnLayerDone: func(idx int, _ []kernelboot.Module) {

			// ★ 每层一个关闭哨兵：它登记在**该层所有模块之后**，
			// 因此 CloseAll 的 LIFO 语义要求哨兵按层号**严格降序**被调用。
			layer := idx
			ks.AddCloser(func() { m.layerClosers = append(m.layerClosers, layer) })
		},
	})
	if err != nil {
		t.Fatalf("★ 整机装配失败: %v", err)
	}
	m.res = res

	// ★ 停机走**生产路径**（cmd/homed 的 waitForShutdown 顺序），
	// 而不是只调 CloseAll：漏关某层往往正是"生产路径少了一步"。
	t.Cleanup(func() {
		if !m.closed {
			m.shutdown()
		}
	})
	return m
}

// shutdown 执行与生产 `waitForShutdown` 一致的停机序列。
func (m *machine) shutdown() {
	m.closed = true
	if reg := m.ks.PluginRegistry; reg != nil {
		reg.StopAll()
	}
	if trk := m.ks.Tracker; trk != nil {
		if err := trk.Stop(); err != nil {
			m.t.Logf("tracker.Stop: %v", err)
		}
	}
	if m.ks.CfgReg != nil {
		_ = m.ks.CfgReg.Flush()
		// 生产里进程随即退出、句柄由内核回收；测试里必须显式关，
		// 否则 *sql.DB 的 connectionOpener 会一直活着（见 bootMachine 注释）。
		_ = m.ks.CfgReg.Close()
	}
	if sup := m.ks.Supervisor; sup != nil {
		sup.Shutdown()
	}
	if err := m.ks.CloseAll(); err != nil {
		m.t.Logf("CloseAll: %v", err)
	}
}

// moduleNames 返回计划里全部模块名（跨层）。
func (m *machine) moduleNames() []string {
	var out []string
	for _, layer := range m.plan.Layers {
		for _, mod := range layer {
			out = append(out, mod.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ─────────────────────── 判据 1：整机全部模块就位 ───────────────────────

// ★★★ 判据：**真实登记表里的每个模块都进了图，且其设施全部就绪**。
//
// 防的失败方式：「模块建了但没挂上」—— 新建 `internal/kernelmod/foo/mod.go`
// 写了类型和 `init()`，但忘了在 `all/all.go` 加聚合 import，或忘了写
// `init()` 里的 Register。后果不是编译错，而是该模块**静默不参与装配**：
// 它提供的设施永远不就绪，依赖它的插件**永不启动**（且无任何报错）。
//
// ★ 本判据的**能力边界（实测）**：只有「漏挂的模块被别人依赖」时才红。
//
//	漏掉的是**叶子模块**（没人 Requires 它，如 multimodal）时，
//	本判据连同判据 2/3/4 **全部照绿** —— 图照样能解、层照样对、
//	哨兵照样齐、也没东西可残留。
//	那种形态由判据 5（TestEveryRegisteredModuleIsCompiledIn）单独咬住，
//	它的真相源是源码里的类型与方法集，不依赖 Register 调用点。
//	⇒ 别把本判据当成「漏挂」的通用防线，两判据是互补关系，不是冗余。
func TestRealRegistryAssemblesWholeMachine(t *testing.T) {
	m := bootMachine(t)

	registered := map[string]bool{}
	for _, name := range kernelmod.Names() {
		registered[name] = true
	}
	inPlan := map[string]bool{}
	for _, name := range m.moduleNames() {
		if inPlan[name] {
			t.Fatalf("★ 模块 %s 在启动图里出现多次（分层去重失败）", name)
		}
		inPlan[name] = true
	}

	// ① 双向集合相等：登记了却没进图 / 进了图却不在登记表，都是缺陷。
	var missingInPlan, missingInRegistry []string
	for n := range registered {
		if !inPlan[n] {
			missingInPlan = append(missingInPlan, n)
		}
	}
	for n := range inPlan {
		if !registered[n] {
			missingInRegistry = append(missingInRegistry, n)
		}
	}
	sort.Strings(missingInPlan)
	sort.Strings(missingInRegistry)
	if len(missingInPlan) > 0 {
		t.Fatalf("★ 已登记但没进启动图的模块: %v —— 模块静默消失，"+
			"它提供的设施永不就绪（依赖它的插件会永不启动且无报错）", missingInPlan)
	}
	if len(missingInRegistry) > 0 {
		t.Fatalf("★ 在启动图里但不在登记表的模块: %v", missingInRegistry)
	}

	// ② 每个模块声明的设施都必须真的「已就绪」（不是"登记过"，是"就绪"）。
	//    ★ 这两个状态必须分开断言：MarkDeclared 会把全部 Provides 记成
	//    known（"登记过"），所以只查 Known 的话，一个模块哪怕从没跑过
	//    也"存在"——恰恰漏掉本判据要抓的失败。
	snap := m.bus.Snapshot()
	var neverReady []string
	for _, layer := range m.plan.Layers {
		for _, mod := range layer {
			for _, f := range mod.Provides() {
				if !snap[f] {
					neverReady = append(neverReady, mod.Name()+"→"+f)
				}
			}
		}
	}
	sort.Strings(neverReady)
	if len(neverReady) > 0 {
		t.Fatalf("★ 这些设施的提供者跑过了却没被标成就绪: %v\n"+
			"   后果：插件声明依赖它会**永远等不到**（静默不启动）", neverReady)
	}

	// ③ 全部层都执行完，且内核完成信号已发（未声明依赖的插件的启动信号）。
	if m.res.LayerCount != len(m.plan.Layers) {
		t.Fatalf("★ 只执行了 %d/%d 层", m.res.LayerCount, len(m.plan.Layers))
	}
	if !m.bus.KernelComplete() {
		t.Fatal("★ 全部层跑完却没有 MarkKernelComplete —— 未声明依赖的插件永不拉起")
	}

	// ④ 每个模块的 Init 都被调过：就绪顺序里应包含全部 Provides，
	//    且不得出现"从未被任何模块声明"的野设施。
	readySet := map[string]bool{}
	for _, f := range m.bus.ReadyInOrder() {
		readySet[f] = true
	}
	for _, layer := range m.plan.Layers {
		for _, mod := range layer {
			for _, f := range mod.Provides() {
				if !readySet[f] {
					t.Errorf("★ 设施 %s（%s）不在 ReadyInOrder 里", f, mod.Name())
				}
			}
		}
	}
	t.Logf("整机就绪：%d 层 / %d 模块，峰值层内并发 %d，就绪设施 %d 个",
		m.res.LayerCount, len(m.moduleNames()), m.res.MaxConcurrent, len(readySet))
}

// ─────────────────── 判据 2：层间顺序严格符合依赖拓扑 ───────────────────

// ★★★ 判据：**层间必须严格有序，层内两两无依赖**。
//
// 防的失败方式：
//   - 执行顺序被改（正序→逆序、或某层被提前/推迟）⇒ 消费者先于提供者
//   - 分层退化成"一层装完"（并行度归零，等价于回到串行 17 步）
//   - 有人把模块塞进错误的层（图被手改 / 层号漂移）
func TestLayerTopologyIsRespected(t *testing.T) {
	m := bootMachine(t)

	// ① 逐层检查：本层每个模块的 Requires 只能由**更早的层**提供。
	//    这就是 BFS 分层的定义（第 k 层的前驱全在 <k 层）。
	providedByLayer := map[string]int{} // 设施 -> 首次被提供的层号
	var violations []string
	for li, layer := range m.plan.Layers {
		// 先检查（用"更早层"的累积结果），再登记本层提供的设施。
		for _, mod := range layer {
			for _, req := range mod.Requires() {
				got, ok := providedByLayer[req]
				if !ok {
					violations = append(violations,
						mod.Name()+" 依赖 "+req+"，但它的提供者在**更晚**的层（或不存在）")
					continue
				}
				if got >= li {
					violations = append(violations,
						mod.Name()+"（第"+itoa(li)+"层）依赖 "+req+
							"（第"+itoa(got)+"层）—— 层间顺序必须严格")
				}
			}
		}
		for _, mod := range layer {
			for _, f := range mod.Provides() {
				if _, seen := providedByLayer[f]; !seen {
					providedByLayer[f] = li
				}
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("★ 层间顺序错了：\n  %s", strings.Join(violations, "\n  "))
	}

	// ② 层内两两无依赖（否则它们不该同层：其中一个必然在更晚的层）。
	//    这是"同层可并行的前提"，算法若坏了这条会先红。
	for li, layer := range m.plan.Layers {
		own := map[string]bool{}
		for _, mod := range layer {
			for _, f := range mod.Provides() {
				own[f] = true
			}
		}
		for _, mod := range layer {
			for _, req := range mod.Requires() {
				if own[req] {
					t.Fatalf("★ 第 %d 层内部存在依赖：%s 依赖同层的 %s —— "+
						"同层并行会读到未初始化的依赖", li, mod.Name(), req)
				}
			}
		}
	}

	// ③ 观测到的就绪顺序必须与分层一致：某设施被标记就绪时，
	//    它所在层的**全部前驱层**都已就绪。用 ReadyInOrder（真实写入顺序）
	//    而不是 Layers（计划），这样才能咬住"执行时乱序"。
	readyAt := map[string]int{}
	for i, f := range m.bus.ReadyInOrder() {
		readyAt[f] = i
	}
	for li, layer := range m.plan.Layers {
		for _, mod := range layer {
			for _, req := range mod.Requires() {
				reqLayer := providedByLayer[req]
				if reqLayer >= li {
					continue // 已在 ① 报过
				}
				// 前驱设施必须先于本模块任一设施进入就绪序列。
				for _, f := range mod.Provides() {
					if readyAt[req] > readyAt[f] {
						t.Fatalf("★ 就绪顺序反了：%s(%s) 先于其依赖 %s 就绪",
							f, mod.Name(), req)
					}
				}
			}
		}
	}

	// ④ 分层必须真的带来并行（否则"图"退化成一条串行链，等于没做）。
	//    ★ 不写死层数/模块数：真实登记表增删模块时这条仍然成立。
	if len(m.plan.Layers) >= len(m.moduleNames()) {
		t.Fatalf("★ 无并行度：%d 层 / %d 模块（等价于全串行）",
			len(m.plan.Layers), len(m.moduleNames()))
	}
	if m.res.MaxConcurrent < 2 {
		t.Fatalf("★ 峰值层内并发只有 %d —— 同层并行没生效", m.res.MaxConcurrent)
	}
}

// ─────────────────── 判据 3：关闭严格逆序 ───────────────────

// ★★★ 判据：**关闭顺序 = 启动顺序的严格逆序（按层）**。
//
// 防的失败方式：
//   - 某层漏关（模块忘 AddCloser / 层边界钩子被删）⇒ 哨兵少一个
//   - 关闭顺序改成与启动同序（正序）⇒ 先关被依赖者，依赖者随后用到已关资源
//   - CloseAll 的 LIFO 语义被改（例如排序后执行）
func TestShutdownIsStrictlyReverseLayerOrder(t *testing.T) {
	m := bootMachine(t)

	layers := len(m.plan.Layers)

	// 先停机再清点：哨兵是在 CloseAll 执行时才被调用的，装配刚完成时它
	// 只是"登记了"。
	m.shutdown()

	got := m.layerClosers
	if len(got) != layers {
		t.Fatalf("★ 有 %d 层，但只观测到 %d 个层边界关闭哨兵被调用（顺序 %v）—— "+
			"说明有层在关闭时被漏掉了（该层的模块资源不会释放）",
			layers, len(got), got)
	}
	want := make([]int, layers)
	for i := 0; i < layers; i++ {
		want[i] = layers - 1 - i // 严格降序 = 启动顺序的逆序
	}
	if !equalIntSlice(got, want) {
		t.Fatalf("★ 关闭顺序不是严格逆序：\n  实际（关闭顺序）= %v\n"+
			"  期望（启动的逆序）= %v\n"+
			"   正序关闭 ⇒ 先关被依赖者，依赖它的模块随后用到已关的资源", got, want)
	}

	// 关闭后 Closers 必须被消费干净（否则下次 CloseAll 会重跑已关资源）。
	if n := len(m.ks.Closers); n != 0 {
		t.Fatalf("★ CloseAll 之后仍残留 %d 个关闭函数（重复执行会二次释放）", n)
	}
	t.Logf("关闭顺序（层号）= %v —— 严格逆序 ✓", got)
}

// ─────────────────── 判据 4：关闭后无残留 ───────────────────

// goroutineSlack 是允许的 goroutine 抖动上限。
//
// ★ 为什么是 2 而不是 0：
// `testing` 自身在用例执行期间会有短暂 goroutine（如 -test.timeout 的
// 定时器、t.Cleanup 的调度），且 Go 运行时的 GC worker 会在
// runtime.GC() 后短暂存活。实测（本机 + CI 同构）关闭后
// 差值恒为 0，留 2 的余量只为吸收这种框架级抖动 —— 不是用来掩盖泄漏的
// （泄漏的量级是"每层一个轮询循环"，即 ≥5，远超 2）。
const goroutineSlack = 2

// ★★★ 判据：**关闭后进程回到基线：无 goroutine 泄漏、无资源残留**。
//
// 防的失败方式：
//   - 某模块的停止函数没跑（轮询/回调循环继续跑）⇒ goroutine 不回落
//   - 句柄没关（DB/文件/HTTP server）⇒ 临时目录删不掉
//   - 停机路径**越关越多**（关闭时又注册订阅/起协程）⇒ 反复启停会累积
//
// 做法：连续启停整机 3 轮，每轮都要求回落到基线，且差值不随轮次增长。
// 单轮回落可能是"碰巧"，多轮不增长才能排除"每轮漏一点"。
func TestShutdownLeavesNoResidue(t *testing.T) {
	baseline := settleGoroutines(t, 1<<30) // 先让运行时安静下来
	t.Logf("基线 goroutine 数 = %d", baseline)

	const rounds = 3
	prevAfter := baseline
	for round := 0; round < rounds; round++ {
		m := bootMachine(t)
		peak := runtime.NumGoroutine()
		subscribersAfterBoot := countBusSubscribers(t, m.bus)

		dir := m.dataDir
		m.shutdown()

		// ① goroutine 回落到基线（有界重试，不写死 sleep）。
		after := settleGoroutines(t, baseline+goroutineSlack)
		delta := after - baseline
		if delta > goroutineSlack {
			t.Fatalf("★ 第 %d 轮关闭后仍多 %d 个 goroutine（基线 %d，现 %d，允许抖动 %d）\n"+
				"   这通常意味着某层模块的停止函数没跑（轮询/回调循环还活着）\n%s",
				round+1, delta, baseline, after, goroutineSlack, goroutineStacks())
		}

		// ② 绝对水位不得逐轮抬升（排除"每轮漏一点"的慢泄漏）。
		//
		// ★ 比的是**绝对 goroutine 数**而不是相对第一轮基线的差值：
		//   测试框架自身（-test.timeout 定时器、t.Cleanup 调度、GC worker）
		//   会漂 1 个，用差值会在第 2 轮把框架抖动误报成泄漏。
		//   真泄漏的量级是"每层一个轮询循环"（≥5/轮），逐轮抬升必然可见。
		if round > 0 && after > prevAfter {
			t.Fatalf("★ 关闭后的 goroutine 水位在抬升：第 %d 轮 %d > 上一轮 %d —— "+
				"反复启停会累积泄漏\n%s", round+1, after, prevAfter, goroutineStacks())
		}
		prevAfter = after

		// ③ 临时目录必须删得掉：有未关闭的句柄/未卸载的 overlay 时会 EBUSY。
		//    这是"漏释放资源"最直接的外部证据（比看代码可靠）。
		if err := removeAllWithRetry(dir, 20); err != nil {
			var left []string
			_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
				if err == nil && fi != nil {
					rel, _ := filepath.Rel(dir, p)
					left = append(left, rel)
				}
				return nil
			})
			sort.Strings(left)
			t.Fatalf("★ 关闭后临时目录删不掉（有句柄/挂载未释放）: %v\n   残留项: %v",
				err, left)
		}

		// ④ syncbus 订阅者不得在关闭路径上**增长**。
		//
		// ★ 为什么断言"不增长"而不是"归零"：`SyncBus` 刻意**没有**
		// Unsubscribe（它是启动期一次性状态快照，随本次装配的 bus 一起
		// 被丢弃）。所以绝对归零需要给内核加 API —— 那是业务改动，
		// 不在本判据的权限内。真正的泄漏形态是"关闭时又挂上了订阅"
		// （每轮+1），这条能咬住；绝对归零作为已知缺口单独报出。
		subsAfterShutdown := countBusSubscribers(t, m.bus)
		if subsAfterShutdown > subscribersAfterBoot {
			t.Fatalf("★ 关闭路径**新增**了 syncbus 订阅者：装配后 %d → 关闭后 %d "+
				"—— 关闭时不该再挂回调（会持有已停止的对象）",
				subscribersAfterBoot, subsAfterShutdown)
		}

		t.Logf("第 %d 轮：基线 %d，装配峰值 %d，关闭后 %d（差 %d），"+
			"订阅者 %d→%d，临时目录已清理",
			round+1, baseline, peak, after, delta,
			subscribersAfterBoot, subsAfterShutdown)
	}
}

// ──────────── 判据 5：登记表必须覆盖源码树（独立真相源） ────────────

// ★★★ 判据：**源码树里每个「实现了 kernelboot.Module 四方法」的类型，
// 其 Name() 必须出现在登记表里**。
//
// ## 真相源为什么必须是「接口形状」而不是 Register 调用点（初版的教训）
//
// 「漏挂一个模块」有两种形态，初版只抓得住第二种：
//
//	a. 写好了 mod.go（类型 + init()），但 all/all.go 少一行聚合 import
//	   ⇒ Register 调用点**仍在源码里** ⇒ 扫得到 ⇒ 抓得住
//	b. 连 init() 都没写 / 写了又删了（其余三方法齐全）
//	   ⇒ Register 调用点不存在 ⇒ 初版扫描的入口本身消失
//	   ⇒ 源码侧与登记表侧**同时少一个** ⇒ 集合比对照常相等 ⇒ 假绿
//
// b 正是「模块静默消失」最危险的形态：四方法写全了、编译也过、
// 只是没人调 Register。把「要检测的对象」同时当「检测入口」，
// 入口一被破坏判据就失去视野 —— 与 ID:16/20/23 记的三次判据假绿同病：
// **真相源与被测对象同源**。
//
// ⇒ 换成与 Register 无关的真相源：**类型 + 方法集**。
// 模块哪怕不 Register、不被 import，Name/Requires/Provides/Init 四个方法
// 仍写在源码里（AST 按「目录+类型名」复合键收集，四方法齐全即视为模块，
// 取其 Name() 的字符串字面量）。
//
// ## 已排除的误报面（设计时逐个实测）
//
//	· 类型重名：本仓大量类型都叫 `Module`，**只按类型名做键会撞成一堆**
//	  （实测撞成只剩 2 个）⇒ 必须用 目录+类型名 复合键
//	· 指针接收者：agent 的方法是 `func (*Module) Name()`，
//	  AST 要同时处理 *ast.Ident 与 *ast.StarExpr
//	· 非字面量 Name()：**显式报错**，不静默跳过（否则该模块从视野消失）
//	· `_test.go` / `all/`（聚合 import 包）/ `testdata/`：不扫
//
// 实测基线：本仓该法恰扫出 17 个类型，与登记表 17 个名字一一对应（零误报）。
//
// 失败面（**已实测咬红**，不是注释声称）：
//
//	· 删任一模块的 init()（含无下游依赖的叶子模块 multimodal）⇒ 红
//	· 删 all/all.go 任一聚合 import ⇒ 红（该类型四方法仍在 ⇒ 源码侧多一个）
//	· 删掉整个模块目录 ⇒ 反向断言红（登记表里有、源码树里没有）
func TestEveryRegisteredModuleIsCompiledIn(t *testing.T) {
	const modRoot = ".."
	fromSource := scanModuleTypes(t, modRoot)
	if len(fromSource) == 0 {
		t.Fatal("★ 源码扫描一个模块都没找到 —— 扫描逻辑失效（判据会永远绿）")
	}

	registered := map[string]bool{}
	for _, n := range kernelmod.Names() {
		registered[n] = true
	}

	var missing []string
	for name, file := range fromSource {
		if !registered[name] {
			rel, _ := filepath.Rel(modRoot, file)
			missing = append(missing, name+"（"+rel+"）")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("★ 源码树里有 %d 个「四方法齐全的模块类型」**没被登记**（模块静默消失）:\n  %s\n"+
			"   修复：给该类型补 `func init() { kernelmod.Register(X{}) }`，"+
			"并在 internal/kernelmod/all/all.go 加聚合 import\n"+
			"   后果：它提供的设施永不就绪 ⇒ 依赖它的插件永不启动，且无任何报错",
			len(missing), strings.Join(missing, "\n  "))
	}

	// 反向：登记表里的每个模块名都应能在源码树里找到（防夹具污染生产表）。
	var phantom []string
	for name := range registered {
		if _, ok := fromSource[name]; !ok {
			phantom = append(phantom, name)
		}
	}
	sort.Strings(phantom)
	if len(phantom) > 0 {
		t.Fatalf("★ 登记表里有 %v，但源码树里找不到四方法齐全的类型 —— "+
			"登记项可能是测试夹具或残留", phantom)
	}
	t.Logf("源码树扫描到 %d 个模块类型，登记表 %d 个，双向覆盖 ✓",
		len(fromSource), len(registered))
}

// scanModuleTypes 按「目录+类型名」复合键收集模块类型（独立真相源）。
//
// 判定：Name/Requires/Provides/Init 四方法齐全 ⇒ 是模块类型；
// 取 Name() 的字符串字面量作为模块名。返回 模块名 → 定义文件路径。
func scanModuleTypes(t *testing.T, root string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()

	type key struct{ dir, typ string }
	methods := map[key]map[string]bool{}
	nameLit := map[key]string{}
	nameFile := map[key]string{}

	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi == nil {
			return nil
		}
		if fi.IsDir() {
			switch fi.Name() {
			case "all", "testdata":
				// all 是聚合 import 包（自身不定义模块类型）；testdata 不被编译。
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // 语法错误交给编译器报，这里不当判据失败
		}
		dir := filepath.Dir(path)

		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			var typ string
			switch rt := fn.Recv.List[0].Type.(type) {
			case *ast.Ident:
				typ = rt.Name
			case *ast.StarExpr:
				if id, ok := rt.X.(*ast.Ident); ok {
					typ = id.Name // 指针接收者（如 agent 的 *Module）
				}
			}
			if typ == "" {
				continue
			}
			k := key{dir, typ}
			if methods[k] == nil {
				methods[k] = map[string]bool{}
			}
			methods[k][fn.Name.Name] = true
			if fn.Name.Name == "Name" && fn.Body != nil {
				for _, st := range fn.Body.List {
					ret, ok := st.(*ast.ReturnStmt)
					if !ok || len(ret.Results) != 1 {
						continue
					}
					lit, ok := ret.Results[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						continue
					}
					nameLit[k] = v
					nameFile[k] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描源码树: %v", err)
	}

	out := map[string]string{}
	for k, ms := range methods {
		complete := true
		for _, want := range []string{"Name", "Requires", "Provides", "Init"} {
			if !ms[want] {
				complete = false
				break
			}
		}
		if !complete {
			continue
		}
		name, ok := nameLit[k]
		if !ok {
			// ★ 显式失败而不是跳过：非字面量 Name() 会让该模块从判据视野消失，
			// 那就又是一次静默漏测（初版判据就是这么假绿的）。
			t.Fatalf("★ %s 里类型 %s 四方法齐全但 Name() 不是字符串字面量 —— "+
				"本判据无法静态解析它。请把 Name() 改成 `return \"...\"`，"+
				"或在 scanModuleTypes 里补上解析（**不要**静默跳过）",
				nameFile[k], k.typ)
		}
		out[name] = nameFile[k]
	}
	return out
}

// ─────────────────────────── 辅助 ───────────────────────────

// settleGoroutines 让 goroutine 数回落到 <= target，返回最终观测值。
//
// 有界重试（不是写死 sleep）：每轮 GC + 短暂让出，最多 100 轮（约 2s）。
// 轮次上限的存在让"真泄漏"以超时后的真实数字报出来，而不是永远等下去。
func settleGoroutines(t *testing.T, target int) int {
	t.Helper()
	var n int
	for i := 0; i < 100; i++ {
		runtime.GC()
		n = runtime.NumGoroutine()
		if n <= target {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
	return n
}

// removeAllWithRetry 反复尝试删目录（有界）。
//
// 为何需要重试：关闭函数的先后顺序里，DB 的 Close 与文件句柄的释放
// 可能比内核的 dentry 回收稍晚；Linux 上偶发 EBUSY。重试上限保证
// "真删不掉"不会变成无限等待。
func removeAllWithRetry(dir string, tries int) error {
	var err error
	for i := 0; i < tries; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// countBusSubscribers 读 syncbus 上的订阅者数量。
//
// 只读反射（不 Set、不 Interface），用于把"关闭时挂订阅"这种泄漏
// 变成可断言的数字。字段名/类型变化时本函数会 fatalf —— 那是判据
// 失去观测点，必须显式修，不能静默变成 0。
func countBusSubscribers(t *testing.T, bus *kernelboot.SyncBus) int {
	t.Helper()
	if bus == nil {
		return 0
	}
	f := reflect.ValueOf(bus).Elem().FieldByName("onUpdate")
	if !f.IsValid() || f.Kind() != reflect.Slice {
		t.Fatalf("syncbus 的订阅者字段不再是 slice（判据失去观测点）")
	}
	return f.Len()
}

func equalIntSlice(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// goroutineStacks 在泄漏时抓全量栈（带进失败消息，便于定位是哪一层没关）。
func goroutineStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return "--- goroutine 栈 ---\n" + string(buf[:n])
}
