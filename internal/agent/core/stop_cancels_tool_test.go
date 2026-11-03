package core

import (
	"os"
	"strings"
	"testing"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// TestStopCancelsRunningTool 钉住：用户按「停止」能掐断**正在跑的工具**。
//
// ## 为什么需要这条判据（2026-10-31 生产实测）
//
// 用户报「实例好像卡死了」。查日志：
//
//	23:20:26 [agent] executing tool: browser_start
//	23:20:27 [agent] tool browser_start timeout 30m0s exceeds hard cap, clamped to 10m0s
//	23:23:55 [agent] stop requested by webui/webui (queued=1 will be short-circuited at pre-action)
//	（之后 3 分钟无任何 tool 返回日志）
//
// 也就是说：模型要求 30 分钟、被硬上限夹到 10 分钟的浏览器会话工具把整轮堵死；
// 用户按了停止，日志说"已请求停止"，但**工具照旧跑到自己超时**。
//
// 根因是接线缺口：`cancelCurrentLLM` 只取消 LLM 的 ctx，而工具的取消管道
// （ExecuteToolCtx → proc 桥 CallContext → call.cancel）虽然早就建好了，
// 却只被「工具超时」这一条路径调用过 —— 停止路径从不碰它。
//
// ## 判据形状
//
// 用一个阻塞到 ctx.Done 的假工具（走真实 executeToolCallOutcome 全链路，
// 含超时包装与 registerToolCancel），然后调 cancelRunningTools，
// 断言它**在 2 秒内返回**且不是"没被取消"的返回值。
func TestStopCancelsRunningTool(t *testing.T) {
	a := New(AgentConfig{
		ID:        "stop-cancels-tool",
		Provider:  &echoTokenProvider{reply: "unused"},
		IO:        agentIO.NewIOManager(),
		StageHost: NewStageHost(),
	})

	started := make(chan struct{})
	release := make(chan struct{}) // 兜底：万一取消没生效，也别让判据永久挂着
	if err := a.stageHost.RegisterTool("slowtool", sdk.ToolDef{
		Name: "slowtool", Parameters: map[string]interface{}{"type": "object"},
	}, func(args map[string]interface{}) (interface{}, error) {
		close(started)
		ctx := CurrentExecuteCtx(args)
		if ctx == nil {
			return "no-ctx", nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return "not-cancelled", nil
		}
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan toolOutcome, 1)
	go func() {
		done <- a.executeToolCallOutcome(agentAPI.ToolCall{
			ID: "c1", Type: "function", Name: "slowtool",
			Arguments: map[string]interface{}{},
		}, "cli")
	}()

	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("工具没有被启动——判据前提不成立")
	}

	// 就是「用户按下停止」那一下。
	a.cancelRunningTools()

	select {
	case out := <-done:
		if strings.Contains(out.Text, "not-cancelled") {
			t.Fatal("★ 停止没有掐断正在跑的工具：工具仍跑到自己结束")
		}
		if strings.Contains(out.Text, "no-ctx") {
			t.Fatal("★ 工具拿不到执行 ctx：取消管道断在 StageHost → handler 这一段")
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("★ 停止后工具 2 秒仍未返回：取消没有传到执行体")
	}
}

// TestInterruptDoesNotCancelTool 钉住 R5：**中断抢占**不得掐断工具。
//
// 用户裁定（R5）：工具执行期间不取消——副作用不可回滚，中途掐断会让
// "决定执行但未执行"变成尴尬状态。所以取消工具的入口只挂在 stop 分支，
// 不能挂到 requestPreempt 分支。
//
// 本判据用源码装配断言（同本仓既有的「钉装配而不是钉注入」做法）：
// `cancelRunningTools` 必须出现在 interceptLoop 的 stop 分支里，
// 且**不得**出现在 requestPreempt 之后的取消点。
func TestInterruptDoesNotCancelTool(t *testing.T) {
	src := readCoreSource(t, "eventloop.go")

	stopAt := strings.Index(src, "armStop(")
	if stopAt < 0 {
		t.Fatal("找不到 stop 分支（armStop）")
	}
	// stop 分支内（armStop 之后一小段）必须调 cancelRunningTools
	window := src[stopAt:minInt(len(src), stopAt+1200)]
	if !strings.Contains(window, "cancelRunningTools()") {
		t.Error("stop 分支必须调 cancelRunningTools()：否则长工具会跑到自己超时，用户按停止没有反应")
	}

	preemptAt := strings.Index(src, "requestPreempt(clone, level)")
	if preemptAt < 0 {
		t.Fatal("找不到抢占分支（requestPreempt）")
	}
	preemptWindow := src[preemptAt:minInt(len(src), preemptAt+600)]
	if strings.Contains(preemptWindow, "cancelRunningTools()") {
		t.Error("★ 抢占分支不得调 cancelRunningTools()：违反 R5（工具副作用不可回滚）")
	}
}

// readCoreSource 读本包的一个源文件（判据用；与 multimodal_departed_test.go
// 里 os.ReadFile 的做法一致，只是收成一个助手免得每处拼路径）。
func readCoreSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("读 %s: %v（判据必须在 internal/agent/core 目录下运行）", name, err)
	}
	return string(b)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
