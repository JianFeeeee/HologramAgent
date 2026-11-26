package mainagent

import (
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"log"
	"runtime/debug"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// RunStage 执行某个阶段的插件处理器。
//
// ★ 它**不是**槽位（I 批删了那个槽位）：实现只有一份（发事件 + 转给
//
//	StageHost），而所有调用点都在本包内。一个"只能填一种函数"的槽位
//	不是扩展点，是转发层。
//
// 类型要拦/改阶段行为，注册 StageHost 的 handler（那是**插件面**），
// 而不是替换整个阶段的执行方式。
func (m *Module) RunStage(stage sdk.Stage, ctx *sdk.StageContext) bool {
	if m == nil || m.a == nil {
		return false
	}
	return m.runStageDefault(stage, ctx)
}

// runStageDefault 是标准阶段执行（由 hooks 注册）。
func (m *Module) runStageDefault(stage sdk.Stage, ctx *sdk.StageContext) bool {
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
	if m.stageHostNow() == nil {
		return false
	}
	ctx.Phase = stage
	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] stage %q plugin panic: %v\n%s", stage, r, debug.Stack())
			}
		}()
		m.stageHostNow().RunStage(stage, ctx)
	}()
	return ctx.Response != nil
}

func (m *Module) stageCtxFromInput(input, userID, groupID string) *sdk.StageContext {
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
	agent.StampComeFrom(ctx.Extra, m.a.ComefromForAgent())
	return ctx
}

func (m *Module) InjectSourceContext(stageCtx *sdk.StageContext, evt *agentIO.InputEvent) {
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
	content := fmt.Sprintf("Current input source: %s; default output channel: %s.", source, channel)
	// 来源含设备身份（webui/{device_id}）时补充设备名，便于 agent 区分多设备输入
	if devID, _ := evt.Payload["device_id"].(string); devID != "" {
		devName, _ := evt.Payload["device_name"].(string)
		if devName == "" {
			devName = devID
		}
		content = fmt.Sprintf("Current input comes from device [%s] (%s); default output channel: %s.", devName, devID, channel)
	}
	if flag, _ := evt.Payload["interrupt"].(bool); flag {
		content = fmt.Sprintf("This is an interrupt input. Source: %s; default output channel: %s.", source, channel)
	}
	stageCtx.ContextMsgs = append(stageCtx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": content,
	})
}
