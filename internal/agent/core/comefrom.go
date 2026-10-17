package core

// 工具调用帧的**来源标识**（`comefrom`）：让插件能精确判定
// "这次工具调用是谁发起的"，而不是只看"输入来自哪个通道"。
//
// ## 为什么必须加这个（真实故障，2026-10-16 现场实证）
//
// 现象：驻留子 `dev-br-jianf-NextAgent-fix_abs-path` 自报 running 但**零产出**，
// 那一轮所有工具被权限策略挡下。它自己的处理表原文：
//
//	本轮所有需要的工具都被权限策略挡下了，实测被拒的有：
//	- cmd_run —— 无法进入工作目录、无法看 git 状态
//
// 日志实证（同一时段，09:21:25~09:21:40）：
//
//	[agent] executing tool: cmd_run (plugin=cmd)
//	[proc] qq stage before_toolcall 改写了 1 个字段   ← 插件写入了拒绝响应
//	[agent] executing tool: files_ls ...
//	[proc] qq stage before_toolcall 改写了 1 个字段
//	（而同一时段父 main 的工具调用**有 result**，驻留子的**零 result**）
//
// ## 根因
//
// 通道插件的权限门是 `StageScopeGlobal` —— 它对**所有 agent** 的每一轮都生效。
// 而它手里只有"输入来源"（`Extra["input_source"]`），**没有"执行者是谁"**：
//
//	· 父：输入来自 qq ⇒ 按那位 QQ 用户判权限 ✅ 正确
//	· 驻留子：输入**继承自父**（QQ 来源），但执行者**不是那位用户**
//	  ⇒ 插件仍按"那个 QQ 用户"判 ⇒ 开发工具不在白名单 ⇒ 全被拒 ❌
//
// 关键在于：**用户请求已经过了门**（父按真实身份判定过），驻留子做的是
// 父自己的内部委派决定。对已经准入的工作再做一次"用户白名单"检查，
// 既拦掉了合法工作，又不产生任何安全增益（同一次请求，父已判定）。
//
// ## 契约（插件侧读法）
//
//	comefrom == "user"    该调用代表**用户请求**在系统边界上发起 ⇒ 走用户权限模型
//	comefrom == "internal" 该调用是 agent 体系内部的（委派/转投/自主续跑）
//	                       ⇒ 不套"用户白名单"；是否放行由插件按自身策略决定
//
// 缺省（键不存在）时插件应**按旧行为**处理（= "user" 语义）：
// 这条是为了不破坏已发布的插件 —— 它们不认识这个键，行为必须逐字不变。
//
// ## 为什么放 Extra 而不是给 StageContext 加具名字段
//
// `Extra` 已有**通用袋**（proc 桥的 `fExtraBag`）能跨进程/跨 stage 往返
// （见 internal/plugin/proc/shm.go 的注释，那正是为"插件自建键"加的）。
// 走 Extra 不需要改 SDK 结构体、不需要动 shm 的具名字段表，
// 且插件侧 `ctx.Extra["comefrom"]` 与既有 `input_source` 读法一致。
//
// ★ 一旦某天需要它**强类型**（编译期保证），再提升为具名字段；
// 现在 Extra 是成本最低且与既有惯例一致的选择。
const extraKeyComeFrom = "comefrom"

// comefrom 的两个取值。
const (
	// comeFromUser：用户请求在边界上发起（默认语义）。
	comeFromUser = "user"
	// comeFromInternal：agent 体系内部发起（委派、转投、自主续跑）。
	comeFromInternal = "internal"
)

// stampComeFrom 把来源标识写进帧的 Extra（幂等；不覆盖已有值）。
//
// 不覆盖是有意的：更内层（知道更准）的调用点先写了就以它为准；
// 外层只在"没人标过"时补一个保守值。
func stampComeFrom(extra map[string]interface{}, v string) {
	if extra == nil {
		return
	}
	if _, exists := extra[extraKeyComeFrom]; exists {
		return
	}
	extra[extraKeyComeFrom] = v
}

// comefromForAgent 由 agent 的层级身份推导来源标识。
//
// 规则（就这么简单，不需要更多分支）：
//
//	驻留子（有 parentID）⇒ internal   它不是用户请求的边界，是父的委派体
//	根 agent（无 parentID）⇒ user     它才是用户请求的落点
//
// 为什么不去看 `input_source`：来源通道说明"消息从哪来"，不说明"谁在执行"。
// 父处理 QQ 消息时来源是 qq（user）；把这条工作转投给驻留子后，
// 驻留子的来源标签**仍是 qq**（转投刻意保留原事件），但执行者变了。
// 只看来源必然判错 —— 这正是本缺陷的成因。
func (a *Agent) comefromForAgent() string {
	if a == nil {
		return comeFromUser
	}
	if a.parentID != "" {
		return comeFromInternal
	}
	return comeFromUser
}
