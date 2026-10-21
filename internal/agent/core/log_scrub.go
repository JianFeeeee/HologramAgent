package core

// 日志脱敏：写进日志的文本必须先清洗明文凭据。
//
// ## 为什么这条比"拦住模型"更紧迫
//
// 模型上下文是**短期**的：一轮对话结束就不再发送。
// 而日志是**长期**的：
//
//	/home/newqqagent/log/homed_*.log  每天一个文件，从不自动清理
//	systemd journal                    另存一份
//	用户/技术支持/issue 排查时会把日志贴出来
//
// 一次泄漏进日志 = 永久泄漏。凭据被轮换前，任何拿到日志的人都能用。
//
// ## 具体泄漏点（2026-10-18 审计）
//
//	task.go:1031  log.Printf("[agent] tool %s result: %s", ..., truncateStr(result, 100))
//	              ← 工具输出可能含服务端回显的凭据（enc_shell 跑 curl 拿 200）
//	task.go:967   log.Printf("[agent] tool %s raw_arguments: %s", ..., truncateStr(tc.RawArguments, 300))
//	              ← 模型给的原始参数。正常是别名，但插件工具可能被直接传明文
//
// 两处都**原样打印**，没有经过 security.Replace。
//
// ## 实现
//
// scrubForLog 是唯一入口，所有"把工具内容写进日志"的地方都走它。
// 空安全：security.Global() 未初始化时原样返回（不 panic、不阻塞）。
// 幂等：已是别名（sec_xxx）的文本再扫一次不会被改动。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/security"
)

// scrubForLog 清洗要写进日志的文本。
//
// where 用于日志里标注拦截位置（如 "tool_result:enc_shell"），
// 便于事后审计"哪些工具的输出带凭据"。
func scrubForLog(text, where string) string {
	if text == "" {
		return text
	}
	st := security.Global()
	if st == nil {
		return text
	}
	out, evts := st.Replace(text, "log:"+where, security.Patterns)
	if len(evts) == 0 {
		return text
	}
	// 拦截本身要记一笔（不打印原文，只打印模式与长度）——
	// 否则运维不知道"这个工具经常吐凭据"，也就不会去修插件。
	for _, e := range evts {
		log.Printf("[security] 日志拦截 %s ← %s（%d 字节），原文未落盘",
			where, e.Kind, e.Len)
	}
	return out
}
