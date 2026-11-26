package worker

// args.go —— admin_child 的参数读取（arg 是规范化的 map，不是 ToolCall）。
//
// ★ 父子协议里参数全在 `arg` 里（顶层只有 type/action/meta/arg），
//   所以这里读 map 而不是 ToolCall。与驻留子/rt 各自的 strOf 同一形态。

import "strings"

func strOf(arg map[string]interface{}, key string) string {
	if arg == nil {
		return ""
	}
	s, _ := arg[key].(string)
	return strings.TrimSpace(s)
}

func intOf(arg map[string]interface{}, key string) int {
	if arg == nil {
		return 0
	}
	switch v := arg[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
