package agent

// toolresult_face.go —— 超长工具结果的**基座侧**入口（纯逻辑在 ctxmgr）。
//
// ## 分工（判据 §8 第 3 条）
//
// 落盘 / 回读的**纯逻辑**（预览文案、句柄登记、区间拼接）住在 `ctxmgr`
// （不认识 Agent），基座只做三件：拿句柄、拿事件 ID、转发。
//
// 这样 `ctxmgr` 不必 import 基座（否则成环），而基座也不必自己实现一遍。

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/agent/ctxmgr"
)

// storeToolResult 把超长结果正文落盘，返回元信息。IO 委托给上下文。
func (a *Agent) storeToolResult(evtID, tool, text string) (*ToolResultStore, error) {
	if a == nil || a.context == nil {
		return nil, fmt.Errorf("toolresult: agent 未就绪")
	}
	if a.context.SavePath() == "" {
		return nil, fmt.Errorf("toolresult: 未配置持久化目录，无法落盘")
	}
	return a.context.PersistToolResult(evtID, tool, text)
}

// offloadToolResult 把超长结果落盘，返回替换后的预览文本。
//
// 返回 ok=false 时调用方保持原内容不变（落盘失败不能吞掉结果 ——
// 宁可撞窗口也不能让模型看不到任何东西）。
func (a *Agent) offloadToolResult(f *TaskFrame, tool, content string) (string, bool) {
	if a == nil || a.context == nil || content == "" {
		return "", false
	}
	evtID := ""
	if f != nil {
		evtID = f.EventID
	}
	if evtID == "" {
		evtID = ctxmgr.NewEventID()
		if f != nil {
			f.EventID = evtID
		}
	}
	st, err := a.storeToolResult(evtID, tool, content)
	if err != nil {
		a.Logf("toolresult offload failed (tool=%s): %v — 保持原内容", tool, err)
		return "", false
	}
	a.context.RegisterToolResultStore(evtID, st)
	return ctxmgr.PreviewToolResult(evtID, st, ctxmgr.HeadLinesOf(content, ctxmgr.PreviewHeadLines), ctxmgr.PreviewHeadLines), true
}

// readStoredToolResult 实现 toolresult_read：按句柄 + 字节区间读回全文。
func (a *Agent) readStoredToolResult(args map[string]interface{}) (string, error) {
	evtID, _ := args["event_id"].(string)
	if evtID == "" {
		return "", fmt.Errorf("缺少 event_id（预览里给出的句柄）")
	}
	offset := 0
	if v, ok := args["offset"].(float64); ok {
		offset = int(v)
	}
	limit := 0
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if offset < 0 {
		return "", fmt.Errorf("offset 不能为负")
	}
	// 单次读取上限：防止模型一次要 500MB 把上下文再撞一次。
	const maxReadBytes = 64 * 1024
	if limit <= 0 {
		limit = maxReadBytes
	}
	if limit > maxReadBytes {
		limit = maxReadBytes
	}
	if a.context == nil {
		return "", fmt.Errorf("上下文不可用")
	}
	st := a.context.FindToolResultStore(evtID)
	if st == nil {
		return "", fmt.Errorf("未找到 event_id=%s 的工具结果记录：可能句柄有误，或该事件已随上下文淘汰", evtID)
	}
	body, atEnd, err := a.context.ReadToolResultBody(evtID, st, offset, limit)
	if err != nil {
		return "", err
	}
	return ctxmgr.FormatReadBack(st, offset, body, atEnd), nil
}

// HasStoredToolResults 判断当前上下文里是否有可回读的超长工具结果。
func (a *Agent) HasStoredToolResults() bool {
	if a == nil || a.context == nil {
		return false
	}
	return a.context.HasAnyToolResultStore()
}
