// Package toolexec 是**工具执行设施**：内核里唯一执行工具的地方。
//
// ## 用户裁定（2026-11-19）
//
// > 「本身不就应该是具体类型的 agentloop 过滤自己定义的工具后，把剩余的工具
// >    丢给基座，由基座给内核嘛，然后基座再把执行结果返给 agentloop，
// >    agentloop 按照工具结果与对应的标记进行处理，比如 nomemory 等标记」
//
// 于是本包只处理**不属于任何类型私有**的工具：
//
//	内核侧登记的（output_* / llm_* / secret_* / plgreload / get_plugin_tools / …）
//	插件工具（StageHost）
//	io / 设备工具（IOManager）
//
// 而 `stablemem_*` / `social_*` / `coldcontext_*` / `persona_set` 这类读类型
// 私有状态的工具**永远不进这里** —— 它们由类型的 agentloop 自己执行。
//
// ## 分层位置
//
//	类型 agentloop ──RunToolBatch──► 基座（5 行转发）──► 本包
//	                                                        ↓
//	                                         路由 → 授权 → 并发 → 超时/取消
//	                                         → panic 恢复 → 脱敏 → 审查
//	                                                        ↓
//	                              同序 []agentPkg.BatchResult + 每条的工具声明（标记）
package toolexec

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// Module 提供工具执行设施。
type Module struct{}

func (Module) Name() string { return "tool-exec" }

// Requires 声明能开工的前置。
//
// ★ agent 必须已装配：宿主（`agent.ToolHost`）是执行时的调用对象。
//
//	  security 必须已装配：授权闸是本包的**必经步骤**，缺了就会"看起来跑了
//
//		但没判"——那比不执行更危险。
func (Module) Requires() []string {
	return []string{kernelmod.FacAgent, kernelmod.FacSecurity}
}

func (Module) Provides() []string { return []string{kernelapi.FacToolExec} }

func (Module) Init(ks *kstate.State) error {
	ks.Provide(kernelapi.FacToolExec, New())
	return nil
}

func init() { kernelmod.Register(Module{}) }
