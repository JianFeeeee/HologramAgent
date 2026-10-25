package core

// 中断向量表 —— 中断源的**统一登记处**。
//
// ## 为什么需要它（这是中断子系统独立出来的真正目的）
//
// 拆分之前，"有哪些内核中断、各自什么语义"散落在三个裸调用点上：
//
//	raiseKernelInterrupt("kernel",          "kernel", "任务 #N panic…")
//	raiseKernelInterrupt("kernel/overflow", "kernel", "上下文超页…")
//	raiseKernelInterrupt("child/"+rc.id,    "kernel", "驻留子已满…")
//
// 三者差异**只在 source 字符串**上，语义契约全靠注释与调用方自律。
// 最能说明问题的是 panic 的递归保护：
//
//	if t.Class == TaskInterrupt && t.Level >= LevelCritical { return }
//	// 否则同一个 panic 会自我放大成中断风暴
//
// 这条规则只属于"panic 中断"，却硬编码在 reportTaskPanic 里。
// **新增一种内核中断时，没人会记得要不要也加递归保护**——这就是要向量化的理由。
//
// ## 边界
//
// 向量表属于**进入中断**的入口定义，因此住在中断管理器这一侧
// （用户裁定：中断管理器负责"进入中断"）。它只回答"这次中断长什么样、
// 谁能产生它"，**不参与**排队、优先级比较、恢复选择。
//
// ## 加一种新的内核中断
//
// 只改这一个文件：在 VectorID 里加常量、在 interruptVectors 里加一条。
// 规则（级别/前置判定/载荷构造/来源标识）随条目声明，不再靠调用方自律。

import "fmt"

// VectorID 是中断源的**稳定标识**。
//
// 与 source 字符串的区别：source 是给人和日志看的（如 "child/xxx"），
// 会随实例变化；VectorID 是稳定的枚举值，可被诊断面、指标、日志字段引用，
// 也可用来回答"现在一共登记了几种内核中断"。
type VectorID string

const (
	// VectorTaskPanic：任务 panic。带递归保护（见 interruptVectors）。
	VectorTaskPanic VectorID = "task_panic"
	// VectorContextOverflow：上下文超页，需要提示模型"内容被移走了"。
	VectorContextOverflow VectorID = "ctx_overflow"
	// VectorChildContextFull：驻留子上下文已满，父据此决定压缩/回收/销毁。
	VectorChildContextFull VectorID = "child_ctxfull"
)

// vectorArgs 是一次中断向量调用的全部入参。
//
// 用显式结构体而不是 `any`：每种中断用哪些字段一眼可见，
// 且新增字段时编译器会提醒所有构造点。
type vectorArgs struct {
	// Task 是发生 panic 的任务（VectorTaskPanic 使用）。
	Task *Task
	// Panic 是 recover() 取到的值（VectorTaskPanic 使用）。
	Panic any
	// ChildID 是驻留子标识（VectorChildContextFull 使用）。
	ChildID string
}

// interruptVector 是一个中断源的**完整契约**。
type interruptVector struct {
	// ID 是稳定标识。
	ID VectorID
	// Level 是该源产生的中断级别。内核源恒为 L4（LevelCritical）。
	Level Level
	// Source 构造来源标识（写进 payload 的 interrupt_source）。
	// 需要动态前缀的源（如 child/<id>）在闭包里拼。
	Source func(a vectorArgs) string
	// Guard 是登记前的**前置判定**，返回 false 表示本次不产生中断。
	//
	// 这是原先散落在调用方里的规则的收容处——例如 panic 的递归保护：
	// 若 panic 的任务本身就是 L4 内核中断，就不能再产生新的 L4，
	// 否则同一个 panic 会自我放大成中断风暴。
	Guard func(a vectorArgs) bool
	// Build 构造中断正文（模型能看到的唯一内容）。
	Build func(a vectorArgs) string
}

