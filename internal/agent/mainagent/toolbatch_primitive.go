package mainagent

// toolbatch_primitive.go —— **工具请求入口**（经基座转发到内核执行设施）。
//
// ## 用户裁定（2026-11-19）的流向
//
//	类型 agentloop 过滤出**自己定义的工具**自己执行，
//	其余丢给基座 → 内核执行设施 → 结果 + 工具声明 返给 agentloop。
//
// 本文件是"其余"那一步：**只转发**，不做路由、不做判定、不做清洗。
// 执行链的唯一实现在**内核工具执行设施**（基座 `RunToolBatch`）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// RunToolBatch 把一批工具交给内核执行设施。
func (m *Module) RunToolBatch(calls []agent.ToolCall, opts agent.BatchOpts) []agent.BatchResult {
	if m == nil || m.a == nil {
		return nil
	}
	if opts.Ctx == nil {
		opts.Ctx = m.a.Ctx()
	}
	if opts.Host == nil {
		opts.Host = m.a.ToolHost()
	}
	return m.a.RunToolBatch(calls, opts)
}
