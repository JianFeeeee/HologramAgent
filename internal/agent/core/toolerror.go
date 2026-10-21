package core

import (
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 工具失败原因码（与 SDK 的 ToolError.Reason 对应）。
const (
	ErrReasonRequired     = "required"
	ErrReasonType         = "type"
	ErrReasonUnauthorized = "unauthorized"
	ErrReasonTimeout      = "timeout"
	ErrReasonNotFound     = "not_found"
)

// newToolError 构造一个结构化失败。
func newToolError(reason, field, detail, hint string) *sdk.ToolError {
	return &sdk.ToolError{Reason: reason, Field: field, Detail: detail, Hint: hint}
}

// isToolError 报告一个工具返回值是否表示**失败**。
//
// 存在的理由：工具失败是以 `nil` error + 错误**值**返回的，而
// ToolResult.Success 此前被硬编码为 true（唯一赋值点），该字段恒真、
// 结构上不可能为 false。
//
// ⚠️ 判据必须兼容**既有三种约定**（实测于仓内，否则升级会把存量插件
// 的成功误判成失败——这是本函数最大的回归风险）：
//
//	① {"error": msg}                      pluginmgr、cmd 的参数校验
//	② {"isError": true, "content": msg}   files / clawhubadapter 的 errorResult
//	③ *sdk.ToolError                      新写的工具（可选，不是迁移要求）
//
// 明确**不**作为失败判据的：
//   - exit_code != 0：命令跑了但返回非零，属业务结果且带真实 stdout/stderr，
//     整条判失败会误伤「命令可用但结果非零」这类正常场景。
//   - stderr 非空：cmd 成功路径常带 stderr（如 warn: deprecated）。
//   - 字符串 / 数字 / bool / 数组 / nil：均视为成功（output_send 成功即返回 "ok"）。
func isToolError(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case *sdk.ToolError:
		return x != nil
	case sdk.ToolError:
		return true
	case error:
		// 工具显式返回 error —— 失败。
		return x != nil
	case map[string]interface{}:
		// ② isError 优先：显式布尔标记，语义最明确。
		if b, ok := x["isError"].(bool); ok && b {
			return true
		}
		// ① error 键：非空字符串才算失败。
		if e, ok := x["error"]; ok {
			switch ev := e.(type) {
			case string:
				return strings.TrimSpace(ev) != ""
			case nil:
				return false
			default:
				// error 是结构化值（如嵌套的 ToolError）—— 视为失败。
				return true
			}
		}
		return false
	case string:
		// 自由文本无法可靠判别成败，按**成功**处理（保守：宁可少报失败，
		// 也不要把正常结果报成失败）。失败请用上面三种显式形态。
		return false
	default:
		return false
	}
}

// toolErrorText 把一个失败返回值渲染成给模型看的文本。
// 结构化 ToolError 会带上 Hint——这是「让模型看得懂真因」的关键。
func toolErrorText(toolName string, v interface{}) string {
	switch x := v.(type) {
	case *sdk.ToolError:
		return renderToolError(toolName, x)
	case sdk.ToolError:
		return renderToolError(toolName, &x)
	case map[string]interface{}:
		if b, ok := x["isError"].(bool); ok && b {
			msg, _ := x["content"].(string)
			if msg == "" {
				msg = "（插件未给出原因）"
			}
			return fmt.Sprintf("工具 %s 失败: %s", toolName, msg)
		}
		if e, ok := x["error"]; ok {
			if s, ok := e.(string); ok {
				return fmt.Sprintf("工具 %s 失败: %s", toolName, s)
			}
		}
	case error:
		return fmt.Sprintf("工具 %s 失败: %v", toolName, x)
	}
	return fmt.Sprintf("工具 %s 失败: %v", toolName, v)
}

// renderToolError 渲染结构化失败：把 field/reason/hint 都摆出来，
// 让模型知道**该改什么**，而不是只知道「失败了」。
func renderToolError(toolName string, e *sdk.ToolError) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "工具 %s 失败", toolName)
	if e.Field != "" {
		fmt.Fprintf(&sb, "（字段 %s）", e.Field)
	}
	sb.WriteString(": ")
	if e.Reason != "" {
		sb.WriteString(e.Reason)
	}
	if e.Detail != "" {
		sb.WriteString(" — ")
		sb.WriteString(e.Detail)
	}
	if e.Hint != "" {
		sb.WriteString("\n请据此修正后重试：")
		sb.WriteString(e.Hint)
	}
	return sb.String()
}

// renderToolResult 把工具返回值渲染成给模型看的文本。
//
// 规则：**结构化失败优先**——失败必须带上可执行信息（字段/原因/Hint），
// 而不是退化成 `map[error:xxx]` 这种模型读不懂的 Go 语法。
// 成功则用紧凑 JSON（绝不用 fmt.Sprintf("%v")，那会产出 Go 的 map 语法）。
func renderToolResult(toolName string, raw interface{}) string {
	if raw == nil {
		return ""
	}
	if isToolError(raw) {
		return toolErrorText(toolName, raw)
	}
	switch x := raw.(type) {
	case string:
		return x
	case error:
		return fmt.Sprintf("%v", x)
	}
	// 非字符串：紧凑 JSON。
	if b, err := json.Marshal(raw); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", raw)
}
