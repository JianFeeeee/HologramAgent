package client

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// CmdRouter 命令路由器，支持按命令前缀分发到不同 handler。
// 用于 CLI 和 GUI 根据能力类型注册不同的执行函数。
type CmdRouter struct {
	mu       sync.RWMutex
	prefixes map[string]CmdHandler
	default_ CmdHandler
}

// NewCmdRouter 创建命令路由器。
func NewCmdRouter() *CmdRouter {
	return &CmdRouter{
		prefixes: make(map[string]CmdHandler),
	}
}

// Handle 注册匹配指定前缀的命令处理器。
// 例如 Handle("homeagent-", homeagentHandler) 会处理所有 homeagent-* 命令。
func (r *CmdRouter) Handle(prefix string, handler CmdHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prefixes[prefix] = handler
}

// HandleDefault 注册默认命令处理器（无前缀匹配时使用）。
func (r *CmdRouter) HandleDefault(handler CmdHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.default_ = handler
}

// Dispatch 分发命令到匹配的处理器。
// 返回 true 表示已处理，false 表示无匹配。
func (r *CmdRouter) Dispatch(reqID, command string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 先按前缀匹配
	for prefix, handler := range r.prefixes {
		if strings.HasPrefix(command, prefix) {
			handler(reqID, command)
			return true
		}
	}

	// 无前缀匹配，使用默认
	if r.default_ != nil {
		r.default_(reqID, command)
		return true
	}
	return false
}

// ===== 能力解析辅助 =====

// ParseHomeagentCmd 解析 homeagent-* 命令，返回能力名和参数。
// 例如 "homeagent-screensue 5 你好" → ("screensue", "5 你好")
// 也支持 "screensue 5 你好"（无前缀）
func ParseHomeagentCmd(command string) (capability, args string) {
	cmd := strings.TrimSpace(command)
	// 去掉 homeagent- 前缀
	cmd = strings.TrimPrefix(cmd, "homeagent-")
	parts := strings.SplitN(cmd, " ", 2)
	capability = parts[0]
	if len(parts) > 1 {
		args = parts[1]
	}
	return
}

// ParseJSONCmd 解析 JSON 格式的命令参数。
// 例如 "computeruse {\"x\":100,\"y\":200,\"action\":\"click\"}"
// 返回动作名和参数 map。
func ParseJSONCmd(command string) (action string, params map[string]interface{}, err error) {
	cmd := strings.TrimSpace(command)
	// 去掉 homeagent- 前缀
	cmd = strings.TrimPrefix(cmd, "homeagent-")

	idx := strings.IndexByte(cmd, '{')
	if idx < 0 {
		action = cmd
		return
	}
	action = strings.TrimSpace(cmd[:idx])
	jsonStr := cmd[idx:]
	if err = json.Unmarshal([]byte(jsonStr), &params); err != nil {
		err = fmt.Errorf("parse json params: %w", err)
	}
	return
}

// BaseResult 构造基础命令结果。
func BaseResult(reqID, status, output, errMsg string) map[string]interface{} {
	res := map[string]interface{}{
		"op":     "cmd_result",
		"req_id": reqID,
		"status": status,
	}
	if output != "" {
		res["output"] = output
	}
	if errMsg != "" {
		res["error"] = errMsg
	}
	return res
}
