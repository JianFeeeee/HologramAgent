package proc

import "context"

// 执行上下文在 args 上的旁路键。
//
// ⚠️ 必须与 core.executeCtxArgKey **逐字一致**。
//
// 为什么复制一份常量而不是从 core 导入：proc 包被 core 依赖（StageHost
// 通过 sdk.ToolHandler 间接用到插件桥），反向导入会成环。这个键只是
// 两包之间的一个私有约定，复制 + 判据（proc 侧的空键行为测试）比拆包更轻。
//
// 一致性的真实风险很低：键里带不可见前缀与点号，正常工具参数不可能撞上；
// 而万一不一致，表现是"取消不生效"（工具照旧跑完），不是功能错误。
const execCtxArgKey = "\x00kernel.execute_ctx\x00"

// execCtxFromArgs 取出内核绑在本次调用上的 ctx；没有则返回 Background。
func execCtxFromArgs(args map[string]interface{}) context.Context {
	if args != nil {
		if ctx, ok := args[execCtxArgKey].(context.Context); ok && ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

// stripExecCtx 返回去掉旁路键的副本，供下发给插件用。
//
// 插件可能对参数做严格校验，这个内核内部键不该出现在它眼里。
func stripExecCtx(args map[string]interface{}) map[string]interface{} {
	if args == nil {
		return nil
	}
	if _, ok := args[execCtxArgKey]; !ok {
		return args
	}
	out := make(map[string]interface{}, len(args))
	for k, v := range args {
		if k == execCtxArgKey {
			continue
		}
		out[k] = v
	}
	return out
}
