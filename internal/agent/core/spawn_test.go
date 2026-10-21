package core

import (
	"fmt"
	"strings"
	"testing"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// child_result 必须幂等——这是 "任务已结束但核心循环不结束" 的根因修复。
//
// 子任务完成通知会写进持久上下文（formatMergedTimeline 每轮重新注入），
// 模型之后还会再查。若第二次查询返回 "不存在或已过期" 这种**永久失败信号**，
// 模型会认定任务未完成而无限重试/汇报（生产实测：单轮 35 次工具调用、
// 持续 514 秒）。
func TestChildResultIsIdempotent(t *testing.T) {
	a := New(AgentConfig{ID: "t"})

	a.childMu.Lock()
	a.childTasks["child_1"] = &childTaskState{result: "任务完成：已创建 3 个日程", seq: 1}
	a.childMu.Unlock()

	call := func(id string) string {
		return a.executeChildResultTool(agentAPI.ToolCall{
			Name:      "child_result",
			Arguments: map[string]interface{}{"task_id": id},
		})
	}

	first := call("child_1")
	if !strings.Contains(first, "任务完成：已创建 3 个日程") {
		t.Fatalf("首次查询应返回结果，实际: %q", first)
	}

	second := call("child_1")
	if strings.Contains(second, "不存在") {
		t.Fatalf("重复查询不能返回失败信号（会驱动模型无限重试），实际: %q", second)
	}
	if !strings.Contains(second, "已完成") {
		t.Fatalf("重复查询应明确告知「已完成、结果已提供」，实际: %q", second)
	}

	// 只有从未创建过的 ID 才应报 "不存在"。
	missing := call("child_999")
	if !strings.Contains(missing, "不存在") {
		t.Fatalf("未知 ID 应报不存在，实际: %q", missing)
	}
}

// 运行中与已完成必须给出不同答复，否则模型无法判断该等还是该继续。
func TestChildResultRunningVsDone(t *testing.T) {
	a := New(AgentConfig{ID: "t"})

	a.childMu.Lock()
	a.childTasks["child_run"] = &childTaskState{running: true}
	a.childMu.Unlock()

	got := a.executeChildResultTool(agentAPI.ToolCall{
		Name:      "child_result",
		Arguments: map[string]interface{}{"task_id": "child_run"},
	})
	if !strings.Contains(got, "仍在运行中") {
		t.Fatalf("运行中的任务应提示仍在运行，实际: %q", got)
	}
}

// 保留的结果必须有界，不能随子任务数量无限增长。
func TestChildTaskRetentionBounded(t *testing.T) {
	a := New(AgentConfig{ID: "t"})

	a.childMu.Lock()
	for i := 0; i < maxRetainedChildTasks*3; i++ {
		a.childSeq++
		a.childTasks[fmt.Sprintf("child_%d", i)] = &childTaskState{result: "r", seq: a.childSeq}
	}
	a.evictChildTasksLocked()
	n := len(a.childTasks)
	a.childMu.Unlock()

	if n > maxRetainedChildTasks {
		t.Fatalf("保留子任务数=%d，超过上限 %d", n, maxRetainedChildTasks)
	}
	// 淘汰应保留最新的：最早的那批必须已不在
	if _, ok := a.childTasks["child_0"]; ok {
		t.Fatal("淘汰应优先丢弃最旧的已完成任务")
	}
}
