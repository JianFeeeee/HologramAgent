package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func (a *Agent) runStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	// 通道从 stage ctx 上取（由发起方写入）——内核不持有"当前通道"。
	ch := ""
	if ctx != nil && ctx.Extra != nil {
		ch, _ = ctx.Extra["output_channel"].(string)
	}
	payload := map[string]interface{}{
		"phase":   string(stage),
		"channel": ch,
	}
	if ctx != nil && len(ctx.ToolCalls) > 0 {
		payload["tool"] = ctx.ToolCalls[0].Name
	}
	a.publishEvent(events.EventStage, payload)
	if a.stageHost == nil {
		return false
	}
	ctx.Phase = stage
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] stage %q plugin panic: %v\n%s", stage, r, debug.Stack())
			}
		}()
		a.stageHost.RunStage(stage, ctx)
	}()
	return ctx.Response != nil
}

func (a *Agent) publishEvent(evtType events.EventType, payload map[string]interface{}) {
	if a.eventBus == nil {
		return
	}
	a.eventBus.Publish(&events.Event{
		Type:      evtType,
		Source:    string(a.id),
		Payload:   payload,
		Timestamp: time.Now().Unix(),
	})
}

func (a *Agent) stageCtxFromInput(input, userID, groupID string) *sdk.StageContext {
	ctx := &sdk.StageContext{
		RawMessage: input,
		UserID:     userID,
		GroupID:    groupID,
		Phase:      sdk.StageOnInput,
		Extra:      make(map[string]interface{}),
	}
	// ★ 来源标识必须在**帧的起点**就写入，不能只写在工具帧上：
	// 插件可能在 on_input 阶段就据此决定是否介入（而那个阶段早于工具帧构造）。
	// 见 comefrom.go 的完整说明。
	stampComeFrom(ctx.Extra, a.comefromForAgent())
	return ctx
}

func (a *Agent) injectSourceContext(stageCtx *sdk.StageContext, evt *agentIO.InputEvent) {
	if stageCtx == nil || evt == nil {
		return
	}
	source := evt.Source
	if source == "" {
		source = "unknown"
	}
	channel := evt.OutputChannel
	if channel == "" {
		channel = source
	}
	content := fmt.Sprintf("当前输入来源: %s；默认输出通道: %s。", source, channel)
	// 来源含设备身份（webui/{device_id}）时补充设备名，便于 agent 区分多设备输入
	if devID, _ := evt.Payload["device_id"].(string); devID != "" {
		devName, _ := evt.Payload["device_name"].(string)
		if devName == "" {
			devName = devID
		}
		content = fmt.Sprintf("当前输入来自设备[%s]（%s）；默认输出通道: %s。", devName, devID, channel)
	}
	if flag, _ := evt.Payload["interrupt"].(bool); flag {
		content = fmt.Sprintf("这是一条打断输入。来源: %s；默认输出通道: %s。", source, channel)
	}
	stageCtx.ContextMsgs = append(stageCtx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": content,
	})
}
