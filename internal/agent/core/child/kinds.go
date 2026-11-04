package child

// kinds.go 记录**当前全部子 agent 类型**及其归属。
//
// # 为什么这份清单必须以文档形式留在代码里
//
// 用户曾问"其他的哪几个子 agent 类型也要迁移"。这个问题的存在本身说明
// 没有一份唯一清单 —— 只能靠 grep `Spawn*` 反推，而反推会漏：
//
//	工作式子    入口叫 runChildTask，前面没有 Spawn 前缀   ⇒ grep 会漏
//	转投驻留子  入口叫 ensureOffloadResident，同样没有前缀 ⇒ grep 会漏
//
// # 四种类型的实际形态（2026-11-04 实测）
//
//	类型        入口                    载体                独立IO  通道  登记表
//	主 agent    New()                  *Agent(完整)        有      有    -
//	驻留子      SpawnResident          完整 *Agent + Start  有      有    children(KindResident)
//	rtchild     SpawnRTChild           完整 *Agent 不 Start  有      有    children(KindRT)
//	工作式子    runChildTask            goroutine + 局部msgs 无      无    ✗（见下）
//	转投驻留子  ensureOffloadResident  = 驻留子            有      有    children(KindResident)
//
// # 为什么这里没有 KindWorker 常量
//
// 工作式子**不进** children 登记表，因此不需要类型标签：
//
//	· 它没有 io、没有通道、没有子策略 ⇒ 不需要"按通道归属路由"
//	· 它的 `childTasks` 是**已完成任务的幂等结果缓存**（child_result 要能
//	  重复查询 + 20 条有界淘汰），语义与"存活登记"完全不同
//	· 塞进 registry 会让 StopAll 去 Dispose 一个已经跑完的实例，
//	  并把结果缓存一起清掉 ⇒ child_result 立刻变成"不存在"，
//	  模型会无限重试（实测单轮 35 次工具调用、持续 514 秒）
//
// 曾定义过 `KindWorker` 常量，实测零使用，已删除（不留技术债）。
//
// # 为什么转投驻留子不是独立类型
//
// `ensureOffloadResident` 走的就是 `SpawnResident`，差异只有两处，
// 且都已在 ResidentOptions 上表达：
//
//	① OffloadOwned 布尔标记 —— 只影响"能否被自动复用 / 是否计入上限"
//	② TaskPrompt 是分诊职责说明 —— 纯粹是提示词内容，不是机制
//
// 若为它新增类型，那份实现会与驻留子逐字重复（除了一个 bool），
// 而我们在消灭的正是"同一份逻辑写两遍、然后漂移"这个形态。
