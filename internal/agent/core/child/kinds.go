package child

// kinds.go 记录**当前全部子 agent 类型**及其迁移归属。
//
// # 为什么要把这份清单写进代码而不是只写在 plan.md
//
// 用户 2026-11-04 指出："其他的哪几个子 agent 类型也要迁移"。
// 这个问题之所以要问，正是因为没有一份**唯一清单** ——
// 只能靠 grep `Spawn*` 反推，而反推会漏（工作式子叫 `runChildTask`，
// 前面没有 Spawn 前缀；转投驻留子是 `ensureOffloadResident`，
// 前面也没有 Spawn 前缀）。
//
// 所以清单必须在这里，且与 Kind 常量一一对应。新增类型时：
//  1. 在这里加一个 Kind + 一行说明
//  2. 实现 ChildKind
//  3. 装配期 Register
//  漏了第 2/3 步时 Register 不会报错但路由会走兜底 —— 由
//  TestAllDeclaredKindsAreRegistered 钉住。
//
// # 四种类型的实际形态（2026-11-04 实测）
//
//	类型        入口                    载体                独立IO  通道    子策略  上下文
//	主 agent    New()                  *Agent(完整)        有      有      -       相关性
//	驻留子      SpawnResident          完整 *Agent + Start  有      有      有      RelevanceContext
//	rtchild     SpawnRTChild           完整 *Agent 不 Start  有      有      有      跨轮 history
//	工作式子    runChildTask            goroutine + 局部msgs 无      无      无      局部 msgs
//	转投驻留子  ensureOffloadResident  = 驻留子（offloadOwned=true 标记）
//
// ★ 迁移范围（用户裁定"注意其他的哪几个子 agent 类型也要迁移"）：
//
//	必须迁（有独立生命周期与登记状态）：驻留子、rtchild、工作式子
//	不需要独立 Kind（是驻留子的一个变体）：转投驻留子
//
// 工作式子**不走骨架的 spawn**（它没有 io/通道/策略，也不需要登记进
// 通道归属表），但它的**执行循环**必须走共享的 runToolBatch ——
// 这是 ChildKind.RunTools 的职责，与"是否登记进 Registry"无关。

// 以下常量在 child.go 的 Kind 定义里已有 KindResident / KindRT，
// 这里只补尚未迁移的那几种的**规划标签**（迁移前不注册实现）。
const (
	// KindWorker 是一次性工作式子 agent（spawn_child / runChildTask）。
	//
	// 与其余三种的结构差异（实测）：
	//   · 无独立 IOManager（工具直接走父的 executeToolCall）
	//   · 无通道（不注册 inputch，父被显式剥掉 output_send__*）
	//   · 无子策略声明（不存在 declareChildPolicy 调用）
	//   · 上下文是**局部 msgs**，跑完即弃（父可按 taskID 取回结果，有界淘汰 20 条）
	//
	// ⇒ 它**不需要**进 Registry（没有通道归属要路由），但需要 ChildKind 的
	//   RunTools / BuildContext 两处语义。
	KindWorker Kind = "worker"

	// KindOffload 是 Kernel 为承接积压自动拉起的驻留子（offload.go）。
	//
	// ★ **它不是一种类型，是对驻留子的一种特殊调用。**
	//
	// 依据（offload.go:293 ensureOffloadResident）：
	//
	//	info, err := a.SpawnResident(ResidentOptions{
	//	    ID:             "offload-<unix>",
	//	    InputChs:       nil,              // 不接插件输入，只由父转投
	//	    AllowedOutputs: nil,              // 完整输出授权（要能回发到原通道）
	//	    TempPath:       a.residentTempPath(id),
	//	    OffloadOwned:   true,             // ← 唯一的“特殊”标在这里
	//	    TaskPrompt:     offloadTaskPrompt(),
	//	})
	//
	// 走的完全是驻留子那一条路（同样的 spawn / 同样的登记表 / 同样的 memory 装配），
	// 差异只有两处，且都已在 ResidentOptions 上表达：
	//
	//	① OffloadOwned 布尔标记 —— 只影响“能否被自动复用 / 是否计入上限”，
	//	   不影响任何生命周期语义（不能拿去复用人工建的子，也不占用它们的名额）
	//	② TaskPrompt 是分诊职责说明 —— 纯粹是提示词内容，不是机制
	//
	// ⇒ 因此**不应**为它新增 Kind。若新增，就得为它写一份 ChildKind，
	//   那份实现会与驻留子的实现逐字重复（除了一个 bool），
	//   而我们正要消灭“同一份逻辑写两遍、然后漂移”这个形态。
	//
	// 判据 TestOffloadIsNotItsOwnKind 钉住这条：DeclaredKinds() 里不得出现它。
)

// DeclaredKinds 返回**应当被注册**的全部类型标签（测试与装配自检用）。
//
// 不含 KindOffload（它是驻留子变体），不含主 agent（它不是子）。
func DeclaredKinds() []Kind {
	return []Kind{KindResident, KindRT, KindWorker}
}
