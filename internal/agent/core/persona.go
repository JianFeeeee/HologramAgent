package core

import (
	"fmt"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// PersonaStore 是人格设定的读写面。
//
// 首启门禁（buildSystemPrompt）与 persona_set 工具都通过它工作，实现在 cmd/homed：
// 读写配置项 core.agent.personal_prompt 与一次性标记 core.internal.persona_initialized
// （落库逻辑与 WebUI 向导共用 internal/config 的实现）。
//
// 为什么放在内核而不是某个通道插件：人格是**任何通道都要问一次**的事。
// 系统提示词每轮重建，门禁放在这里，WebUI / QQ / CLI / ACP / 邮件等全部通道自动覆盖。
type PersonaStore interface {
	// PersonaInitialized 报告人格是否已确认（向导或工具已问过）。
	PersonaInitialized() bool
	// SetPersona 落库人格并打一次性标记，返回是否需要重启才生效。
	SetPersona(mode, content string) (restartRequired bool, err error)
}

// executePersonaTool 落地首启人格设定。
//
// 成功即打一次性标记 → 之后 buildSystemPrompt 不再要求模型询问人格。
// custom 模式返回「需重启生效」：人格在 homed 启动时载入。
func (a *Agent) executePersonaTool(tc agentAPI.ToolCall) string {
	if a.personaStore == nil {
		return "人格设定不可用：内核未接入配置"
	}
	mode, _ := tc.Arguments["mode"].(string)
	content, _ := tc.Arguments["content"].(string)
	mode = strings.TrimSpace(mode)
	restart, err := a.personaStore.SetPersona(mode, content)
	if err != nil {
		return fmt.Sprintf("人格设定失败：%v", err)
	}
	switch mode {
	case "custom":
		msg := "已保存自定义人格"
		if restart {
			msg += "；**重启 homed 后生效**（人格在启动时载入）"
		}
		return msg
	case "default":
		return "已确认使用默认人格"
	case "later":
		return "已记为「以后再说」，继续使用默认人格"
	default:
		return "已保存人格设定"
	}
}
