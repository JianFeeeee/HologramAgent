package worker

// instance.go —— 造**工作式子自己的** agent 实例（与 resident 同构）。
//
// ## 与 resident 的差别
//
//	resident  长期驻留：有自己的输入队列、调度器、轻量图记忆、上下文落盘
//	worker    一次性：跑完就退役，不接通道，不要图记忆，不要上下文落盘
//
// 所以这里的 `AgentConfig` 明显更薄 —— 但**实例是独立的**：
// 它有自己的 ctx、自己的 ID、自己的 ParentID，走自己的 `RunToolBatch`
// （安全闸按**它自己**的身份判，不是父的）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// newInstance 造一个工作式子实例（尚未登记/启动）。
func (m *M) newInstance(id, task, parentChannel string, maxTurns int) (*workerEntry, error) {
	parent := m.A
	if parent == nil {
		return nil, fmt.Errorf("worker: 父 agent 未装配")
	}

	// ★ 子自己的 io：独立输入入口 + **共享通道登记表** + 父视图回退。
	//
	// 为什么仍要给 io（worker 不接通道）：`output_send__*` 都要经它；
	// 而"不接通道"的意思是**不划 inputch**，不是没有 io。
	childIO := agentIO.NewIOManager()
	if reg := parent.IO().ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(parent.IO())

	ctx, cancel := context.WithCancel(parent.Ctx())

	a := agent.New(agent.AgentConfig{
		ID:       types.AgentID(id),
		TypeName: TypeName,
		// ★ 能力面（docs §8.1.1）：工作式子承接文本。
		AgentCap: agentIO.CapText,
		// 提示词由**本类型**渲染（§ 渲染归类型）：worker 有自己的一套
		// （执行纪律 / 工具执行顺序），不继承父的人格与通道规则。
		// ★ 安全运行时必须继承父的闸门（子不得比父宽）。
		// ★ 记录进同一张表（安全判定与实例持久化同一份事实）。
		// ★ 只继承设施句柄：子的选源/回退/记账都由它负责。
		// 完成通知回到它自己的出口，不是父的。
		KernelSource: string(parent.ID()),
		ParentID:     string(parent.ID()),
		TaskPrompt:   taskMaxTurnsHint(maxTurns),
		AllowedTools: nil,
		DenyTools:    nil,
	})

	a.MountSurface(childIO, parent.StageHost(), parent.Gate(), parent.AgentsRegistry(), "", parent.LLM())
	a.MountToolRegistry(parent.ToolRegistry())
	// ★ 父声明类（系统提示词素材 / 优先模型源）：装配期写入，运行期只读。
	//   worker 的提示词**由本类型渲染**（不继承父的人格与通道规则），
	//   所以这里给的是本类型自己的模板渲染结果，而不是 parent.SystemPrompt()。
	a.SetSystemPrompt(fmt.Sprintf(childSystemPromptTemplate, task))
	a.SetPreferredProvider(parent.PreferredProvider())

	ent := newWorkerEntry(id, a, m, string(parent.ID()), childIO, ctx, cancel)
	// ★ 载体回指：父侧任务表用它读产出。
	_ = parentChannel
	return ent, nil
}

// taskMaxTurnsHint 把轮数上限塞进任务提示（模型据此自行收尾）。
func taskMaxTurnsHint(maxTurns int) string {
	if maxTurns <= 0 {
		return ""
	}
	return fmt.Sprintf("（本任务最多 %d 轮工具调用，到限请给出已有结论）", maxTurns)
}

var _ = strings.TrimSpace
