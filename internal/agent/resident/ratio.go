package resident

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// ratio.go —— 驻留子自己的**水位算术**（它自己的上下文，不问基座）。

// ratioOf 返回 x 的上下文水位（积累 token / 窗口）。
//
// ★ 水位是**类型策略的输入**：驻留子用整窗策略，撞 90% 就上报父；
//
//	主 agent 用相关性裁剪；rt 滑窗。各算各的，基座不做这个算术。
func ratioOf(x *agent.Agent) float64 {
	if x == nil {
		return 0
	}
	window := x.ContextWindow()
	if window <= 0 {
		return 0
	}
	return float64(x.ContextAccumulated()) / float64(window)
}
