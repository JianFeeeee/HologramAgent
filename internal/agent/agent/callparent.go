package agent

// callparent.go —— 子 agent → 父 agent 的**唯一**上行接口（用户裁定 2026-11-18）。
//
// ## 用户原话
//
// > 「内核统一向子 agent 暴露一个 callparent 接口（各个子 agent 借助这个接口
// >    实现如 askparent，contextfull 等，该接口可设置 l1-l3 的中断）」
//
// > 「主 agent 对子 agent 的管理通道以及子 agent 向主 agent 的上报通道
// >    要复用现有的基础设施而不是再开一套」
//
// ## 复用的是什么（不新开通道、不新开注入原语）
//
// 子 → 父的通道**本来就是"父的一个入站 inputch"**：
//
//	名字     `inboundChannelName(childID)`
//	归属     Owner = 父（创建子时由 `residentInboundChannel` 登记进共享登记表）
//	注入     `IOManager.InjectInterrupt*Opts`
//	级别     `InjectOptions.Priority`（L1..L4）
//
// 所以本文件**不新增**通道类型、注入方法或优先级枚举。它只做两件事：
//
//	1. 把"往父投一条带级别的消息"收成**一个调用**（取代散写的 4 处）；
//	2. 让**父**在创建子时把"怎么投"装进子的基座（`InstallCallParent`）——
//	   因为只有父知道自己的 IOManager 与那条入站通道名，子不该自己猜。
//
// ## 为什么必须由父安装，而不是子自己拼
//
// 子若自己拼，就要猜两件事：父的 io 在哪、入站通道叫什么。历史上正是这里
// 出过两次故障：
//
//	· rt 把通道名拼成 "rt/"+id 而路由按 id 查 ⇒ 插件输入永远打在父身上
//	· 子在自己的 io 上注入 ⇒ 消息进了**子自己的队列**（父根本收不到）
//
// 让父一次性把闭包装进来，这两种拼错在结构上就不可能出现。

