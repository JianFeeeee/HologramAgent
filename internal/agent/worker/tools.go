package worker

// tools.go —— 工作式子**没有自己的工具**（父侧与子侧的入口都由协议提供）。
//
// ## 为什么删掉了 spawn_child / child_result（用户反复裁定）
//
// 父子通信只有两条协议：
//
//	父 → 子   admin_child(type, action, meta, arg) + admin_child_help + admin_child_list
//	子 → 父   CallParent(level, kind, text, meta)
//
// 旧的两个工具是**第三、第四条**，而且它们绕开了内核骨架：
//
//	SpawnChild    起一个 goroutine，**不进 child.Registry、不进 InstanceManager**
//	              ⇒ 不持久化（重启丢）、admin_child_list 看不到、
//	        DeclareChildPolicy 没有它、StopAll 收不掉（主 agent 退出即孤儿）
//	child_result  从 worker 自己的 State 里查快照 —— 另一张私有结果表
//
// 现在：创建工作式子走 `admin_child(type="worker", action="create", arg=...)`
// （由 `Descriptor.Construct` 落地 → 进实例表 → 可持久化/可 StopAll/可列）。
// 结果上报走 `CallParent`；父要取结果走 `admin_child(type="worker", action="inspect")`。
//
// 判据：grep `spawn_child` / `child_result` = 0。
