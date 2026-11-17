package agent

import agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"

// runner_standard.go —— 标准**同步** toolcall 实现（ToolRunner 槽位）。
//
// 用户裁定：「rt agent 的一切接口都需要是异步的」⇒ 主 agent 与驻留子用
// 这一份同步实现，rt 注册自己的异步实现。基座**不假设**是哪一种。
type SynchronousToolRunner struct{}

// RunBatch 执行一批工具并按**索引顺序**返回结果（协议要求：assistant 的
// tool_calls 与其后的 tool 结果消息必须相邻且一一对应）。
func (SynchronousToolRunner) RunBatch(a *Agent, calls []agentAPI.ToolCall, opts BatchOpts) []BatchResult {
	ctx := opts.Ctx
	if ctx == nil {
		ctx = a.ctx
	}
	return a.runToolBatch(ctx, calls, opts)
}