import (
	"fmt"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// CallLevel 是子 → 父上报的**中断级别**（复用内核既有的 L1..L3 阶梯）。
//
// ★ 刻意只到 L3：L4 是"内核级、立即打断"（只有编译期内置插件可声明，
// 见 sdk.PriorityL4 的注释）。子 agent 不是内核级主体 —— 它往上投的都是
// "父该知道的事"，最急也只到交互级。给它 L4 等于让一个普通子能抢占父的
// 全部执行，而父无从分辨"这是内核要求"还是"某个子在自作主张"。
type CallLevel string

const (
	// CallLevelBackground(L1) 是后台信息：父排空时才需要看一眼。
	// 典型：任务完成通知、定期状态。
	CallLevelBackground CallLevel = pubsdk.PriorityL1
	// CallLevelMessage(L2) 是普通消息：父该处理，但不打断正在跑的轮次。
	// 典型：contextfull 上报（父需要决定是否回收/压缩）。
	CallLevelMessage CallLevel = pubsdk.PriorityL2
	// CallLevelInteractive(L3) 是需要父**尽快**回应的事。
	// 典型：askparent（子等父答复）、残余任务交接。
	CallLevelInteractive CallLevel = pubsdk.PriorityL3
)

// Validate 报告级别是否合法（只接受 L1..L3）。
//
// ★ 为什么必须校验而不是让内核夹取值：L4 被夹到 L3 是**外部插件**的
//
//	防越权规则。子 agent 根本没有 L4 这一档，给它一个"会被夹"的值，
//	等于让调用方以为它拿到了立即打断的能力。
func (l CallLevel) Validate() error {
	switch l {
	case CallLevelBackground, CallLevelMessage, CallLevelInteractive:
		return nil
	}
	return fmt.Errorf("callparent: 非法级别 %q（只接受 L1/L2/L3；L4 属内核级插件，子 agent 不可用）", string(l))
}

// CallKind 是上报的**语义类别**（决定父侧怎么分流）。
//
// ★ 用枚举而不是自由字符串：父侧必须对每个类别有明确处理，否则一条内部
// 消息会被当成普通对话塞进模型上下文 —— 模型看得见却不知道该怎么处理。
type CallKind string

const (
	// CallKindNotify 是纯通知：父知道就好，不需要动作。
	CallKindNotify CallKind = "notify"
	// CallKindAsk 是需要父**答复**的提问（`askparent`）。
	CallKindAsk CallKind = "ask"
	// CallKindContextFull 是子的上下文撞窗口上报（父决定回收/压缩）。
	CallKindContextFull CallKind = "contextfull"
	// CallKindHandover 是子被回收时的上下文交接（父按需拉取原文）。
	CallKindHandover CallKind = "handover"
)

// Validate 报告类别是否合法。
func (k CallKind) Validate() error {
	switch k {
	case CallKindNotify, CallKindAsk, CallKindContextFull, CallKindHandover:
		return nil
	}
	return fmt.Errorf("callparent: 非法上报类别 %q", string(k))
}

// CallParentFunc 是**父侧安装**的上行投递函数（子只持有，不自己实现）。
//
// 父知道自己的 IOManager 与那条入站通道名，所以"怎么投"这一件事归父；
// 子只需要说"我要上报什么"。
type CallParentFunc func(level CallLevel, kind CallKind, text string, meta map[string]interface{}) error

// InstallCallParent 装入上行投递函数（**父在创建子时调用一次**）。
//
// 未装入 ⇒ `CallParent` 返回错误（不静默成功）：子以为上报了而父没收到，
// 是最难排查的一类故障。
func (a *Agent) InstallCallParent(fn CallParentFunc) {
	if a != nil {
		a.callParent = fn
	}
}

// CallParent 向父 agent 上报一条消息（子 agent 的**唯一**上行接口）。
//
//   - `level` 决定优先级（L1/L2/L3），复用内核既有的中断阶梯；
//   - `kind` 决定载荷形态（父侧按 `callparent_kind` 分流）；
//   - `text` 是给父看的人类可读正文（可空，例如 contextfull 只看 kind）；
//   - `meta` 是附加键值（合并进 payload，供父侧程序化读取）。
func (a *Agent) CallParent(level CallLevel, kind CallKind, text string, meta map[string]interface{}) error {
	if a == nil {
		return fmt.Errorf("callparent: nil agent")
	}
	if err := level.Validate(); err != nil {
		return err
	}
	if err := kind.Validate(); err != nil {
		return err
	}
	if a.callParent == nil {
		return fmt.Errorf("callparent: agent %q 未装入上行函数（父创建它时必须调 InstallCallParent）", string(a.id))
	}
	return a.callParent(level, kind, text, meta)
}

// callParentReservedKeys 是 CallParent 的**保留载荷键**（meta 不得覆盖它们）。
//
// 父侧分流靠 `callparent_kind` / `callparent_from`；能被 meta 覆盖的话，
// 一条 contextfull 就能伪装成 notify，或伪装成别的子发的。
var callParentReservedKeys = map[string]bool{
	"type":              true,
	"content":           true,
	"callparent_kind":   true,
	"callparent_level":  true,
	"callparent_from":   true,
	"callparent_parent": true,
}

// BuildCallParent 是**父侧**构造上行投递函数的唯一实现。
//
// 参数由父给出（它知道自己的 io、子的 id、那条入站通道名），于是：
//
//	· 通道名只由 `inboundChannelName(childID)` 一处产生（不再各拼一次）
//	· 注入发生在**父的 IOManager** 上（不会误投进子自己的队列）
//	· 级别 / 保留键 / 记忆与场面策略在这里统一处理
//
// 驻留子与 rtchild 都调它 —— 这就是"复用现有基础设施"的落点：
// 一张登记表、一套 InjectOptions、一套优先级阶梯，没有第二套。
func BuildCallParent(parent *Agent, childID, inboundCh string) CallParentFunc {
	if parent == nil || parent.io == nil {
		return nil
	}
	return func(level CallLevel, kind CallKind, text string, meta map[string]interface{}) error {
		if parent.io == nil {
			return fmt.Errorf("callparent: 父 %q 的 IO 已销毁", string(parent.id))
		}
		payload := map[string]interface{}{
			// type 是**消息类型**（内核按它选通道 Cleaner、决定怎么进上下文）。
			// 上报一律是文本载体，结构化部分在 callparent_* 键与 meta 里。
			"type":    "text",
			"content": text,

			"callparent_kind":   string(kind),
			"callparent_level":  string(level),
			"callparent_from":   childID,
			"callparent_parent": string(parent.id),
		}
		for k, v := range meta {
			if callParentReservedKeys[k] {
				continue
			}
			payload[k] = v
		}

		// 上报是**系统内部信号**：不据其内容召回记忆、不参与场面识别
		// （与心跳/自循环同族）。级别走 level。
		parent.io.InjectInterruptOpts(childID, inboundCh, payload, agentIO.InjectOptions{
			Priority:     string(level),
			RecallPolicy: pubsdk.RecallPolicyNone,
			ScenePolicy:  pubsdk.ScenePolicyNone,
		})
		return nil
	}
}
