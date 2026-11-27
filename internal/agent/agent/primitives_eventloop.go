package agent

import (
	"log"
)

// cancelCurrentLLM 取消正在进行的 LLM 请求（若有）。
//
// 边界（2026-10-31 更新）：它只停 LLM 流，**不停工具**。
// 工具另有入口 `cancelCurrentTool`，且只由「用户按停止」触发 ——
// 中断抢占用它会把正在执行副作用的工具掤断，那是用户裁定 R5 禁止的。
func (a *Agent) CancelCurrentLLM() {
	a.llmMu.Lock()
	cancel := a.cancelLLM
	a.llmMu.Unlock()
	if cancel != nil {
		cancel()
		log.Printf("[agent] LLM request cancelled by preemption")
	}
}
