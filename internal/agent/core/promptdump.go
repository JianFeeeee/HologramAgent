package core

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 提示词抓包（opt-in 诊断）。
//
// 为什么需要：系统提示词是**每轮动态拼装**的（身份 + 人格 + 记忆 + 时序 +
// 工具顺序 + 输出规则 + 技能索引 + 首启门禁…），只看代码永远说不清模型
// 这一轮到底收到了什么。排查「它怎么又列清单了」「为什么它说有某能力」
// 这类问题时，唯一可靠的事实是**实际发出去的 payload**。
//
// 开关是环境变量而不是配置项：它会把记忆与用户输入原样落盘，属于
// 临时诊断手段，不该在界面上有一个能被随手打开的开关。
//
//	HOMED_PROMPT_DUMP=/var/log/homeagent/prompt.jsonl
//
// 每行一条 JSON：时间、通道、模型、完整 messages（role + content）。
// 文件权限 0600 —— 内容是用户隐私。
const promptDumpEnv = "HOMED_PROMPT_DUMP"

var promptDumpMu sync.Mutex

func dumpPrompt(channel string, req *agentAPI.CompletionRequest) {
	path := os.Getenv(promptDumpEnv)
	if path == "" || req == nil {
		return
	}
	entry := map[string]interface{}{
		"at":      time.Now().Format(time.RFC3339Nano),
		"channel": channel,
		"model":   req.Model,
	}
	msgs := make([]map[string]interface{}, 0, len(req.Messages))
	for _, m := range req.Messages {
		item := map[string]interface{}{"role": m.Role, "content": m.Content}
		if len(m.Blocks) > 0 {
			item["blocks"] = len(m.Blocks)
		}
		if len(m.ToolCalls) > 0 {
			item["tool_calls"] = len(m.ToolCalls)
		}
		msgs = append(msgs, item)
	}
	entry["messages"] = msgs
	entry["tool_count"] = len(req.Tools)

	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	promptDumpMu.Lock()
	defer promptDumpMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(line)
}