// interruptVectors 是中断源的登记表。
//
// 判据（interrupt_vectors_test.go）：
//   - 表内 ID 必须与 interruptVector.ID 一致（防手滑）
//   - 每条必须有 Source/Guard/Build，且 Level 必须是内核级
//   - panic 条目必须能挡住"L4 panic 再产生 L4"（递归保护）
var interruptVectors = map[VectorID]interruptVector{
	VectorTaskPanic: {
		ID:     VectorTaskPanic,
		Level:  LevelCritical,
		Source: func(vectorArgs) string { return "kernel" },
		Guard: func(a vectorArgs) bool {
			// 递归保护是**结构性**的（原先硬编码在 reportTaskPanic 里）：
			// 若 panic 的任务本身就是 L4 内核中断，则不再产生新的 L4——
			// 否则同一个 panic 会自我放大成中断风暴，与"内核事件"的语义相反。
			if a.Task == nil {
				return false
			}
			return !(a.Task.Class == TaskInterrupt && a.Task.Level >= LevelCritical)
		},
		Build: func(a vectorArgs) string {
			return fmt.Sprintf("内核事件：任务 #%d 发生 panic：%v（该任务已被丢弃，调度器存活）",
				a.Task.ID, a.Panic)
		},
	},

	VectorContextOverflow: {
		ID:     VectorContextOverflow,
		Level:  LevelCritical,
		Source: func(vectorArgs) string { return "kernel/overflow" },
		Guard:  func(vectorArgs) bool { return true },
		// ★ 空中断：**不携带任何消息**（2026-10-25 按原始设计修正）。
		//
		// 原始设计里这个 L4 的用途是「**触发**语义相关性裁剪」——它本身
		// 只是一次调度信号，不是一条给模型的通知。此前的实现把它变成了
		// 「裁剪完事后发一条回执」（先在 raise 之前 pruneByQuery，再让
		// Build 生成「已裁剪 N 条…」的说明文本），顺序与语义都反了。
		//
		// 现在：Build 返回空串 ⇒ payload 里没有 content ⇒ 任务是空中断；
		// 裁剪由任务真正被调度执行时（rebalanceFrameContextPrefix）触发。
		//
		// 为什么必须带空串而不是省略 Build：lookupVector 的契约要求 Build
		// 存在（否则调用方 nil panic），"没有正文"要显式表达成 ""。
		Build: func(vectorArgs) string { return "" },
	},

	VectorChildContextFull: {
		ID:     VectorChildContextFull,
		Level:  LevelCritical,
		Source: func(a vectorArgs) string { return "child/" + a.ChildID },
		Guard:  func(vectorArgs) bool { return true },
		Build: func(a vectorArgs) string {
			// 父子协议：只推信号，细节靠"查看"拉状态面。
			return fmt.Sprintf("内核事件：驻留子 %s 上下文已满（contextfull）。"+
				"请【查看】其状态面后决定【压缩】/【回收】/【销毁】。", a.ChildID)
		},
	},
}

// lookupVector 取一个向量；未登记时返回 false（**不静默降级**）。
//
// 未登记必须响亮失败而不是悄悄用默认级别——否则新增中断源时打错 ID
// 会变成"中断发了但没人认领"，比不编译还难查。
func lookupVector(id VectorID) (interruptVector, bool) {
	v, ok := interruptVectors[id]
	return v, ok
}

// raiseVector 是登记一次内核中断的**统一入口**。
//
// 与直接调用 raiseKernelInterrupt 的区别：级别、来源标识、前置判定、
// 正文构造全部来自向量表，调用点只声明"哪一类中断 + 载荷"。
//
// 返回 true 表示中断真的占了 immediate 槽（会立即生效）。
func (a *Agent) raiseVector(id VectorID, args vectorArgs) bool {
	v, ok := lookupVector(id)
	if !ok {
		// 未登记是**编码错误**：宁可响亮失败，也不要静默发出一个没人认领的中断。
		panic(fmt.Sprintf("core: 未登记的中断向量 %q（请在 interrupt_vectors.go 里登记）", id))
	}
	if v.Guard != nil && !v.Guard(args) {
		return false
	}
	return a.raiseKernelInterruptAt(v.Source(args), "kernel", v.Build(args), v.Level)
}
