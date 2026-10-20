package webui

import (
	"net/http"

	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// 终端面：终端会话列表与命令历史接口。
//
// 职责边界（2026-10-20 调整）：终端状态归 agentcli，内核**不**再维护终端语义
// （旧的 internal/agent/core/terminal_registry.go 已删除）。WebUI 在
// NewHandler 里订阅事件总线（EventTerminalOutput + EventToolCall）攒自己的视图，
// CLI 用同一份 internal/pluginview 实现，两个插件的解析口径一致。
//
// 为什么不塞进 KernelStatus：那是全量快照，前端每 3 秒轮询 /kernel，
// 把每终端最多 64KB 的输出缓冲背进去会让轮询成本爆炸。

// handleTerminals 返回终端会话快照（本插件从事件总线攒的视图）。
func (h *Handler) handleTerminals(w http.ResponseWriter, r *http.Request) {
	if h.termView == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": []interface{}{}})
		return
	}
	terms, _ := h.termView.Snapshot()
	if terms == nil {
		terms = []sdk.TerminalStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"terminals": terms})
}

// handleCmdHistory 返回命令执行历史快照（本插件从事件总线攒的视图）。
func (h *Handler) handleCmdHistory(w http.ResponseWriter, r *http.Request) {
	if h.termView == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"history": []interface{}{}})
		return
	}
	_, cmds := h.termView.Snapshot()
	if cmds == nil {
		cmds = []sdk.CmdExecStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"history": cmds})
}
