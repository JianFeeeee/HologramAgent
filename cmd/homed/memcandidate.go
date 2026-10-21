package main

// 记忆候选消费管线（常驻 goroutine）。
//
// ## 为什么放在 cmd/homed 而不是某个模块包里
//
// 它**不产出任何产物**（纯副作用：把事件写进文本记忆与蒸馏管线）⇒
// 没有"注册到 KernelState"的价值，也不是一个可被依赖的设施。
//
// 但它**依赖** memory（distiller/memDB）与 kernel-io（iom）的产物，
// 所以必须在内核装配**完成之后**启动 —— 由 main.go 在
// RunKernelModules 之后调用（原顺序里它在 textmem 之后、provider 之前）。

import (
	"context"
	"log"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/pipeline"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
)

// startMemoryCandidateConsumer 起一个常驻 goroutine：把 eventbus 上的
// memory_candidate 事件写进文本记忆并喂给蒸馏管线。
func startMemoryCandidateConsumer(ctx context.Context, iom *agentIO.IOManager,
	textMem *text.Memory, memDB *memory.GraphDB, distiller *pipeline.Distiller) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-iom.OutputChan():
				if !ok {
					return
				}
				if evt.Target == "memory" && evt.Type == "memory_candidate" {
					source, _ := evt.Payload["source"].(string)
					input, _ := evt.Payload["input"].(string)
					response, _ := evt.Payload["response"].(string)
					toolsUsed, _ := evt.Payload["tools_used"].([]string)
					toolResults, _ := evt.Payload["tool_results"].([]interface{})
					agentID, _ := evt.Payload["agent_id"].(string)

					if input != "" && textMem != nil {
						te := text.Event{
							Timestamp: time.Now().Unix(),
							Source:    source,
							Input:     input,
							Response:  response,
							ToolsUsed: toolsUsed,
							AgentID:   agentID,
						}
						if err := textMem.Append(te); err != nil {
							log.Printf("[homed] text memory append: %v", err)
						}
					}

					if input != "" && memDB != nil {
						distiller.Append("agent", "user", input)
					}
					if response != "" && memDB != nil {
						distiller.Append("agent", "assistant", response)
					}

					// 工具输出接入蒸馏管线
					for _, tr := range toolResults {
						if trMap, ok := tr.(map[string]interface{}); ok {
							if out, ok := trMap["output"].(string); ok && out != "" && memDB != nil {
								distiller.Append("agent", "tool", out)
							}
						}
					}
				}
			}
		}
	}()
}
